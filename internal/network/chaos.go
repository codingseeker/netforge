package network

import (
	"context"
	"fmt"
	"os"

	"github.com/codingseeker/netforge/internal/apperr"
	"github.com/codingseeker/netforge/internal/chaos"
	"github.com/codingseeker/netforge/internal/probe"
)

func (l *Lab) Chaos() *chaos.Engine { return l.faults }

func (l *Lab) Measurer() *probe.Measurer { return l.measurer }

func (l *Lab) Measure(ctx context.Context, namespace, target string, options probe.Options) (probe.Measurement, error) {
	const op = "network.Measure"
	if l.measurer == nil {
		return probe.Measurement{}, apperr.Internal(op, "the laboratory has no measurer", nil)
	}
	return l.measurer.Measure(ctx, namespace, target, options)
}

func (l *Lab) startFaults(ctx context.Context) error {
	const op = "network.startFaults"
	measurer, err := probe.New(l.spec.Probes, l)
	if err != nil {
		return err
	}
	engine, err := chaos.New(ctx, l.scope(), l.spec.Chaos, l, l)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	prober, err := probe.NewEngine(l.spec.Probes, l.probeScope(), l, measurer, self)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	l.faults = engine
	l.measurer = measurer
	l.prober = prober
	if err := l.stack.Defer("chaos", func(context.Context) error { return engine.Close() }); err != nil {
		return err
	}

	if len(l.spec.Faults) == 0 {
		return nil
	}
	applied, err := engine.ApplyAll(ctx, l.experiment, l.spec.Faults)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	l.declared = applied
	return nil
}

func (l *Lab) scope() chaos.Scope {
	names := make([]string, 0, len(l.plan.Nodes)+1)
	names = append(names, l.plan.RouterNS)
	for _, node := range l.plan.Nodes {
		names = append(names, node.Namespace)
	}
	return chaos.Scope{Network: l.plan.Network, Namespaces: names}
}

func (l *Lab) probeScope() probe.Scope {
	names := make([]string, 0, len(l.plan.Nodes)+1)
	names = append(names, l.plan.RouterNS)
	for _, node := range l.plan.Nodes {
		names = append(names, node.Namespace)
	}
	return probe.Scope{Network: l.plan.Network, Namespaces: names}
}

func (l *Lab) Run(namespace string, argv ...string) (string, string, int, error) {
	space, err := l.Space(namespace)
	if err != nil {
		return "", "", 0, err
	}
	result, err := space.Run(argv...)
	return result.Stdout, result.Stderr, result.Code, err
}

func (l *Lab) Devices(namespace string) ([]string, error) {
	const op = "network.Devices"
	if _, err := l.Space(namespace); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	var devices []string
	for _, node := range l.plan.Nodes {
		switch namespace {
		case node.Namespace:
			devices = append(devices, node.Interface)
		case l.plan.RouterNS:
			devices = append(devices, node.RouterIf)
		}
	}
	return devices, nil
}

func (l *Lab) Peer(namespace, device string) (chaos.Endpoint, bool) {
	for _, node := range l.plan.Nodes {
		switch {
		case namespace == node.Namespace && device == node.Interface:
			return chaos.Endpoint{Namespace: l.plan.RouterNS, Interface: node.RouterIf}, true
		case namespace == l.plan.RouterNS && device == node.RouterIf:
			return chaos.Endpoint{Namespace: node.Namespace, Interface: node.Interface}, true
		}
	}
	return chaos.Endpoint{}, false
}

func (l *Lab) DeclaredFaults() []chaos.Fault {
	if l.faults == nil {
		return nil
	}
	return append([]chaos.Fault(nil), l.declared...)
}
