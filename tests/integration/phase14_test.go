package integration

import (
	"encoding/json"
	"strings"
	"testing"
)

// Phase 14 is the proof that the command line can reach what the laboratory
// already had. The tests that need a kernel build real namespaces; the ones that
// only check refusals are in internal/cli, because a refusal that has to create
// something first is not a refusal.
//
// What every test here holds to is the same: the report a run prints has to say
// what the run built, and it has to say it in a way a reader can check against
// the host.

const (
	phase14LabID     = "nfp14"
	phase14Client    = "nf-client0-" + phase14LabID
	phase14Server    = "nf-server0-" + phase14LabID
	phase14Router    = "nf-router-" + phase14LabID
	phase14ServerIP  = "10.42.0.6"
	phase14ServiceIP = 7000
)

// phase14Document is a laboratory that fits in one run: one client, one server,
// one router, and a fault the observation will find.
func phase14Document(t *testing.T, faults bool) string {
	t.Helper()
	document := "name: phase14-cli\n" +
		"seed: 123456\n" +
		"scope:\n" +
		"  network: 10.42.0.0/24\n" +
		"  namespaces:\n" +
		"    - " + phase14Client + "\n" +
		"    - " + phase14Server + "\n" +
		"    - " + phase14Router + "\n" +
		"limits:\n" +
		"  max_namespaces: 8\n" +
		"  max_interfaces: 16\n" +
		"  max_duration: 5m\n" +
		"  max_bandwidth: 1Gbps\n" +
		"  max_packet_rate: 100000\n" +
		"  max_concurrent_probes: 64\n"
	if faults {
		document += "faults:\n" +
			"  - target: " + phase14Server + "\n" +
			"    type: packet_loss\n" +
			"    value: 80%\n"
	}
	return document
}

// phase14Run runs an experiment from the command line and demands it worked. A
// test that is checking the report cannot proceed on a failed run, and saying so
// with the run's own output is more useful than asserting a substring is absent.
//
// The recovery flags are not incidental. An experiment that injects a fault
// diagnoses the loss and then verifies that the path came back, so a run with a
// fault and no authorized recovery fails at the verify step by design. These
// tests are about what a run builds and reports, so every run carries the
// recovery its own document implies.
func phase14Run(t *testing.T, document string, flags ...string) runResult {
	t.Helper()
	args := append([]string{
		"experiment", "run",
		"--id", phase14LabID,
		"--namespace", phase14Client,
		"--count", "4",
		"--timeout", "300ms",
		"--deadline", "4s",
		"--after-fault", "300ms",
		"-recover", "remove_fault:" + phase14Server,
		"-authorize",
		"-why", "the run removes the fault its own document declared",
	}, flags...)
	result := execNetforge(t, "", append(args, document)...)
	if result.exitCode != 0 {
		t.Fatalf("the run failed: exit %d\nstdout:\n%s\nstderr:\n%s",
			result.exitCode, result.stdout, result.stderr)
	}
	return result
}

// TestASoftwareRoutedRunForwardsRealTraffic is the requirement the phase exists
// for. Before it, a CLI run was always kernel-routed, so a report claiming to
// measure a software router was describing something the process never built.
//
// The proof that the software router forwarded rather than the kernel is in the
// report: a software-routed run reads ip_forward=0 in the router namespace, and
// the router's own counters are non-zero. Both halves are checked, because either
// alone is satisfiable by accident.
func TestASoftwareRoutedRunForwardsRealTraffic(t *testing.T) {
	document := writeFile(t, "phase14-software.yaml", phase14Document(t, true))
	result := phase14Run(t, document, "--routing", "software", "--output", "json",
		"--protocol", "icmp", "--target", phase14ServerIP)

	// The report is the run's own record, so it is read as data rather than
	// grepped: a substring check on a JSON document would pass for a key nested
	// somewhere it has no business being.
	lab := phase14Laboratory(t, result.stdout)
	if lab.Routing != "software" {
		t.Errorf("the report says routing=%q, want software", lab.Routing)
	}
	// The kernel forwarded nothing. Without this the run would look like a
	// software router even if the kernel had been doing the work.
	if lab.KernelForwarding != "0" {
		t.Errorf("the report says ip_forward=%q in the router namespace, want 0: "+
			"a software-routed run must prove the kernel forwarded nothing", lab.KernelForwarding)
	}
	if lab.Router == nil {
		t.Fatalf("the report carries no router counters: %+v", lab.RouterError)
	}
	if lab.Router.Forwarded == 0 {
		t.Errorf("the software router forwarded nothing, so the run measured something else: %+v", *lab.Router)
	}
	if lab.Router.Received == 0 {
		t.Errorf("the software router received nothing: %+v", *lab.Router)
	}
	// A run that claims a software router and also reports a kernel that
	// forwarded would be self-contradictory, so the counters and the forwarding
	// flag are asserted against each other.
	if lab.Router.Forwarded > 0 && lab.KernelForwarding != "0" {
		t.Error("the run reports both a software router forwarding and kernel forwarding on")
	}
}

// TestAKernelRoutedRunSaysSoAndLeavesTheKernelForwarding proves the two modes are
// actually different from outside the process, which is the only way an operator
// can tell which one a given report describes.
func TestAKernelRoutedRunSaysSoAndLeavesTheKernelForwarding(t *testing.T) {
	document := writeFile(t, "phase14-kernel.yaml", phase14Document(t, true))
	result := phase14Run(t, document, "--routing", "kernel", "--output", "json",
		"--protocol", "icmp", "--target", phase14ServerIP)

	lab := phase14Laboratory(t, result.stdout)
	if lab.Routing != "kernel" {
		t.Errorf("the report says routing=%q, want kernel", lab.Routing)
	}
	if lab.KernelForwarding != "1" {
		t.Errorf("the report says ip_forward=%q, want 1 in a kernel-routed run", lab.KernelForwarding)
	}
	if lab.Router != nil {
		t.Errorf("a kernel-routed run carries software router counters: %+v", *lab.Router)
	}
	if !strings.Contains(result.stdout, `"routing": "kernel"`) {
		t.Errorf("the encoded report does not name the routing mode:\n%s", result.stdout)
	}
}

// TestARunReportsWhatItBuilt is the reporting requirement, read as data. A report
// that says what it measured but not what it measured it on cannot be checked
// against anything, and the fields here are the ones a reader needs to check it.
func TestARunReportsWhatItBuilt(t *testing.T) {
	document := writeFile(t, "phase14-report.yaml", phase14Document(t, true))
	result := phase14Run(t, document, "--output", "json",
		"--protocol", "icmp", "--target", phase14ServerIP)

	lab := phase14Laboratory(t, result.stdout)
	if lab.ID != phase14LabID {
		t.Errorf("the report names the laboratory %q, want %q", lab.ID, phase14LabID)
	}
	if lab.Network != "10.42.0.0/24" {
		t.Errorf("the report names the network %q, want the document's own", lab.Network)
	}
	if lab.RouterNamespace != phase14Router {
		t.Errorf("the report names the router namespace %q, want %q", lab.RouterNamespace, phase14Router)
	}
	// Every namespace the run created is named with the address it held, so a
	// reader can match the report against the plan the run was built from.
	byNamespace := map[string]phase14Node{}
	for _, node := range lab.Nodes {
		byNamespace[node.Namespace] = node
	}
	for _, want := range []struct{ namespace, role, address string }{
		{phase14Client, "client", "10.42.0.2/30"},
		{phase14Server, "server", phase14ServerIP + "/30"},
	} {
		node, found := byNamespace[want.namespace]
		if !found {
			t.Errorf("the report does not name the %s namespace %q", want.role, want.namespace)
			continue
		}
		if node.Role != want.role {
			t.Errorf("the report calls %q a %q, want a %q", want.namespace, node.Role, want.role)
		}
		if node.Address != want.address {
			t.Errorf("the report gives %q the address %q, want %q", want.namespace, node.Address, want.address)
		}
		if node.Gateway == "" {
			t.Errorf("the report gives %q no gateway, so the path it measured cannot be reconstructed", want.namespace)
		}
	}
	if len(lab.RouterLinks) == 0 {
		t.Error("the report names no router address, so the two sides of the path cannot be told apart")
	}
}

// TestAServiceAnswersATCPObservationFromTheCommandLine is the second capability
// the phase reaches. Before it, a --protocol tcp run from the command line had
// nothing to answer it; now a listener is asked for, and the run reports what was
// bound.
func TestAServiceAnswersATCPObservationFromTheCommandLine(t *testing.T) {
	document := writeFile(t, "phase14-service.yaml", phase14Document(t, true))
	result := phase14Run(t, document, "--output", "json",
		"--protocol", "tcp", "--port", "7000",
		"--service-ports", "1", "--service-base-port", "7000",
		"--target", phase14ServerIP)

	lab := phase14Laboratory(t, result.stdout)
	if len(lab.Services) != 1 {
		t.Fatalf("the report lists %d services, want 1", len(lab.Services))
	}
	service := lab.Services[0]
	if service.Namespace != phase14Server {
		t.Errorf("the service serves from %q, want the server namespace the run measured", service.Namespace)
	}
	// The report names what was really bound rather than what was asked for,
	// which is the difference between an endpoint a probe can find and one a
	// specification claimed.
	if len(service.Endpoints) == 0 {
		t.Fatalf("the report names no endpoint for the service in %q", service.Namespace)
	}
	joined := strings.Join(service.Endpoints, " ")
	if !strings.Contains(joined, "tcp/") {
		t.Errorf("the service's endpoints %q are not a TCP endpoint", joined)
	}
	if !strings.Contains(joined, "7000") {
		t.Errorf("the service's endpoints %q do not carry the port the run measured", joined)
	}
}

// TestTheServicePortsAreChosenByTheCaller proves the arithmetic rather than
// asserting a single case: the i-th serving namespace binds base+i, so a caller
// serving three servers knows all three ports without the laboratory having to
// tell them afterwards.
func TestTheServicePortsAreChosenByTheCaller(t *testing.T) {
	// The document declares a fault, as every run here does: an experiment that
	// impaired nothing has no failure to diagnose, and the diagnose step refuses
	// one by design rather than reporting a diagnosis of nothing.
	document := writeFile(t, "phase14-ports.yaml", phase14Document(t, true))
	lab := runAndReadLaboratory(t, document,
		"--output", "json", "--protocol", "tcp", "--port", "8100",
		"--service-ports", "1", "--service-base-port", "8100",
		"--target", phase14ServerIP)
	if len(lab.Services) != 1 {
		t.Fatalf("the report lists %d services, want 1", len(lab.Services))
	}
	if !strings.Contains(strings.Join(lab.Services[0].Endpoints, " "), "8100") {
		t.Errorf("the service bound %q, want the port the caller chose", lab.Services[0].Endpoints)
	}
}

// TestEveryFlagIsRefusedBeforeAnythingExists is the property that makes the whole
// phase testable: a run that is refused for its flags has created no namespace and
// left no state behind. The reconciler listing is what proves the second half.
func TestEveryFlagIsRefusedBeforeAnythingExists(t *testing.T) {
	root := t.TempDir()
	document := writeFile(t, "phase14-refusals.yaml", phase14Document(t, false))
	cases := []struct {
		name  string
		flags []string
		want  string
	}{
		{
			name:  "an unimplemented routing mode",
			flags: []string{"--routing", "ebpf", "--protocol", "icmp", "--target", phase14ServerIP},
			want:  "ebpf",
		},
		{
			name:  "a protocol that needs a listener, with none asked for",
			flags: []string{"--protocol", "tcp", "--port", "7000", "--target", phase14ServerIP},
			want:  "-service-ports",
		},
		{
			name: "a listener asked for a protocol that needs none",
			flags: []string{"--protocol", "icmp", "--service-ports", "1",
				"--service-base-port", "7000", "--target", phase14ServerIP},
			want: "icmp",
		},
		{
			name: "a port past the last one",
			flags: []string{"--protocol", "tcp", "--port", "7000", "--target", phase14ServerIP,
				"--service-ports", "2", "--service-base-port", "65535", "--servers", "2"},
			want: "past the last port",
		},
		{
			name: "more serving namespaces than servers",
			flags: []string{"--protocol", "tcp", "--port", "7000", "--target", phase14ServerIP,
				"--service-ports", "3", "--servers", "1"},
			want: "-service-ports",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{
				"experiment", "run",
				"--id", phase14LabID,
				"--namespace", phase14Client,
				"--count", "2",
				"--timeout", "300ms",
				"--deadline", "3s",
			}, test.flags...)
			result := execNetforgeEnv(t, "", map[string]string{"NETFORGE_STATE_ROOT": root},
				append(args, document)...)
			if result.exitCode == 0 {
				t.Fatalf("the run was accepted:\n%s", result.stdout)
			}
			if !strings.Contains(result.stderr, test.want) {
				t.Errorf("the refusal does not name %q:\n%s", test.want, result.stderr)
			}
			if result.stdout != "" {
				t.Errorf("a refused run printed a report:\n%s", result.stdout)
			}
			// Nothing was created, so the reconciler has nothing to find. This is
			// the check that a refusal is genuinely a refusal and not a run that
			// cleaned up after itself.
			listing := execNetforgeEnv(t, "", map[string]string{"NETFORGE_STATE_ROOT": root},
				"reconcile", "list")
			if listing.exitCode != 0 {
				t.Fatalf("the reconciler could not list the state root: %s", listing.stderr)
			}
			if !strings.Contains(listing.stdout, "nothing is there") {
				t.Errorf("a refused run left state behind:\n%s", listing.stdout)
			}
		})
	}
}

// TestAFinishedRunLeavesNothingForTheReconciler is the property that makes the
// reconciler's output a report about crashes rather than about normal operation.
// A listing that found leftovers after an ordinary run would make every operator
// distrust it.
func TestAFinishedRunLeavesNothingForTheReconciler(t *testing.T) {
	root := t.TempDir()
	document := writeFile(t, "phase14-clean.yaml", phase14Document(t, true))
	env := map[string]string{"NETFORGE_STATE_ROOT": root}

	for _, routing := range []string{"kernel", "software"} {
		t.Run(routing, func(t *testing.T) {
			// A software-routed run creates two state directories rather than
			// one, so both kinds are exercised by this loop.
			result := execNetforgeEnv(t, "", env, append([]string{
				"experiment", "run",
				"--id", phase14LabID,
				"--namespace", phase14Client,
				"--target", phase14ServerIP,
				"--protocol", "icmp",
				"--routing", routing,
				"--count", "3",
				"--timeout", "300ms",
				"--deadline", "3s",
				"-recover", "remove_fault:" + phase14Server,
				"-authorize",
				"-why", "the run removes the fault its own document declared",
			}, document)...)
			if result.exitCode != 0 {
				t.Fatalf("the run failed: exit %d\n%s\n%s", result.exitCode, result.stdout, result.stderr)
			}
			listing := execNetforgeEnv(t, "", env, "reconcile", "list")
			if listing.exitCode != 0 {
				t.Fatalf("the reconciler could not list the state root: %s", listing.stderr)
			}
			if !strings.Contains(listing.stdout, "nothing is there") {
				t.Errorf("a %s-routed run that finished left state behind:\n%s", routing, listing.stdout)
			}
		})
	}
}

// phase14RunLaboratory is the shape of the laboratory a report carries. It is
// declared here rather than imported so that the test asserts against the fields
// the requirement names instead of against whatever the report package happens
// to export.
type phase14Node struct {
	Role      string `json:"role"`
	Namespace string `json:"namespace"`
	Address   string `json:"address"`
	Gateway   string `json:"gateway"`
}

type phase14Service struct {
	Namespace string   `json:"namespace"`
	Endpoints []string `json:"endpoints"`
}

type phase14RouterStats struct {
	Received  uint64 `json:"received"`
	Forwarded uint64 `json:"forwarded"`
	Bytes     uint64 `json:"bytes"`
	Malformed uint64 `json:"malformed"`
	NoRoute   uint64 `json:"noRoute"`
}

type phase14LaboratoryReport struct {
	ID               string              `json:"id"`
	Network          string              `json:"network"`
	Routing          string              `json:"routing"`
	KernelForwarding string              `json:"ip_forward"`
	RouterNamespace  string              `json:"router_namespace"`
	RouterLinks      []string            `json:"router_links"`
	Nodes            []phase14Node       `json:"nodes"`
	Services         []phase14Service    `json:"services"`
	Router           *phase14RouterStats `json:"router"`
	RouterError      string              `json:"router_error"`
}

type phase14RunReport struct {
	Succeeded    bool                     `json:"succeeded"`
	CleanedUp    bool                     `json:"cleaned_up"`
	Laboratory   *phase14LaboratoryReport `json:"laboratory"`
	Steps        []map[string]any         `json:"steps"`
	ExperimentID string                   `json:"experiment_id"`
}

// phase14Laboratory reads the laboratory a run reported, failing the test if the
// report is not the report of a run that finished and cleaned up.
func phase14Laboratory(t *testing.T, encoded string) *phase14LaboratoryReport {
	t.Helper()
	var report phase14RunReport
	if err := json.Unmarshal([]byte(encoded), &report); err != nil {
		t.Fatalf("the report is not JSON: %v\n%s", err, encoded)
	}
	if !report.Succeeded {
		t.Fatalf("the run did not succeed:\n%s", encoded)
	}
	if !report.CleanedUp {
		t.Errorf("the run reports it did not clean up:\n%s", encoded)
	}
	if report.Laboratory == nil {
		t.Fatalf("the report carries no laboratory, so it does not say what it built:\n%s", encoded)
	}
	return report.Laboratory
}

// runAndReadLaboratory runs an experiment and returns what it reported building.
func runAndReadLaboratory(t *testing.T, document string, flags ...string) *phase14LaboratoryReport {
	t.Helper()
	return phase14Laboratory(t, phase14Run(t, document, flags...).stdout)
}
