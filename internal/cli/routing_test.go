package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The routing and service flags are what reach a capability the laboratory
// already had. Every refusal below happens while the command is still deciding
// what to build: a refused run has created no namespace, opened no listener and
// started no process, which is the property that makes these tests runnable
// without a kernel at all.

// Phase 14's laboratory is one client and one server, and the identifier it uses
// decides the namespaces, so a test can name them without a kernel.
const (
	phase14ID     = "nfp14"
	phase14Client = "nf-client0-" + phase14ID
	phase14Server = "nf-server0-" + phase14ID
	phase14Router = "nf-router-" + phase14ID
	// phase14ServerAddr is the address the plan of a one-client, one-server
	// laboratory gives its server: the block starts at 10.42.0.4, the router
	// takes .5 and the node takes .6.
	phase14ServerAddr = "10.42.0.6"
)

// phase14Document writes the document to a file, because that is what the command
// takes: a path, or - for standard input.
func phase14Document(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "phase14.yaml")
	body := "name: phase14-cli\n" +
		"seed: 123456\n" +
		"scope:\n" +
		"  network: 10.42.0.0/24\n" +
		"  namespaces:\n" +
		"    - nf-client0-" + phase14ID + "\n" +
		"    - nf-server0-" + phase14ID + "\n" +
		"    - nf-router-" + phase14ID + "\n" +
		"limits:\n" +
		"  max_namespaces: 8\n" +
		"  max_interfaces: 16\n" +
		"  max_duration: 5m\n" +
		"  max_bandwidth: 1Gbps\n" +
		"  max_packet_rate: 100000\n" +
		"  max_concurrent_probes: 64\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func phase14Run(t *testing.T, flags ...string) runResult {
	t.Helper()
	args := append([]string{
		"experiment", "run",
		"--id", phase14ID,
		"--namespace", phase14Client,
		"--target", phase14ServerAddr,
	}, flags...)
	return run(t, context.Background(), "", append(args, phase14Document(t))...)
}

func TestRoutingRefusesAModeTheRouterDoesNotImplement(t *testing.T) {
	// The refusal is a refusal, not a default. A silently ignored
	// --routing software would report a software router that never ran, which is
	// the same class of defect as a report claiming a checksum never recomputed.
	result := phase14Run(t, "--protocol", "icmp", "--routing", "ebpf")
	if result.err == nil {
		t.Fatal("an unimplemented routing mode was accepted")
	}
	if !strings.Contains(result.err.Error(), "ebpf") {
		t.Errorf("the refusal %q does not name the mode asked for", result.err)
	}
	if !strings.Contains(result.err.Error(), "kernel or software") {
		t.Errorf("the refusal %q does not name the modes that exist", result.err)
	}
	if result.stdout != "" {
		t.Errorf("stdout = %q, want nothing printed for a refused run", result.stdout)
	}
}

func TestRoutingAcceptsBothImplementedModes(t *testing.T) {
	for _, mode := range []string{"kernel", "software"} {
		t.Run(mode, func(t *testing.T) {
			opts := &experimentOptions{routing: mode}
			got, err := opts.routingMode()
			if err != nil {
				t.Fatalf("routingMode(%q) = %v, want nil", mode, err)
			}
			want := "software"
			if mode == "kernel" {
				want = "kernel"
			}
			if got.String() != want {
				t.Errorf("routingMode(%q) = %q, want %q", mode, got.String(), want)
			}
		})
	}
	// An empty routing flag is the mode every laboratory used before the
	// software router existed, and it has to keep meaning the kernel rather than
	// becoming a refusal.
	opts := &experimentOptions{routing: "  "}
	if got, err := opts.routingMode(); err != nil || got.String() != "kernel" {
		t.Errorf("an unset -routing is %q/%v, want kernel and no error", got.String(), err)
	}
}

func TestAProtocolThatNeedsAListenerIsRefusedAgainstNothing(t *testing.T) {
	// This is the defect the phase exists to close: before it, a --protocol tcp
	// run from the command line was answered by nothing, and the report described
	// a closed port as a measurement.
	for _, protocol := range []string{"tcp", "udp", "tls", "http"} {
		t.Run(protocol, func(t *testing.T) {
			result := phase14Run(t, "--protocol", protocol, "--port", "7000")
			if result.err == nil {
				t.Fatalf("a %s observation with no listener was accepted", protocol)
			}
			if !strings.Contains(result.err.Error(), "-service-ports") {
				t.Errorf("the refusal %q does not name the flag that would answer it", result.err)
			}
			if !strings.Contains(result.err.Error(), protocol) {
				t.Errorf("the refusal %q does not name the protocol", result.err)
			}
		})
	}
}

func TestICMPIsNotAskedToServe(t *testing.T) {
	// An echo request is answered by the network itself. A listener asked for
	// here would bind a port nothing measures, and a report that listed it would
	// be describing an endpoint the run never used.
	result := phase14Run(t, "--protocol", "icmp", "--service-ports", "1", "--port", "7000")
	if result.err == nil {
		t.Fatal("a listener was accepted for a protocol that needs none")
	}
	if !strings.Contains(result.err.Error(), "icmp") {
		t.Errorf("the refusal %q does not name the protocol", result.err)
	}
}

func TestServicePortsAreBoundedByTheServersThatExist(t *testing.T) {
	result := phase14Run(t, "--protocol", "tcp", "--port", "7000",
		"--service-ports", "3", "--servers", "1")
	if result.err == nil {
		t.Fatal("more serving namespaces than servers was accepted")
	}
	if !strings.Contains(result.err.Error(), "-service-ports") ||
		!strings.Contains(result.err.Error(), "-servers") {
		t.Errorf("the refusal %q does not name both fields", result.err)
	}
}

func TestThePortsAServiceBindsAreChosenByTheCallerAndBounded(t *testing.T) {
	// The count and the base port are separate flags so that the arithmetic is
	// the caller's: the i-th serving namespace binds base+i, and a port outside
	// the range is refused rather than clamped.
	opts := &experimentOptions{
		protocol:        "tcp",
		port:            7100,
		servers:         3,
		servicePorts:    3,
		serviceBasePort: 7100,
	}
	count, base, err := opts.serviceRange()
	if err != nil {
		t.Fatalf("serviceRange() = %v, want nil", err)
	}
	if count != 3 {
		t.Errorf("serviceRange() count = %d, want 3", count)
	}
	if base != 7100 {
		t.Errorf("serviceRange() base = %d, want the observation's port 7100", base)
	}

	// A base port of zero is not a port. It is a refusal, because a service that
	// bound port 0 would answer on an ephemeral port nothing knows to ask.
	zero := &experimentOptions{protocol: "tcp", port: 0, servers: 1, servicePorts: 1}
	if _, _, err := zero.serviceRange(); err == nil {
		t.Error("a service with no port to bind was accepted")
	} else if !strings.Contains(err.Error(), "-service-base-port") {
		t.Errorf("the refusal %q does not name the field", err)
	}
}

func TestServicePortsPastTheLastPortAreRefused(t *testing.T) {
	opts := &experimentOptions{
		protocol:        "tcp",
		port:            65535,
		servers:         2,
		servicePorts:    2,
		serviceBasePort: 65535,
	}
	_, _, err := opts.serviceRange()
	if err == nil {
		t.Fatal("a range that runs past the last port was accepted")
	}
	if !strings.Contains(err.Error(), "past the last port") {
		t.Errorf("the refusal %q does not say the range is out of bounds", err)
	}
}

func TestABasePortWithoutAServingNamespaceIsRefused(t *testing.T) {
	// A base port with nothing to serve it is a flag that says nothing, and
	// accepting it would leave an operator believing a port was reserved.
	opts := &experimentOptions{protocol: "icmp", port: 0, serviceBasePort: 7000}
	_, _, err := opts.serviceRange()
	if err == nil {
		t.Fatal("a base port with no serving namespace was accepted")
	}
	if !strings.Contains(err.Error(), "-service-base-port") ||
		!strings.Contains(err.Error(), "-service-ports") {
		t.Errorf("the refusal %q does not name both fields", err)
	}
}

// phase14Options builds the options a run of the phase 14 document would carry,
// so a test can reach the arithmetic without the command line.
func phase14Options(target string) *experimentOptions {
	return &experimentOptions{
		id:              phase14ID,
		clients:         1,
		servers:         1,
		protocol:        "tcp",
		port:            7000,
		target:          target,
		servicePorts:    1,
		serviceBasePort: 7000,
	}
}

func TestAnObservationOutsideTheServedAddressesIsRefused(t *testing.T) {
	// Serving one server and measuring another is the case a count-and-base-port
	// pair makes possible, and it is refused before anything is built: the run
	// would succeed and mean nothing.
	opts := phase14Options("10.42.0.99")
	if _, err := opts.serviceSpecs("10.42.0.0/24"); err == nil {
		t.Fatal("an observation of an address nothing serves was accepted")
	} else if !strings.Contains(err.Error(), "10.42.0.99") {
		t.Errorf("the refusal %q does not name the address measured", err)
	}
}

func TestAServedAddressIsAccepted(t *testing.T) {
	// The server of a one-client, one-server plan on this network holds
	// 10.42.0.7, so a run that serves it and measures it is coherent.
	opts := phase14Options(phase14ServerAddr)
	specs, err := opts.serviceSpecs("10.42.0.0/24")
	if err != nil {
		t.Fatalf("a served address was refused: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("built %d services, want 1", len(specs))
	}
	if specs[0].Namespace != "nf-server0-"+phase14ID {
		t.Errorf("the service serves from %q, want the laboratory's own server namespace", specs[0].Namespace)
	}
}

func TestTwoRunsOnOneHostDoNotCollide(t *testing.T) {
	// The requirement the base port exists for. Two laboratories may be built at
	// the same time on one host as long as their ports differ, and because the
	// namespaces are separate a port is only ever bound inside one of them.
	first := phase14Options(phase14ServerAddr)
	second := phase14Options(phase14ServerAddr)
	second.port, second.serviceBasePort = 7100, 7100
	firstSpecs, err := first.serviceSpecs("10.42.0.0/24")
	if err != nil {
		t.Fatalf("the first run's services: %v", err)
	}
	secondSpecs, err := second.serviceSpecs("10.42.0.0/24")
	if err != nil {
		t.Fatalf("the second run's services: %v", err)
	}
	if firstSpecs[0].TCPPort == secondSpecs[0].TCPPort {
		t.Errorf("two runs chose the same port %d", firstSpecs[0].TCPPort)
	}
}

func TestThePortsOfEachServingNamespaceFollowTheBase(t *testing.T) {
	// Three servers are planned, so the third serving namespace is the one whose
	// address the observation names.
	opts := phase14Options(phase14ServerAddr)
	opts.servers, opts.servicePorts, opts.serviceBasePort = 3, 3, 7000
	specs, err := opts.serviceSpecs("10.42.0.0/24")
	if err != nil {
		t.Fatalf("serviceSpecs: %v", err)
	}
	if len(specs) != 3 {
		t.Fatalf("built %d services, want 3", len(specs))
	}
	for i, spec := range specs {
		want := uint16(7000 + i)
		if spec.TCPPort != want {
			t.Errorf("service %d binds %d, want %d: the i-th serving namespace takes base+i",
				i, spec.TCPPort, want)
		}
		if spec.Namespace == "" {
			t.Errorf("service %d has no namespace to serve from", i)
		}
	}
}
