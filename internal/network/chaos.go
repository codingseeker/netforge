package network

import (
	"context"
	"fmt"
	"os"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/chaos"
	"github.com/netforge/netforge/internal/probe"
)

// Chaos returns the fault engine of the laboratory, or nil when the laboratory
// was not created with one. The engine is owned by the laboratory: destroying
// the laboratory takes every fault back out of the kernel.
func (l *Lab) Chaos() *chaos.Engine { return l.faults }

// Measurer returns the measurer of the laboratory, or nil when the laboratory
// was not created with one.
func (l *Lab) Measurer() *probe.Measurer { return l.measurer }

// Measure sends echo requests from inside a laboratory namespace and reports
// what came back from the wire.
func (l *Lab) Measure(ctx context.Context, namespace, target string, options probe.Options) (probe.Measurement, error) {
	const op = "network.Measure"
	if l.measurer == nil {
		return probe.Measurement{}, apperr.Internal(op, "the laboratory has no measurer", nil)
	}
	return l.measurer.Measure(ctx, namespace, target, options)
}

// startFaults builds the measurer and the fault engine and puts them under the
// laboratory's ownership, so that a teardown takes the faults out of the
// kernel before the namespaces they live in disappear.
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

	// The declared faults go in last, once the links they sit on exist. A
	// refusal here fails the laboratory, and the engine's own rollback plus
	// the cleanup stack leave nothing behind.
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

// scope is the set of namespaces the fault engine may touch: exactly the
// namespaces this laboratory created, and nothing else.
func (l *Lab) scope() chaos.Scope {
	names := make([]string, 0, len(l.plan.Nodes)+1)
	names = append(names, l.plan.RouterNS)
	for _, node := range l.plan.Nodes {
		names = append(names, node.Namespace)
	}
	return chaos.Scope{Network: l.plan.Network, Namespaces: names}
}

// probeScope is the set of namespaces a probe may run in and the addresses it
// may aim at: exactly this laboratory's namespaces and its own network. A host
// interface or a host address is outside it, so a probe cannot reach one even
// if a request asks it to.
func (l *Lab) probeScope() probe.Scope {
	names := make([]string, 0, len(l.plan.Nodes)+1)
	names = append(names, l.plan.RouterNS)
	for _, node := range l.plan.Nodes {
		names = append(names, node.Namespace)
	}
	return probe.Scope{Network: l.plan.Network, Namespaces: names}
}

// Run executes a command in a laboratory namespace and reports its exit
// status. It is how the fault engine and the measurer reach the laboratory:
// only namespaces this laboratory created can be named, so neither can reach
// the host network.
func (l *Lab) Run(namespace string, argv ...string) (string, string, int, error) {
	space, err := l.Space(namespace)
	if err != nil {
		return "", "", 0, err
	}
	result, err := space.Run(argv...)
	return result.Stdout, result.Stderr, result.Code, err
}

// Devices returns the data interfaces of a laboratory namespace, without the
// loopback device.
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

// Peer returns the far end of the link a device belongs to.
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

// DeclaredFaults returns the faults the specification asked for. The records
// are the ones the kernel agreed to, so a caller can compare what the file
// asked for against what is actually in the laboratory.
func (l *Lab) DeclaredFaults() []chaos.Fault {
	if l.faults == nil {
		return nil
	}
	return append([]chaos.Fault(nil), l.declared...)
}
