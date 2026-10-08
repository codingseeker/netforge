package integration

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/codingseeker/netforge/internal/loadbalancer"
	"github.com/codingseeker/netforge/internal/network"
	"github.com/codingseeker/netforge/internal/recovery"
	"github.com/codingseeker/netforge/internal/transport"
)

const (
	phase16LabID     = "nfp16"
	phase16Client    = "nf-client0-" + phase16LabID
	phase16Server0   = "nf-server0-" + phase16LabID
	phase16Server1   = "nf-server1-" + phase16LabID
	phase16Balancer  = "nf-balancer0-" + phase16LabID
	phase16Router    = "nf-router-" + phase16LabID
	phase16CIDR      = "10.43.0.0/24"
	phase16BasePort  = 9300
	phase16BalancerP = phase16BasePort
)

func phase16Spec(t *testing.T, backends int) network.Spec {
	t.Helper()
	plan, err := network.BuildPlan(phase16LabID, phase16CIDR, 1, backends, 1500)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	plan, err = network.AddBalancer(plan)
	if err != nil {
		t.Fatalf("AddBalancer: %v", err)
	}
	node, ok := plan.Balancer()
	if !ok {
		t.Fatal("the plan has no balancer node")
	}
	spec := network.Spec{
		ID:      phase16LabID,
		Network: phase16CIDR,
		Clients: 1,
		Servers: backends,
		MTU:     1500,
		Names: []string{
			network.NamespaceName("client0", phase16LabID),
			network.NamespaceName("server0", phase16LabID),
			network.NamespaceName("server1", phase16LabID),
			network.NamespaceName("balancer0", phase16LabID),
			network.NamespaceName("router", phase16LabID),
		},
		Balancer: &network.BalancerSpec{
			Namespace: node.Namespace,
			Listen:    joinHostPort(node.Address.Addr().String(), phase16BalancerP),
		},
	}
	for i := 0; i < backends; i++ {
		spec.Balancer.Backends = append(spec.Balancer.Backends,
			joinHostPort(plan.Servers()[i].Address.Addr().String(), phase16BasePort+1+i))
		service, err := network.ReliableServiceFor(plan.Servers()[i].Namespace, phase16BasePort+1+uint16(i))
		if err != nil {
			t.Fatalf("ReliableServiceFor: %v", err)
		}
		spec.Services = append(spec.Services, service)
	}
	return spec
}

func joinHostPort(host string, port int) string {
	return host + ":" + itoaTest(port)
}

func itoaTest(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}

func phase16Lab(t *testing.T) *network.Lab {
	t.Helper()
	lab, err := network.Create(context.Background(), phase16Spec(t, 2))
	if err != nil {
		t.Fatalf("create a laboratory with a balancer: %v", err)
	}
	t.Cleanup(func() { _ = lab.Destroy(context.Background()) })
	return lab
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func balancerState(t *testing.T, lab *network.Lab, index int) loadbalancer.Backend {
	t.Helper()
	status, err := lab.BalancerStatus()
	if err != nil {
		t.Fatalf("BalancerStatus: %v", err)
	}
	if index >= len(status.Backends) {
		t.Fatalf("the balancer reports %d backends, want more than %d", len(status.Backends), index)
	}
	return status.Backends[index]
}

func TestALaboratoryCanCarryALoadBalancer(t *testing.T) {
	lab := phase16Lab(t)

	namespace, ok := lab.BalancerNamespace()
	if !ok || namespace != phase16Balancer {
		t.Fatalf("BalancerNamespace() = %q/%t, want %q", namespace, ok, phase16Balancer)
	}
	names := lab.Names()
	found := false
	for _, name := range names {
		if name == phase16Balancer {
			found = true
		}
	}
	if !found {
		t.Errorf("the balancer's namespace %q is not one of the laboratory's: %v", phase16Balancer, names)
	}

	eventually(t, "both backends to be checked and healthy", func() bool {
		status, err := lab.BalancerStatus()
		if err != nil {
			return false
		}
		if status.State != loadbalancer.StateRunning || len(status.Backends) != 2 {
			return false
		}
		if status.Stats.Checks == 0 || status.Stats.FailedChecks != 0 {
			return false
		}
		for _, backend := range status.Backends {
			if backend.State != loadbalancer.StateHealthy || backend.Checks == 0 {
				return false
			}
		}
		return true
	})

	status, err := lab.BalancerStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.Stats.Checks == 0 {
		t.Error("the balancer never checked a backend, so nothing proved it healthy")
	}
	if status.Listen == "" {
		t.Error("the balancer reports no listen address, so a reader cannot find it")
	}
	if status.Policy != string(loadbalancer.PolicyRoundRobin) {
		t.Errorf("the policy is %q, want the balancer's own default", status.Policy)
	}
}

func TestTheBalancerDistributesAcrossItsBackends(t *testing.T) {
	lab := phase16Lab(t)
	listen := lab.BalancerListen()
	if listen == "" {
		t.Fatal("the laboratory reports no balancer listen address")
	}
	eventually(t, "the balancer to be checked and healthy", func() bool {
		status, err := lab.BalancerStatus()
		return err == nil && len(status.Backends) == 2 && status.Stats.Checks > 0
	})

	_, client, err := lab.Client(0)
	if err != nil {
		t.Fatal(err)
	}
	const rounds = 6
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code, err := lab.Run(client.Namespace,
		self,
		"-test.run=^TestPhase16BalancerClient$",
		"-phase16-address="+listen,
		"-phase16-rounds="+itoaTest(rounds),
	)
	if err != nil {
		t.Fatalf("run the client in %s: %v", client.Namespace, err)
	}
	if code != 0 {
		t.Fatalf("the client in %s exited with %d:\n%s", client.Namespace, code, stderr)
	}
	if !strings.Contains(stdout, "phase16-ok") {
		t.Fatalf("the client did not report success:\n%s\n%s", stdout, stderr)
	}

	eventually(t, "both backends to have been chosen", func() bool {
		status, err := lab.BalancerStatus()
		if err != nil {
			return false
		}
		if len(status.Backends) != 2 {
			return false
		}
		return status.Backends[0].Accepted > 0 && status.Backends[1].Accepted > 0
	})
	first := balancerState(t, lab, 0)
	second := balancerState(t, lab, 1)
	if first.Accepted+second.Accepted < rounds {
		t.Errorf("the backends accepted %d and %d connections for %d rounds",
			first.Accepted, second.Accepted, rounds)
	}
	if first.Forwarded == 0 || second.Forwarded == 0 {
		t.Errorf("one backend forwarded no bytes at all: %d and %d",
			first.Forwarded, second.Forwarded)
	}
}

var (
	phase16Address = flag.String("phase16-address", "", "the balancer address to dial")
	phase16Rounds  = flag.Int("phase16-rounds", 0, "how many connections to open")
)

func TestPhase16BalancerClient(t *testing.T) {
	if *phase16Address == "" || *phase16Rounds == 0 {
		t.Skip("this test is the client half of TestTheBalancerDistributesAcrossItsBackends")
	}
	limits := transport.Limits{
		MaxConnections: 8,
		MaxPayload:     64,
		MaxWindow:      16,
		MaxDatagram:    transport.MaxFrameSize,
		IdleTimeout:    5 * time.Second,
	}
	checked, err := limits.Validate()
	if err != nil {
		t.Fatalf("the client's transport limits are refused: %v", err)
	}
	for i := 0; i < *phase16Rounds; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn, err := transport.Dial(ctx, "", *phase16Address, checked)
		if err != nil {
			cancel()
			t.Fatalf("round %d: dial %s: %v", i, *phase16Address, err)
		}
		payload := []byte("phase16")
		if _, err := conn.Write(ctx, payload); err != nil {
			cancel()
			t.Fatalf("round %d: write: %v", i, err)
		}
		answer, err := conn.Read(ctx)
		if err != nil {
			cancel()
			t.Fatalf("round %d: read: %v", i, err)
		}
		if string(answer) != string(payload) {
			cancel()
			t.Fatalf("round %d: the balancer answered %q, want the payload echoed", i, answer)
		}
		_ = conn.Close()
		cancel()
	}
	fmt.Println("phase16-ok")
}

func TestAnAuthorizedBackendActionRunsAgainstARealBalancer(t *testing.T) {
	lab := phase16Lab(t)
	eventually(t, "both backends to be checked and healthy", func() bool {
		status, err := lab.BalancerStatus()
		if err != nil {
			return false
		}
		if status.Stats.Checks == 0 || status.Stats.FailedChecks != 0 {
			return false
		}
		for _, backend := range status.Backends {
			if backend.State != loadbalancer.StateHealthy || backend.Checks == 0 {
				return false
			}
		}
		return true
	})
	target := balancerState(t, lab, 0).Address

	path, experiment, ok := lab.BalancerControl()
	if !ok {
		t.Fatal("the laboratory exposes no balancer control document")
	}
	control, err := recovery.NewControlFile(path, experiment)
	if err != nil {
		t.Fatalf("NewControlFile: %v", err)
	}
	ctx := context.Background()
	if err := control.Remove(ctx, target); err != nil {
		t.Fatalf("Remove(%s) = %v, want nil", target, err)
	}
	eventually(t, "the backend to be reported removed", func() bool {
		return balancerState(t, lab, 0).State == loadbalancer.StateRemoved
	})

	if got := balancerState(t, lab, 1).State; got != loadbalancer.StateHealthy {
		t.Errorf("removing one backend left the other %q, want it healthy", got)
	}

	if err := control.Restore(ctx, target); err != nil {
		t.Fatalf("Restore(%s) = %v, want nil", target, err)
	}
	eventually(t, "the backend to come back", func() bool {
		return balancerState(t, lab, 0).State == loadbalancer.StateHealthy
	})
}

func TestABackendActionOnALaboratoryWithNoBalancerIsRefused(t *testing.T) {

	lab, err := network.Create(context.Background(), network.Spec{
		ID:      phase16LabID,
		Network: phase16CIDR,
		Clients: 1,
		Servers: 1,
		MTU:     1500,
		Names: []string{
			network.NamespaceName("client0", phase16LabID),
			network.NamespaceName("server0", phase16LabID),
			network.NamespaceName("router", phase16LabID),
		},
	})
	if err != nil {
		t.Fatalf("create a laboratory with no balancer: %v", err)
	}
	t.Cleanup(func() { _ = lab.Destroy(context.Background()) })

	agent, err := recovery.NewLab(lab, nil, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	err = agent.RemoveBackend(context.Background(), phase16Client, "10.43.0.6:9000")
	if err == nil {
		t.Fatal("a backend action ran on a laboratory with no balancer")
	}
	if !strings.Contains(err.Error(), "no load balancer") {
		t.Errorf("the refusal %q does not say there is no balancer", err)
	}
}

func TestTheRecoveryActionsSayWhichNeedABalancer(t *testing.T) {

	result := execNetforge(t, "", "recovery", "actions")
	if result.exitCode != 0 {
		t.Fatalf("recovery actions failed: exit %d\n%s", result.exitCode, result.stderr)
	}
	for _, action := range []string{
		string(recovery.ActionRemoveBackend),
		string(recovery.ActionRestoreBackend),
		string(recovery.ActionDrainConnection),
		string(recovery.ActionReEnableBackend),
	} {
		line := listingLineOf(result.stdout, action)
		if line == "" {
			t.Errorf("the action %s is not listed", action)
			continue
		}
		if !strings.Contains(line, "--balancer") {
			t.Errorf("the action %s is listed without saying it needs a balancer: %q", action, line)
		}
	}
	if !strings.Contains(result.stdout, "-recover-from") {
		t.Errorf("the output does not say where an address recovery is verified from:\n%s", result.stdout)
	}
}

func listingLineOf(output, fragment string) string {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), fragment) {
			return line
		}
	}
	return ""
}

func TestTheBalancerFlagsAreRefusedBeforeAnythingExists(t *testing.T) {

	root := t.TempDir()
	env := map[string]string{"NETFORGE_STATE_ROOT": root}
	cases := []struct {
		name  string
		flags []string
		want  string
	}{
		{
			name:  "more backends than servers",
			flags: []string{"--protocol", "icmp", "--target", "10.43.0.14", "--servers", "1", "--balancer", "2", "--balancer-base-port", "9300"},
			want:  "-balancer",
		},
		{
			name:  "a base port with no balancer",
			flags: []string{"--protocol", "icmp", "--target", "10.43.0.14", "--balancer-base-port", "9300"},
			want:  "-balancer-base-port",
		},
		{
			name:  "a range past the last port",
			flags: []string{"--protocol", "icmp", "--target", "10.43.0.14", "--servers", "2", "--balancer", "2", "--balancer-base-port", "65535"},
			want:  "past the last port",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{
				"experiment", "run",
				"--id", phase16LabID,
				"--namespace", phase16Client,
				"--count", "2",
				"--timeout", "300ms",
				"--deadline", "3s",
			}, test.flags...)
			result := execNetforgeEnv(t, "", env, append(args, phase16Document(t))...)
			if result.exitCode == 0 {
				t.Fatalf("the run was accepted:\n%s", result.stdout)
			}
			if !strings.Contains(result.stderr, test.want) {
				t.Errorf("the refusal does not name %q:\n%s", test.want, result.stderr)
			}
			if result.stdout != "" {
				t.Errorf("a refused run printed a report:\n%s", result.stdout)
			}
			listing := execNetforgeEnv(t, "", env, "reconcile", "list")
			if !strings.Contains(listing.stdout, "nothing is there") {
				t.Errorf("a refused run left state behind:\n%s", listing.stdout)
			}
		})
	}
}

func TestTheBalancerFlagsAppearInHelp(t *testing.T) {
	result := execNetforge(t, "", "help", "experiment", "run")
	if result.exitCode != 0 {
		t.Fatalf("help failed: exit %d\n%s", result.exitCode, result.stderr)
	}
	for _, want := range []string{"--balancer", "--balancer-base-port", "-recover-from"} {
		if !strings.Contains(result.stdout, want) {
			t.Errorf("the help for experiment run does not mention %q:\n%s", want, result.stdout)
		}
	}
}

func phase16Document(t *testing.T) string {
	t.Helper()
	return writeFile(t, "phase16.yaml", "name: phase16-balancer\n"+
		"seed: 4242\n"+
		"scope:\n"+
		"  network: "+phase16CIDR+"\n"+
		"  namespaces:\n"+
		"    - "+phase16Client+"\n"+
		"    - "+phase16Server0+"\n"+
		"    - "+phase16Server1+"\n"+
		"    - "+phase16Balancer+"\n"+
		"    - "+phase16Router+"\n"+
		"limits:\n"+
		"  max_namespaces: 8\n"+
		"  max_interfaces: 16\n"+
		"  max_duration: 5m\n"+
		"  max_bandwidth: 1Gbps\n"+
		"  max_packet_rate: 100000\n"+
		"  max_concurrent_probes: 64\n"+
		"faults:\n"+
		"  - target: "+phase16Server0+"\n"+
		"    type: packet_loss\n"+
		"    value: 70%\n")
}

func TestARunReportsWhatItsBalancerWasDoing(t *testing.T) {

	document := writeFile(t, "phase16-report.yaml", "name: phase16-balancer\n"+
		"seed: 4242\n"+
		"scope:\n"+
		"  network: "+phase16CIDR+"\n"+
		"  namespaces:\n"+
		"    - "+phase16Client+"\n"+
		"    - "+phase16Server0+"\n"+
		"    - "+phase16Server1+"\n"+
		"    - "+phase16Balancer+"\n"+
		"    - "+phase16Router+"\n"+
		"limits:\n"+
		"  max_namespaces: 8\n"+
		"  max_interfaces: 16\n"+
		"  max_duration: 5m\n"+
		"  max_bandwidth: 1Gbps\n"+
		"  max_packet_rate: 100000\n"+
		"  max_concurrent_probes: 64\n"+
		"faults:\n"+
		"  - target: "+phase16Balancer+"\n"+
		"    type: packet_loss\n"+
		"    value: 70%\n")
	result := execNetforge(t, "",
		"experiment", "run",
		"--id", phase16LabID,
		"--namespace", phase16Client,
		"--target", "10.43.0.14",
		"--protocol", "icmp",
		"--servers", "2",
		"--balancer", "2",
		"--balancer-base-port", itoaTest(phase16BasePort),
		"--count", "10",
		"--timeout", "300ms",
		"--deadline", "8s",
		"--after-fault", "300ms",
		"--settle", "500ms",
		"-recover", "remove_fault:"+phase16Balancer,
		"-authorize",
		"-why", "the run removes the fault its own document declared",
		"--output", "json",
		document)
	if result.exitCode != 0 {
		t.Fatalf("the run failed: exit %d\n%s\n%s", result.exitCode, result.stdout, result.stderr)
	}
	report := decodeRun(t, result.stdout)
	lab := report.Laboratory
	if lab.Balancer == nil {
		t.Fatalf("the report carries no balancer, so it does not say what the run built:\n%s", result.stdout)
	}
	if lab.Balancer.Namespace != phase16Balancer {
		t.Errorf("the balancer's namespace is %q, want %q", lab.Balancer.Namespace, phase16Balancer)
	}
	if lab.Balancer.Listen == "" {
		t.Error("the balancer reports no listen address, so a reader cannot find it")
	}
	if lab.Balancer.Policy == "" {
		t.Error("the balancer reports no policy, so a reader cannot tell how it chose")
	}
	if len(lab.Balancer.Backends) != 2 {
		t.Fatalf("the report carries %d backends, want 2", len(lab.Balancer.Backends))
	}

	for i, backend := range lab.Balancer.Backends {
		if backend.Address == "" {
			t.Errorf("backend %d has no address", i)
		}
		if backend.State == "" {
			t.Errorf("backend %d has no state, so a reader cannot tell whether it was usable", i)
		}
		if backend.Checks == 0 {
			t.Errorf("backend %d was never checked, so nothing proved its state", i)
		}
	}
	if lab.Balancer.Healthy != 2 {
		t.Errorf("the report says %d healthy backends, want 2", lab.Balancer.Healthy)
	}

	found := false
	for _, node := range lab.Nodes {
		if node.Namespace == phase16Balancer && node.Role == "balancer" {
			found = true
		}
	}
	if !found {
		t.Errorf("the report does not name the balancer namespace as a laboratory node: %+v", lab.Nodes)
	}
}
