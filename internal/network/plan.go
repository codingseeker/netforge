package network

import (
	"fmt"
	"net/netip"
	"strconv"

	"github.com/codingseeker/netforge/internal/iface"
)

const blockStride = uint32(4)

type NodeRole string

const (
	RoleClient   NodeRole = "client"
	RoleServer   NodeRole = "server"
	RoleBalancer NodeRole = "balancer"
)

type Node struct {
	Name      string
	Role      NodeRole
	Namespace string
	Interface string
	RouterIf  string
	Address   netip.Prefix
	Router    netip.Prefix
	Gateway   netip.Addr
	Index     int
}

type Link struct {
	Name     string
	Node     Node
	Network  netip.Prefix
	RouterIf string
	RouterIP netip.Addr
}

type Plan struct {
	Network  netip.Prefix
	IDName   string
	Nodes    []Node
	Links    []Link
	MTU      int
	RouterNS string
	BlockLen int
}

func BuildPlan(id, network string, clients, servers, mtu int) (Plan, error) {
	const op = "network.BuildPlan"
	prefix, err := netip.ParsePrefix(network)
	if err != nil {
		return Plan{}, fmt.Errorf("%s: %q is not a prefix: %w", op, network, err)
	}
	if err := iface.ValidatePrefix(op, "network", prefix); err != nil {
		return Plan{}, err
	}
	if clients < 1 || servers < 1 {
		return Plan{}, fmt.Errorf("%s: an experiment needs at least one client and one server, got %d and %d",
			op, clients, servers)
	}
	if clients > 8 || servers > 8 {
		return Plan{}, fmt.Errorf("%s: at most 8 clients and 8 servers are supported, got %d and %d",
			op, clients, servers)
	}
	if mtu == 0 {
		mtu = iface.DefaultMTU
	}
	if err := iface.ValidateMTU(op, mtu); err != nil {
		return Plan{}, err
	}

	hostBits := prefix.Addr().BitLen() - prefix.Bits()
	if hostBits < 8 {
		return Plan{}, fmt.Errorf("%s: %s leaves %d addresses, too few for point to point links",
			op, prefix, 1<<hostBits)
	}
	blockLen := prefix.Bits() + 6
	blocks := 1 << (hostBits - 2)
	stride := blockStride
	if clients+servers > blocks {
		return Plan{}, fmt.Errorf("%s: %s holds %d point to point links, %d were asked for",
			op, prefix, blocks, clients+servers)
	}

	base := prefix.Addr().As4()
	origin := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	at := func(offset uint32) netip.Addr {
		value := origin + offset
		return netip.AddrFrom4([4]byte{byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value)})
	}

	plan := Plan{
		Network:  prefix,
		IDName:   id,
		MTU:      mtu,
		RouterNS: NamespaceName("router", id),
		BlockLen: blockLen,
	}
	next := uint32(0)
	add := func(role NodeRole, count int) {
		for i := 0; i < count; i++ {
			block := netip.PrefixFrom(at(next), blockLen)
			router := netip.PrefixFrom(at(next+1), blockLen)
			node := netip.PrefixFrom(at(next+2), blockLen)
			kind := "cl"
			if role == RoleServer {
				kind = "sv"
			}
			entry := Node{
				Name:      string(role) + strconv.Itoa(i),
				Role:      role,
				Namespace: NamespaceName(string(role)+strconv.Itoa(i), id),
				Interface: iface.SideName(kind, i),
				RouterIf:  iface.RouterName(kind, i),
				Address:   node,
				Router:    router,
				Gateway:   router.Addr(),
				Index:     i,
			}
			plan.Nodes = append(plan.Nodes, entry)
			plan.Links = append(plan.Links, Link{
				Name:     "link-" + string(role) + strconv.Itoa(i),
				Node:     entry,
				Network:  block,
				RouterIf: entry.RouterIf,
				RouterIP: router.Addr(),
			})
			next += stride
		}
	}
	add(RoleClient, clients)
	add(RoleServer, servers)
	return plan, nil
}

func NamespaceName(role, id string) string {
	name := "nf-" + role
	if id != "" {
		name += "-" + id
	}
	return name
}

func (p Plan) Clients() []Node { return p.byRole(RoleClient) }
func (p Plan) Servers() []Node { return p.byRole(RoleServer) }

func (p Plan) byRole(role NodeRole) []Node {
	var found []Node
	for _, node := range p.Nodes {
		if node.Role == role {
			found = append(found, node)
		}
	}
	return found
}

func (p Plan) ServerBlocks() []netip.Prefix {
	var found []netip.Prefix
	for _, link := range p.Links {
		if link.Node.Role == RoleServer {
			found = append(found, link.Network)
		}
	}
	return found
}

func (p Plan) Contains(address netip.Addr) bool { return p.Network.Contains(address) }

func (p Plan) Balancers() []Node { return p.byRole(RoleBalancer) }

func (p Plan) HasBalancer() bool { return len(p.Balancers()) > 0 }

func (p Plan) Balancer() (Node, bool) {
	found := p.Balancers()
	if len(found) == 0 {
		return Node{}, false
	}
	return found[0], true
}

func AddBalancer(plan Plan) (Plan, error) {
	const op = "network.AddBalancer"
	if plan.HasBalancer() {
		return plan, nil
	}
	if !plan.Network.IsValid() {
		return plan, fmt.Errorf("%s: the plan has no network to take a block from", op)
	}
	next := uint32(len(plan.Nodes)) * blockStride
	blocks := 1 << (plan.Network.Addr().BitLen() - plan.Network.Bits() - 2)
	if uint32(len(plan.Nodes)) >= uint32(blocks) {
		return plan, fmt.Errorf("%s: %s has no block left for a balancer; it holds %d point to point links",
			op, plan.Network, blocks)
	}
	base := plan.Network.Addr().As4()
	origin := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	at := func(offset uint32) netip.Addr {
		value := origin + offset
		return netip.AddrFrom4([4]byte{byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value)})
	}
	blockLen := plan.Network.Bits() + 6
	block := netip.PrefixFrom(at(next), blockLen)
	router := netip.PrefixFrom(at(next+1), blockLen)
	node := netip.PrefixFrom(at(next+2), blockLen)
	entry := Node{
		Name:      "balancer0",
		Role:      RoleBalancer,
		Namespace: NamespaceName("balancer0", plan.ID()),
		Interface: iface.SideName("lb", 0),
		RouterIf:  iface.RouterName("lb", 0),
		Address:   node,
		Router:    router,
		Gateway:   router.Addr(),
		Index:     0,
	}
	plan.Nodes = append(plan.Nodes, entry)
	plan.Links = append(plan.Links, Link{
		Name:     "link-balancer0",
		Node:     entry,
		Network:  block,
		RouterIf: entry.RouterIf,
		RouterIP: router.Addr(),
	})
	return plan, nil
}

func (p Plan) ID() string { return p.IDName }
