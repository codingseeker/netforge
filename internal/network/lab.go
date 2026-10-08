package network

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/codingseeker/netforge/internal/apperr"
	"github.com/codingseeker/netforge/internal/chaos"
	"github.com/codingseeker/netforge/internal/cleanup"
	"github.com/codingseeker/netforge/internal/iface"
	"github.com/codingseeker/netforge/internal/loadbalancer"
	"github.com/codingseeker/netforge/internal/namespace"
	"github.com/codingseeker/netforge/internal/probe"
	"github.com/codingseeker/netforge/internal/reconcile"
)

type Spec struct {
	ID       string
	Network  string
	Clients  int
	Servers  int
	MTU      int
	Names    []string
	MaxNames int
	MaxIfs   int
	Routing  Routing
	Router   RouterLimits

	Chaos  chaos.Limits
	Probes probe.Limits

	Faults []chaos.Spec

	Services []ServiceSpec
	Balancer *BalancerSpec
}

type BalancerSpec struct {
	Namespace string
	Listen    string
	Backends  []string
	Policy    loadbalancer.Policy
	Limits    loadbalancer.Limits
}

type Routing string

const (
	RoutingKernel   Routing = ""
	RoutingSoftware Routing = "software"
)

func (r Routing) String() string {
	if r == RoutingKernel {
		return "kernel"
	}
	return string(r)
}

type RouterLimits struct {
	MaxRoutes    int
	MaxNeighbors int
	QueueSize    int
}

type Space struct {
	Node   Node
	Handle *namespace.Space
}

type Lab struct {
	spec   Spec
	plan   Plan
	holder *namespace.Lab

	prober   *probe.Engine
	services []*serviceProcess

	work     string
	net      *iface.Controller
	stack    *cleanup.Stack
	spaces   map[string]*Space
	mu       sync.Mutex
	closed   bool
	dir      string
	router   *routerProcess
	faults   *chaos.Engine
	measurer *probe.Measurer

	declared []chaos.Fault

	experiment string
	balancer   *balancerProcess
}

func Create(ctx context.Context, spec Spec) (*Lab, error) {
	if err := checkSpec(spec); err != nil {
		return nil, err
	}
	plan, err := BuildPlan(spec.ID, spec.Network, spec.Clients, spec.Servers, spec.MTU)
	if err != nil {
		return nil, err
	}
	if spec.Balancer != nil {
		plan, err = AddBalancer(plan)
		if err != nil {
			return nil, err
		}
	}
	if err := enforceScope(spec, plan); err != nil {
		return nil, err
	}
	holder, err := namespace.NewLab()
	if err != nil {
		return nil, fmt.Errorf("network.Create: %w", err)
	}
	lab := &Lab{
		spec:   spec,
		plan:   plan,
		holder: holder,
		net:    iface.New(holder),
		stack:  cleanup.New(),
		spaces: map[string]*Space{},

		experiment: spec.ID,
	}
	stack, err := cleanup.NewWithTimeout(defaultTeardown)
	if err != nil {
		_ = lab.Destroy(ctx)
		return nil, err
	}
	lab.stack = stack

	work, _, err := reconcile.Create(reconcile.StateRoot(), reconcile.KindLaboratory, spec.ID, time.Now())
	if err != nil {
		_ = lab.Destroy(ctx)
		return nil, fmt.Errorf("network.Create: %w", err)
	}
	lab.work = work
	if err := lab.stack.Defer("work", func(context.Context) error {
		_ = os.RemoveAll(lab.work)
		lab.work = ""
		return nil
	}); err != nil {
		_ = lab.Destroy(ctx)
		return nil, err
	}
	if err := lab.stack.Defer("laboratory", func(context.Context) error { return lab.Destroy(context.Background()) }); err != nil {
		_ = lab.Destroy(ctx)
		return nil, err
	}
	if err := lab.build(ctx); err != nil {
		_ = lab.stack.Run(ctx)
		return nil, err
	}
	if spec.Routing == RoutingSoftware {
		if err := lab.startRouter(); err != nil {
			_ = lab.stack.Run(ctx)
			return nil, err
		}
	}

	if err := lab.startServices(); err != nil {
		_ = lab.stack.Run(ctx)
		return nil, err
	}
	if err := lab.startBalancer(); err != nil {
		_ = lab.stack.Run(ctx)
		return nil, err
	}
	if err := lab.startFaults(ctx); err != nil {
		_ = lab.stack.Run(ctx)
		return nil, err
	}
	return lab, nil
}

const defaultTeardown = 5 * time.Second

func checkSpec(spec Spec) error {
	const op = "network.Create"
	if spec.Clients < 1 || spec.Servers < 1 {
		return apperr.InvalidArgument(op, "a laboratory needs at least one client and one server", nil)
	}
	if spec.Routing != RoutingKernel && spec.Routing != RoutingSoftware {
		return apperr.InvalidArgument(op,
			"routing "+string(spec.Routing)+" is unknown, want "+string(RoutingSoftware)+" or kernel forwarding", nil)
	}
	if spec.Balancer != nil {
		if spec.Balancer.Namespace == "" {
			return apperr.InvalidArgument(op, "a balancer needs a namespace to run in", nil)
		}
		if spec.Balancer.Listen == "" {
			return apperr.InvalidArgument(op, "a balancer needs an address to listen on", nil)
		}
		if len(spec.Balancer.Backends) == 0 {
			return apperr.InvalidArgument(op, "a balancer needs at least one backend to distribute to", nil)
		}
	}
	if spec.MaxNames > 0 {
		needed := spec.Clients + spec.Servers + 1
		if spec.Balancer != nil {
			needed++
		}
		if needed > spec.MaxNames {
			return apperr.LimitExceeded(op,
				"the laboratory needs "+strconv.Itoa(needed)+" namespaces but limits.max_namespaces is "+strconv.Itoa(spec.MaxNames), nil)
		}
	}
	if spec.MaxIfs > 0 {
		needed := 2 * (spec.Clients + spec.Servers)
		if spec.Balancer != nil {
			needed += 2
		}
		if needed > spec.MaxIfs {
			return apperr.LimitExceeded(op,
				"the laboratory needs "+strconv.Itoa(needed)+" interfaces but limits.max_interfaces is "+strconv.Itoa(spec.MaxIfs), nil)
		}
	}
	return nil
}

func enforceScope(spec Spec, plan Plan) error {
	const op = "network.Create"
	allowed := map[string]struct{}{}
	for _, name := range spec.Names {
		allowed[name] = struct{}{}
	}
	check := func(name string) error {
		if _, ok := allowed[name]; !ok {
			return apperr.InvalidArgument(op,
				"the laboratory needs namespace "+name+", which scope.namespaces does not grant", nil)
		}
		return nil
	}
	if err := check(plan.RouterNS); err != nil {
		return err
	}
	for _, node := range plan.Nodes {
		if err := check(node.Namespace); err != nil {
			return err
		}
	}
	for _, node := range plan.Nodes {
		if !plan.Network.Contains(node.Address.Addr()) {
			return apperr.InvalidArgument(op,
				"the plan puts "+node.Address.Addr().String()+" outside scope.network "+plan.Network.String(), nil)
		}
	}
	return nil
}

func (l *Lab) build(ctx context.Context) error {
	const op = "network.Create"
	if err := l.net.SetLoopback(op, l.holder.Root()); err != nil {
		return err
	}
	router, err := l.holder.Spawn(l.plan.RouterNS)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	l.spaces[l.plan.RouterNS] = &Space{Node: Node{Name: "router", Namespace: l.plan.RouterNS}, Handle: router}
	if err := l.net.SetLoopback(op, router); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if l.spec.Routing != RoutingSoftware {
		if err := l.net.EnableForwarding(op, router); err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
	}
	for _, node := range l.plan.Nodes {
		if err := l.spawnNode(ctx, node); err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
	}
	return nil
}

func (l *Lab) spawnNode(ctx context.Context, node Node) error {
	const op = "network.Create"
	space, err := l.holder.Spawn(node.Namespace)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	l.spaces[node.Namespace] = &Space{Node: node, Handle: space}
	if err := l.net.SetLoopback(op, space); err != nil {
		return err
	}
	link, ok := linkFor(l.plan, node)
	if !ok {
		return fmt.Errorf("%s: the plan has no link for %s", op, node.Name)
	}
	if err := l.net.CreatePair(op, node.RouterIf, node.Interface); err != nil {
		return err
	}
	if err := l.net.Move(op, node.Interface, space); err != nil {
		return err
	}
	router := l.spaces[l.plan.RouterNS]
	if router == nil {
		return fmt.Errorf("%s: the router is missing", op)
	}
	routerIface := link.RouterIf
	if err := l.net.Move(op, node.RouterIf, router.Handle); err != nil {
		return err
	}
	if err := l.net.AddAddress(op, router.Handle, routerIface, link.Node.Router); err != nil {
		return err
	}
	if err := l.net.SetMTU(op, router.Handle, routerIface, l.plan.MTU); err != nil {
		return err
	}
	if err := l.net.SetUp(op, router.Handle, routerIface); err != nil {
		return err
	}
	if err := l.net.AddAddress(op, space, node.Interface, node.Address); err != nil {
		return err
	}
	if err := l.net.SetMTU(op, space, node.Interface, l.plan.MTU); err != nil {
		return err
	}
	if err := l.net.SetUp(op, space, node.Interface); err != nil {
		return err
	}
	if err := l.net.AddDefaultRoute(op, space, node.Gateway, node.Interface); err != nil {
		return err
	}
	return nil
}

func linkFor(plan Plan, node Node) (Link, bool) {
	for _, link := range plan.Links {
		if link.Node.Name == node.Name {
			return link, true
		}
	}
	return Link{}, false
}

func (l *Lab) Plan() Plan { return l.plan }

func (l *Lab) Names() []string {
	var names []string
	for _, node := range l.plan.Nodes {
		names = append(names, node.Namespace)
	}
	names = append(names, l.plan.RouterNS)
	live := names[:0]
	for _, name := range names {
		if space, err := l.holder.Lookup(name); err == nil && space.Alive() {
			live = append(live, name)
		}
	}
	return live
}

func (l *Lab) Lookup(name string) (*namespace.Space, error) {
	space, err := l.holder.Lookup(name)
	if err != nil {
		return nil, err
	}
	return space, nil
}

func (l *Lab) Space(node string) (*namespace.Space, error) {
	l.mu.Lock()
	space, ok := l.spaces[node]
	l.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", namespace.ErrNotFound, node)
	}
	return space.Handle, nil
}

func (l *Lab) Inspect(node string) (iface.State, error) {
	space, err := l.Space(node)
	if err != nil {
		return iface.State{}, err
	}
	return l.net.Inspect("network.Inspect", space)
}

func (l *Lab) Forwarding() (bool, error) {
	space, err := l.Space(l.plan.RouterNS)
	if err != nil {
		return false, err
	}
	return l.net.Forwarding("network.Forwarding", space)
}

func (l *Lab) Client(index int) (*namespace.Space, Node, error) {
	return l.byIndex(RoleClient, index)
}

func (l *Lab) Server(index int) (*namespace.Space, Node, error) {
	return l.byIndex(RoleServer, index)
}

func (l *Lab) byIndex(role NodeRole, index int) (*namespace.Space, Node, error) {
	clients := l.plan.Clients()
	servers := l.plan.Servers()
	var nodes []Node
	switch role {
	case RoleClient:
		nodes = clients
	case RoleServer:
		nodes = servers
	default:
		return nil, Node{}, fmt.Errorf("network: unknown role %q", role)
	}
	if index < 0 || index >= len(nodes) {
		return nil, Node{}, fmt.Errorf("network: %s %d does not exist, the plan has %d", role, index, len(nodes))
	}
	node := nodes[index]
	space, err := l.Space(node.Namespace)
	if err != nil {
		return nil, Node{}, err
	}
	return space, node, nil
}

func (l *Lab) Destroy(ctx context.Context) error {
	l.stopRouter()
	defer l.removeRouterDir()
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	l.mu.Unlock()

	var failure error
	if l.stack.Len() > 1 {
		failure = l.stack.Run(ctx)
	}
	if err := l.holder.Close(); err != nil {
		failure = errors.Join(failure, err)
	}
	return failure
}

func (l *Lab) AddRoute(node string, target netip.Prefix, via netip.Addr) error {
	space, err := l.Space(node)
	if err != nil {
		return err
	}
	return l.net.AddRoute("network.AddRoute", space, target, via)
}

func (l *Lab) RemoveRoute(node string, target netip.Prefix) error {
	space, err := l.Space(node)
	if err != nil {
		return err
	}
	return l.net.RemoveRoute("network.RemoveRoute", space, target)
}

func (l *Lab) Send(ctx context.Context, node string, argv ...string) (string, string, error) {
	space, err := l.Space(node)
	if err != nil {
		return "", "", err
	}
	stdout, stderr, err := space.Exec(argv...)
	if err != nil {
		return stdout, stderr, fmt.Errorf("network.Send: %w", err)
	}
	return stdout, stderr, nil
}
