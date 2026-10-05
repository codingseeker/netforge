package recovery

import (
	"context"
	"errors"
	"net/netip"
	"strconv"
	"time"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/loadbalancer"
	"github.com/netforge/netforge/internal/network"
	"github.com/netforge/netforge/internal/probe"
)

// Lab performs recovery against a running laboratory.
//
// It is the only place in the project where the seven allowed actions reach
// real machinery. The checks the recovery already did are not repeated here
// beyond what a laboratory has to check for itself: a laboratory knows whether
// a namespace is one of its own, and an action aimed at something that is not
// belongs to nobody.
type Lab struct {
	// lab is the laboratory the actions act on.
	lab *network.Lab
	// backends changes which of a load balancer's backends are in service. It
	// is optional, because an experiment without a balancer has nothing to
	// change there, and an action that needs it is refused rather than ignored.
	backends BackendController
	// settle is how long a service is given to answer after it is started
	// again, before the measurement decides whether it did.
	settle time.Duration
}

// BackendController is the part of a load balancer a recovery needs: a way to
// change the service state of a backend that the balancer already registered.
//
// It is an interface rather than a balancer so this package does not depend on
// how the balancer is reached. A laboratory in one process hands over the
// balancer it started; a laboratory on another host hands over a client that
// writes the balancer's control document.
type BackendController interface {
	// Remove takes a backend out of service.
	Remove(ctx context.Context, address string) error
	// Restore puts a removed backend back to being checked.
	Restore(ctx context.Context, address string) error
	// Drain stops new clients being placed on a backend and lets the
	// connections it holds finish.
	Drain(ctx context.Context, address string) error
	// ReEnable cancels a drain.
	ReEnable(ctx context.Context, address string) error
}

// NewLab binds a recovery to a laboratory.
func NewLab(lab *network.Lab, backends BackendController, settle time.Duration) (*Lab, error) {
	const op = "recovery.NewLab"
	if lab == nil {
		return nil, apperr.InvalidArgument(op, "a recovery needs a laboratory to act on", nil)
	}
	if settle <= 0 {
		settle = 500 * time.Millisecond
	}
	return &Lab{lab: lab, backends: backends, settle: settle}, nil
}

// Measure verifies a path. It is the Prober the recovery uses, so the
// measurement behind every verdict comes from the laboratory itself.
func (l *Lab) Measure(ctx context.Context, namespace, target string, options probe.Options) (probe.Measurement, error) {
	const op = "recovery.Lab.Measure"
	if err := l.checkNamespace(op, namespace); err != nil {
		return probe.Measurement{}, err
	}
	return l.lab.Measure(ctx, namespace, target, options)
}

// RemoveFault takes the faults on a namespace back out of the kernel.
//
// A fault that is not there any more is not a failure. Removing a fault twice,
// or removing one that expired on its own, is the ordinary result of a
// laboratory that was interrupted, and the second call has nothing to do and
// reports no error.
func (l *Lab) RemoveFault(ctx context.Context, namespace string) error {
	const op = "recovery.Lab.RemoveFault"
	if err := l.checkNamespace(op, namespace); err != nil {
		return err
	}
	engine := l.lab.Chaos()
	if engine == nil {
		return apperr.Unsupported(op, "this laboratory has no fault engine", nil)
	}
	var removed []string
	for _, fault := range engine.List() {
		if fault.Target.Namespace == namespace {
			removed = append(removed, fault.ID)
		}
	}
	var failure error
	for _, id := range removed {
		if err := ctx.Err(); err != nil {
			return apperr.FromContext(ctx, op)
		}
		if _, err := engine.Remove(id); err != nil {
			failure = joinErrors(failure, err)
		}
	}
	return failure
}

// RestoreRoute puts a route back the way the laboratory installed it.
//
// The laboratory owns one route per node: the default route through that
// node's gateway. Restoring that is what a recovery can be sure of restoring,
// because it is the only route this laboratory ever created. A route to some
// other destination is a route the operator added, and guessing at it would be
// a repair that changes something nobody described.
func (l *Lab) RestoreRoute(ctx context.Context, namespace, target string) error {
	const op = "recovery.Lab.RestoreRoute"
	if err := l.checkNamespace(op, namespace); err != nil {
		return err
	}
	prefix, err := routePrefix(target)
	if err != nil {
		return apperr.Validation(op, "target "+strconv.Quote(target)+" is not a prefix or address", err)
	}
	plan := l.lab.Plan()
	for _, node := range plan.Nodes {
		if node.Namespace != namespace {
			continue
		}
		// The route that is being restored is the network the laboratory is
		// built on, through the node's own gateway. A destination the scope
		// refused never reaches here, so this is the only route a recovery can
		// be authorized to restore.
		if prefix.Contains(plan.Network.Addr()) {
			if err := l.lab.AddRoute(namespace, plan.Network, node.Gateway); err != nil {
				return apperr.Wrap(err, kindOf(err), op, "the route through "+node.Gateway.String()+" could not be restored")
			}
			return nil
		}
		return apperr.Unsupported(op, "the route to "+prefix.String()+" is not the route this laboratory created, "+
			"which is the route to "+plan.Network.String()+", so it will not guess at the gateway to restore it through", nil)
	}
	return apperr.NotFound(op, "no node of this laboratory runs in namespace "+strconv.Quote(namespace), nil)
}

// routePrefix reads a route destination: a prefix, or a bare address which is the
// host route to that one address. The scope has already decided that the
// destination is inside the experiment network; this is only about spelling.
func routePrefix(target string) (netip.Prefix, error) {
	prefix, err := netip.ParsePrefix(target)
	if err == nil {
		return prefix, nil
	}
	addr, addrErr := netip.ParseAddr(target)
	if addrErr != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(addr.Unmap(), addr.BitLen()), nil
}

// RestartService restarts a laboratory service and waits for it to answer.
func (l *Lab) RestartService(ctx context.Context, namespace string) error {
	const op = "recovery.Lab.RestartService"
	if err := l.checkNamespace(op, namespace); err != nil {
		return err
	}
	if err := l.lab.RestartService(ctx, namespace); err != nil {
		return apperr.Wrap(err, kindOf(err), op, "the service in "+namespace+" could not be restarted")
	}
	return l.settleFor(ctx, namespace)
}

// RemoveBackend takes a backend out of service.
func (l *Lab) RemoveBackend(ctx context.Context, namespace, address string) error {
	return l.backend(ctx, ActionRemoveBackend, namespace, address)
}

// RestoreBackend puts a removed backend back to being checked.
func (l *Lab) RestoreBackend(ctx context.Context, namespace, address string) error {
	return l.backend(ctx, ActionRestoreBackend, namespace, address)
}

// DrainConnection stops new clients being placed on a backend.
func (l *Lab) DrainConnection(ctx context.Context, namespace, address string) error {
	return l.backend(ctx, ActionDrainConnection, namespace, address)
}

// ReEnableBackend cancels a drain.
func (l *Lab) ReEnableBackend(ctx context.Context, namespace, address string) error {
	return l.backend(ctx, ActionReEnableBackend, namespace, address)
}

// backend routes the four backend actions to the controller. An experiment
// without a balancer refuses them rather than reporting a success it did not
// achieve.
func (l *Lab) backend(ctx context.Context, action Action, namespace, address string) error {
	const op = "recovery.Lab.backend"
	if l.backends == nil {
		return apperr.Unsupported(op, "this experiment has no load balancer, so "+string(action)+" has nothing to act on", nil)
	}
	// The namespace is checked even though the balancer does not act inside it:
	// the measurement that follows runs there, so a namespace outside the
	// laboratory is a namespace the recovery is not allowed to measure from.
	if err := l.checkNamespace(op, namespace); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return apperr.FromContext(ctx, op)
	}
	var err error
	switch action {
	case ActionRemoveBackend:
		err = l.backends.Remove(ctx, address)
	case ActionRestoreBackend:
		err = l.backends.Restore(ctx, address)
	case ActionDrainConnection:
		err = l.backends.Drain(ctx, address)
	case ActionReEnableBackend:
		err = l.backends.ReEnable(ctx, address)
	default:
		return apperr.Unsupported(op, strconv.Quote(string(action))+" is not a backend action", nil)
	}
	if err != nil {
		return apperr.Wrap(err, kindOf(err), op, string(action)+" on "+address+" was refused")
	}
	// The balancer applies a control command on its next health tick, so a
	// measurement taken immediately would be a measurement of the state before
	// the command. The wait is the settle time rather than a guess at a poll
	// interval, and the measurement that judges the repair is what proves it
	// landed.
	return l.settleFor(ctx, namespace)
}

// settleFor waits for a laboratory to be quiet enough to measure. A service
// that was just restarted may not have bound its listener yet, and a path
// measured in that moment would blame the recovery for the restart.
func (l *Lab) settleFor(ctx context.Context, namespace string) error {
	const op = "recovery.Lab.settle"
	if err := ctx.Err(); err != nil {
		return apperr.FromContext(ctx, op)
	}
	timer := time.NewTimer(l.settle)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return apperr.FromContext(ctx, op)
	case <-timer.C:
		return nil
	}
}

// checkNamespace refuses a namespace this laboratory did not create. Every
// action passes through here, so a recovery cannot reach outside the laboratory
// even if it were handed a target from somewhere else.
func (l *Lab) checkNamespace(op, namespace string) error {
	if namespace == "" {
		return apperr.Validation(op, "a namespace is required", nil)
	}
	for _, name := range l.lab.Names() {
		if name == namespace {
			return nil
		}
	}
	return apperr.Permission(op, "namespace "+strconv.Quote(namespace)+" is not part of this laboratory", nil)
}

// joinErrors keeps going after a failure that does not stop the recovery. One
// fault that would not come out is worth reporting alongside the others rather
// than instead of them, because the next step may still have to try.
func joinErrors(first, second error) error {
	return errors.Join(first, second)
}

// ControlFile is a BackendController that drives a balancer in another process
// through its control document.
//
// The balancer reads the document, not this code, so the boundary is the same
// one the balancer validates: a sequence that has been applied is not applied
// again, and a backend that was not registered is refused there as well as
// here.
type ControlFile struct {
	// Path is the balancer's control document.
	Path string
	// Experiment is the experiment the balancer serves, which the document has
	// to name.
	Experiment string
	// seq is this controller's next sequence number.
	seq uint64
}

// NewControlFile binds a controller to a balancer's control document.
func NewControlFile(path, experiment string) (*ControlFile, error) {
	const op = "recovery.NewControlFile"
	if path == "" {
		return nil, apperr.InvalidArgument(op, "a control path is required", nil)
	}
	if experiment == "" {
		return nil, apperr.InvalidArgument(op, "an experiment is required, so a document for another run is refused", nil)
	}
	return &ControlFile{Path: path, Experiment: experiment}, nil
}

func (c *ControlFile) command(ctx context.Context, operation loadbalancer.Operation, address string) error {
	const op = "recovery.ControlFile"
	if err := ctx.Err(); err != nil {
		return apperr.FromContext(ctx, op)
	}
	// The target is checked here as well as by the balancer. Writing a document
	// is the last moment a nonsense target can be caught without waiting for a
	// health tick to refuse it, and this controller is the thing that composed
	// the request, so it is the thing that should notice it was malformed.
	endpoint, err := netip.ParseAddrPort(address)
	if err != nil {
		return apperr.Validation(op, "target "+strconv.Quote(address)+" is not the endpoint of a backend", err)
	}
	if endpoint.Addr().Zone() != "" {
		return apperr.Unsupported(op, "target "+strconv.Quote(address)+" carries a zone, which a laboratory does not route", nil)
	}
	c.seq++
	document := loadbalancer.Control{
		Experiment: c.Experiment,
		Commands:   []loadbalancer.Command{{Seq: c.seq, Operation: operation, Backend: endpoint.String()}},
	}
	if err := loadbalancer.WriteControl(c.Path, document); err != nil {
		return apperr.Wrap(err, kindOf(err), op, "the balancer could not be asked to "+string(operation)+" "+address)
	}
	return nil
}

func (c *ControlFile) Remove(ctx context.Context, address string) error {
	return c.command(ctx, loadbalancer.OpRemoveBackend, address)
}

func (c *ControlFile) Restore(ctx context.Context, address string) error {
	return c.command(ctx, loadbalancer.OpRestoreBackend, address)
}

func (c *ControlFile) Drain(ctx context.Context, address string) error {
	return c.command(ctx, loadbalancer.OpDrainConnection, address)
}

func (c *ControlFile) ReEnable(ctx context.Context, address string) error {
	return c.command(ctx, loadbalancer.OpReEnableBackend, address)
}
