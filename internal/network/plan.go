package network

import (
	"fmt"
	"net/netip"
	"strconv"

	"github.com/netforge/netforge/internal/iface"
)

type NodeRole string

const (
	RoleClient NodeRole = "client"
	RoleServer NodeRole = "server"
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
	stride := uint32(4)
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
