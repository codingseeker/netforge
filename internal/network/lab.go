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

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/chaos"
	"github.com/netforge/netforge/internal/cleanup"
	"github.com/netforge/netforge/internal/iface"
	"github.com/netforge/netforge/internal/namespace"
	"github.com/netforge/netforge/internal/probe"
	"github.com/netforge/netforge/internal/reconcile"
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
	// Chaos bounds what the fault engine may do, and Probes what a
	// measurement may cost. Zero values use the defaults of each package.
	Chaos  chaos.Limits
	Probes probe.Limits
	// Faults are applied once the laboratory is up, and are taken back out
	// when it is destroyed. A specification that the engine refuses fails the
	// whole laboratory rather than leaving part of the set in the kernel.
	Faults []chaos.Spec
	// Services are the endpoints laboratory namespaces serve, so a probe has
	// something real on the far end to measure. They are started after the
	// links exist and stopped before the namespaces disappear.
	Services []ServiceSpec
}

type Routing string

const (
	RoutingKernel   Routing = ""
	RoutingSoftware Routing = "software"
)

// String names the routing mode. The kernel mode is the empty value, because a
// laboratory that forwards with the kernel asks for nothing; a report that
// printed nothing in its place would read as a missing field rather than as the
// mode every laboratory used before the software router existed.
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
	// prober runs protocol probes and services are the endpoints they
	// measure. Both are owned by the laboratory.
	prober   *probe.Engine
	services []*serviceProcess
	// work is this laboratory's own state directory. Every laboratory gets one,
	// so two laboratories running at the same time cannot read each other's
	// service status.
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
	// declared are the faults the specification asked for, kept so a reader
	// can see which records came from the file rather than from a caller.
	declared []chaos.Fault
	// experiment is the identifier every fault of this laboratory is recorded
	// under.
	experiment string
}

func Create(ctx context.Context, spec Spec) (*Lab, error) {
	if err := checkSpec(spec); err != nil {
		return nil, err
	}
	plan, err := BuildPlan(spec.ID, spec.Network, spec.Clients, spec.Servers, spec.MTU)
	if err != nil {
		return nil, err
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
		// Every fault of this laboratory is recorded under the same
		// experiment, so the records of one laboratory can be told from
		// another even when two run at once.
		experiment: spec.ID,
	}
	stack, err := cleanup.NewWithTimeout(defaultTeardown)
	if err != nil {
		_ = lab.Destroy(ctx)
		return nil, err
	}
	lab.stack = stack
	// The state directory is registered first, so it is removed last: a
	// service has to be stopped before the file it reports into is gone. It is
	// created through the reconciler rather than with os.MkdirTemp so that it
	// carries an ownership marker: a run killed with SIGKILL cannot remove this
	// directory, and a directory nobody can prove is NetForge's is one nobody may
	// reclaim.
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
	// The services come up before the declared faults, so the endpoints a
	// probe is meant to measure exist before anything can impair the path to
	// them, and a probe never races a listener that is still binding.
	if err := lab.startServices(); err != nil {
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
	if spec.MaxNames > 0 {
		needed := spec.Clients + spec.Servers + 1
		if needed > spec.MaxNames {
			return apperr.LimitExceeded(op,
				"the laboratory needs "+strconv.Itoa(needed)+" namespaces but limits.max_namespaces is "+strconv.Itoa(spec.MaxNames), nil)
		}
	}
	if spec.MaxIfs > 0 {
		needed := 2 * (spec.Clients + spec.Servers)
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

	// The stack carries the laboratory's own teardown, and the holder carries
	// the namespaces, so both have to go. Running the stack on its own leaves
	// every namespace behind as soon as the stack holds anything else.
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
