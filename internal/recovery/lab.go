package recovery

import (
	"context"
	"errors"
	"net/netip"
	"strconv"
	"time"

	"github.com/codingseeker/netforge/internal/apperr"
	"github.com/codingseeker/netforge/internal/loadbalancer"
	"github.com/codingseeker/netforge/internal/network"
	"github.com/codingseeker/netforge/internal/probe"
)

type Lab struct {
	lab *network.Lab

	backends BackendController

	settle time.Duration
}

type BackendController interface {
	Remove(ctx context.Context, address string) error

	Restore(ctx context.Context, address string) error

	Drain(ctx context.Context, address string) error

	ReEnable(ctx context.Context, address string) error
}

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

func (l *Lab) Measure(ctx context.Context, namespace, target string, options probe.Options) (probe.Measurement, error) {
	const op = "recovery.Lab.Measure"
	if err := l.checkNamespace(op, namespace); err != nil {
		return probe.Measurement{}, err
	}
	return l.lab.Measure(ctx, namespace, target, options)
}

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

func (l *Lab) RemoveBackend(ctx context.Context, namespace, address string) error {
	return l.backend(ctx, ActionRemoveBackend, namespace, address)
}

func (l *Lab) RestoreBackend(ctx context.Context, namespace, address string) error {
	return l.backend(ctx, ActionRestoreBackend, namespace, address)
}

func (l *Lab) DrainConnection(ctx context.Context, namespace, address string) error {
	return l.backend(ctx, ActionDrainConnection, namespace, address)
}

func (l *Lab) ReEnableBackend(ctx context.Context, namespace, address string) error {
	return l.backend(ctx, ActionReEnableBackend, namespace, address)
}

func (l *Lab) backend(ctx context.Context, action Action, namespace, address string) error {
	const op = "recovery.Lab.backend"
	if l.backends == nil {
		return apperr.Unsupported(op, "this experiment has no load balancer, so "+string(action)+" has nothing to act on", nil)
	}

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

	return l.settleFor(ctx, namespace)
}

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

func joinErrors(first, second error) error {
	return errors.Join(first, second)
}

type ControlFile struct {
	Path string

	Experiment string

	seq uint64
}

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
