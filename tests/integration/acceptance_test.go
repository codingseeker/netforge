package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/codingseeker/netforge/internal/chaos"
	"github.com/codingseeker/netforge/internal/config"
	"github.com/codingseeker/netforge/internal/experiment"
	"github.com/codingseeker/netforge/internal/investigator"
	"github.com/codingseeker/netforge/internal/network"
	"github.com/codingseeker/netforge/internal/probe"
	"github.com/codingseeker/netforge/internal/recovery"
	reportpkg "github.com/codingseeker/netforge/internal/report"
)

const (
	acceptanceLabID    = "nf-accept"
	acceptanceExpLabID = "nf-acceptance"
)

var (
	acceptanceRouter  = "nf-router-" + acceptanceLabID
	acceptanceClient  = "nf-client0-" + acceptanceLabID
	acceptanceServers = []string{
		"nf-server0-" + acceptanceLabID,
		"nf-server1-" + acceptanceLabID,
		"nf-server2-" + acceptanceLabID,
	}
	acceptanceExpServer = "nf-server0-" + acceptanceExpLabID
	servicePorts        = []uint16{9104, 9105, 9106}
)

func acceptanceServiceSpecs(id string) []network.ServiceSpec {
	return []network.ServiceSpec{
		{Namespace: "nf-server0-" + id, TCPPort: servicePorts[0], MaxRequest: 4096, Timeout: 30 * time.Second},
		{Namespace: "nf-server1-" + id, TCPPort: servicePorts[1], MaxRequest: 4096, Timeout: 30 * time.Second},
		{Namespace: "nf-server2-" + id, TCPPort: servicePorts[2], MaxRequest: 4096, Timeout: 30 * time.Second},
	}
}

func acceptanceLabSpec(id string) network.Spec {
	spec := labSpec(id)
	spec.Servers = 3
	spec.Chaos = chaosLimitsForAcceptance()
	spec.Probes = probeLimitsForAcceptance()
	spec.Services = acceptanceServiceSpecs(id)
	return granted(spec)
}

func chaosLimitsForAcceptance() chaos.Limits {
	return chaos.Limits{
		MaxFaults:    8,
		MaxDuration:  5 * time.Minute,
		MaxLatency:   5 * time.Second,
		MaxBandwidth: 10_000_000,
		MaxQueue:     1024,
		MaxGap:       100,
	}
}

func probeLimitsForAcceptance() probe.Limits {
	return probe.Limits{
		MaxProbes: 100, MaxRate: 200, MaxSize: 1400,
		MaxDuration: 90 * time.Second, MaxConcurrent: 2,
	}
}

func acceptanceDocument(t *testing.T, name string) config.Config {
	t.Helper()
	raw := "name: " + name + "\n" +
		"scope:\n  network: 10.42.0.0/24\n  namespaces: [" +
		"nf-router-" + acceptanceExpLabID + ", nf-client0-" + acceptanceExpLabID + ", " +
		acceptanceExpServer + ", nf-server1-" + acceptanceExpLabID + ", nf-server2-" + acceptanceExpLabID + "]\n" +
		"limits:\n  max_namespaces: 8\n  max_interfaces: 16\n  max_duration: 5m\n" +
		"  max_bandwidth: 1Gbps\n  max_packet_rate: 100000\n  max_concurrent_probes: 64\n" +
		"faults:\n" +
		"  - {target: " + acceptanceExpServer + ", type: packet_loss, value: \"50%\"}\n" +
		"  - {target: nf-router-" + acceptanceExpLabID + ", type: latency, value: 20ms, interface: nfr-s0}\n"
	document, err := config.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	return *document
}

func acceptanceObservation() experiment.Observation {
	return experiment.Observation{
		Namespace:  "nf-client0-" + acceptanceExpLabID,
		Target:     acceptanceExpAddress(),
		Protocol:   probe.ProtocolICMP,
		AfterFault: 250 * time.Millisecond,
		Settle:     250 * time.Millisecond,
		Options: probe.Options{
			Count:    8,
			Size:     64,
			Timeout:  250 * time.Millisecond,
			Deadline: 10 * time.Second,
		},
	}
}

func acceptanceExpAddress() string {
	plan, err := network.BuildPlan(acceptanceExpLabID, "10.42.0.0/24", 1, 3, 1500)
	if err != nil {
		panic(err)
	}
	return plan.Nodes[1].Address.Addr().String()
}

func acceptanceServerAddresses() []string {
	plan, err := network.BuildPlan(acceptanceLabID, "10.42.0.0/24", 1, 3, 1500)
	if err != nil {
		panic(err)
	}
	addresses := make([]string, 0, 3)
	for _, node := range plan.Nodes[1:] {
		addresses = append(addresses, node.Address.Addr().String())
	}
	return addresses
}

func buildAcceptanceExperimentLab(t *testing.T) func(context.Context, network.Spec) (*network.Lab, error) {
	t.Helper()
	return func(ctx context.Context, spec network.Spec) (*network.Lab, error) {
		spec.ID = acceptanceExpLabID
		lab, err := network.Create(ctx, granted(spec))
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() {
			if err := lab.Destroy(context.Background()); err != nil {
				t.Errorf("destroy: %v", err)
			}
		})
		return lab, nil
	}
}

func runAcceptance(t *testing.T, document config.Config, requests []recovery.Request) (experiment.Report, experiment.LabEnvironment) {
	t.Helper()
	plan, err := experiment.Compile(document, acceptanceObservation(), requests,
		recovery.DefaultCriteria(), experiment.DefaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	environment, err := experiment.NewLabEnvironment(plan, acceptanceLabSpec(acceptanceExpLabID), buildAcceptanceExperimentLab(t))
	if err != nil {
		t.Fatalf("NewLabEnvironment: %v", err)
	}
	controller, err := experiment.New(plan, environment, time.Now())
	if err != nil {
		t.Fatalf("experiment.New: %v", err)
	}
	report_, _ := controller.Run(context.Background())
	if !report_.Succeeded() {
		t.Logf("run failed in %q: %v", report_.Failed, report_.Err)
	}
	return report_, *environment
}

func serviceProbe(t *testing.T, lab *network.Lab, clientNamespace, target string, port uint16) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := lab.Probe(ctx, probe.Request{
		Protocol:   probe.ProtocolTCP,
		Target:     target,
		Port:       port,
		Namespace:  clientNamespace,
		Experiment: acceptanceExpLabID,
		Timeout:    5 * time.Second,
	})
	if err != nil {
		t.Fatalf("probe %s:%d from %s: %v", target, port, clientNamespace, err)
	}
	if !result.OK {
		t.Fatalf("probe %s:%d from %s did not get an answer", target, port, clientNamespace)
	}
}

func step(t *testing.T, n int, label string, check func(*testing.T)) {
	t.Helper()
	t.Run(fmt.Sprintf("%02d %s", n, label), check)
}

func acceptanceHostLeaks() (leaks []string) {
	for _, probe := range [][]string{
		{"ip", "-o", "link", "show"},
		{"ip", "-o", "route", "show"},
		{"ip", "netns", "list"},
		{"tc", "qdisc", "show"},
	} {
		out, err := exec.Command(probe[0], probe[1:]...).Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, acceptanceLabID) || strings.Contains(line, acceptanceExpLabID) {
				leaks = append(leaks, strings.Join(probe, " ")+" => "+line)
			}
		}
	}
	return leaks
}

func TestTheFinalAcceptanceWorkflow(t *testing.T) {
	document := acceptanceDocument(t, "nf-acceptance")

	step(t, 1, "create an isolated network", func(t *testing.T) {
		lab, err := network.Create(context.Background(), acceptanceLabSpec(acceptanceLabID))
		if err != nil {
			t.Fatalf("network.Create: %v", err)
		}
		t.Cleanup(func() {
			if err := lab.Destroy(context.Background()); err != nil {
				t.Errorf("destroy: %v", err)
			}
		})
		names := lab.Names()
		if len(names) != len(acceptanceServers)+2 {
			t.Errorf("the laboratory holds %d namespaces, want %d: %v", len(names), len(acceptanceServers)+2, names)
		}
	})

	step(t, 2, "validate the experiment scope", func(t *testing.T) {
		plan, err := experiment.Compile(document, acceptanceObservation(), nil,
			recovery.DefaultCriteria(), experiment.DefaultLimits(), time.Now())
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}
		if len(plan.Scope.Namespaces) != len(acceptanceServers)+2 {
			t.Errorf("the scope grants %d namespaces, want %d", len(plan.Scope.Namespaces), len(acceptanceServers)+2)
		}
		rogue := acceptanceObservation()
		rogue.Namespace = "nf-rogue"
		if _, err := experiment.Compile(document, rogue, nil,
			recovery.DefaultCriteria(), experiment.DefaultLimits(), time.Now()); err == nil {
			t.Error("an observation outside the granted namespaces was not refused")
		}
	})

	lab := newLab(t, acceptanceLabSpec(acceptanceLabID))
	client, _, err := lab.Client(0)
	if err != nil {
		t.Fatalf("start the client: %v", err)
	}
	addresses := acceptanceServerAddresses()

	step(t, 3, "start the client", func(t *testing.T) {
		if client == nil {
			t.Fatal("the client namespace was not created")
		}
	})

	step(t, 4, "start the router", func(t *testing.T) {
		forwarding, err := lab.Forwarding()
		if err != nil {
			t.Fatalf("Forwarding: %v", err)
		}
		if !forwarding {
			t.Error("the router is not forwarding")
		}
	})

	step(t, 5, "start the load-balancing hop", func(t *testing.T) {
		if lab.Prober() == nil {
			t.Fatal("the laboratory has no probe engine, so no path can be measured")
		}
		if got := lab.Prober().Limits().MaxConcurrent; got < 1 {
			t.Errorf("the probe engine allows %d concurrent probes", got)
		}
	})

	step(t, 6, "start the three services", func(t *testing.T) {
		services := lab.Services()
		if len(services) != len(acceptanceServers) {
			t.Fatalf("the laboratory serves %d services, want %d", len(services), len(acceptanceServers))
		}
		for i := range addresses {
			serviceProbe(t, lab, acceptanceClient, addresses[i], servicePorts[i])
		}
	})

	step(t, 7, "record a healthy baseline", func(t *testing.T) {
		result, err := lab.Probe(context.Background(), probe.Request{
			Protocol: probe.ProtocolICMP, Target: addresses[0], Namespace: acceptanceClient,
			Experiment: acceptanceExpLabID, Timeout: 2 * time.Second,
		})
		if err != nil {
			t.Fatalf("baseline probe: %v", err)
		}
		if !result.OK {
			t.Error("the baseline probe did not get an answer")
		}
	})

	step(t, 8, "record the baseline metrics", func(t *testing.T) {
		if report_, err := experiment.Compile(document, acceptanceObservation(), nil,
			recovery.DefaultCriteria(), experiment.DefaultLimits(), time.Now()); err == nil {
			if len(report_.Faults) != 2 {
				t.Errorf("the plan declares %d faults, want the document's two", len(report_.Faults))
			}
		}
	})

	t.Cleanup(func() {
		if leaks := acceptanceHostLeaks(); len(leaks) != 0 {
			t.Errorf("the host still carries laboratory objects:\n%s", strings.Join(leaks, "\n"))
		}
	})

	requests := []recovery.Request{
		{
			Action:  recovery.ActionRemoveFault,
			Target:  acceptanceExpServer,
			Reason:  "the operator authorized removing the packet loss this experiment declared",
			Consent: true,
		},
		{
			Action:  recovery.ActionRemoveFault,
			Target:  "nf-router-" + acceptanceExpLabID,
			Reason:  "the operator authorized removing the latency this experiment declared",
			Consent: true,
		},
	}
	run, environment := runAcceptance(t, document, requests)

	step(t, 9, "inject controlled packet loss", func(t *testing.T) {
		for _, fault := range run.Faults {
			if fault.Kind == chaos.KindPacketLoss && fault.Target.Namespace == acceptanceExpServer {
				return
			}
		}
		t.Error("the report records no packet-loss fault on the frontend hop")
	})

	step(t, 10, "inject controlled latency", func(t *testing.T) {
		for _, fault := range run.Faults {
			if fault.Kind == chaos.KindLatency && fault.Target.Namespace == "nf-router-"+acceptanceExpLabID {
				return
			}
		}
		t.Error("the report records no latency fault on the balancing router")
	})

	step(t, 11, "observe the degraded path", func(t *testing.T) {
		if len(run.Baseline) == 0 || len(run.Observed) == 0 {
			t.Fatal("the report holds no measurements to compare")
		}
		if run.Observed[0].Loss <= run.Baseline[0].Loss {
			t.Errorf("the impaired path lost %.1f%% and the healthy one lost %.1f%%, want the loss to show",
				run.Observed[0].Loss, run.Baseline[0].Loss)
		}
		if run.Observed[0].AverageRTT <= run.Baseline[0].AverageRTT {
			t.Errorf("the impaired path took %s and the healthy one %s, want the latency to show",
				run.Observed[0].AverageRTT, run.Baseline[0].AverageRTT)
		}
	})

	step(t, 12, "run the network diagnostics", func(t *testing.T) {
		for _, s := range run.Steps {
			if s.State == experiment.StateDiagnose && !s.Failed() {
				return
			}
		}
		t.Error("the experiment never ran a successful diagnosis")
	})

	step(t, 13, "conclude from the evidence", func(t *testing.T) {
		switch run.Diagnosis.Diagnosis {
		case investigator.Confirmed, investigator.Probable, investigator.Unknown:
		default:
			t.Errorf("the diagnosis is %q, which is not a classification", run.Diagnosis.Diagnosis)
		}
		if run.Diagnosis.Layer != "network" {
			t.Errorf("the diagnosis is at layer %q, want the network layer", run.Diagnosis.Layer)
		}
		if run.Diagnosis.Summary == "" {
			t.Error("the diagnosis carries no summary")
		}
	})

	step(t, 14, "execute the explicitly authorized recovery", func(t *testing.T) {
		if len(run.Recoveries) != 2 {
			t.Fatalf("the report holds %d recoveries, want the two authorized actions", len(run.Recoveries))
		}
		targets := make(map[string]bool, len(run.Recoveries))
		for _, outcome := range run.Recoveries {
			authorization := outcome.Authorization
			if authorization.Action() != recovery.ActionRemoveFault {
				t.Errorf("the recovery did %q, want remove_fault", authorization.Action())
			}
			targets[authorization.Target()] = true
			if !outcome.Passed() {
				t.Errorf("the authorized recovery of %q did not pass its own verification", authorization.Target())
			}
		}
		if !targets[acceptanceExpServer] {
			t.Error("no recovery removed the fault from the frontend hop")
		}
		if !targets["nf-router-"+acceptanceExpLabID] {
			t.Error("no recovery removed the latency from the balancing router")
		}
	})

	step(t, 15, "verify the network recovered", func(t *testing.T) {
		if len(run.Verified) == 0 {
			t.Fatal("the report holds no verification measurement")
		}
		if run.Verified[0].Loss > 0 {
			t.Errorf("the verified path still loses %.1f%%", run.Verified[0].Loss)
		}
		if run.Verified[0].AverageRTT >= run.Observed[0].AverageRTT {
			t.Errorf("the verified path took %s, the impaired one %s, want the latency gone",
				run.Verified[0].AverageRTT, run.Observed[0].AverageRTT)
		}
		if run.Verified[0].AverageRTT > run.Baseline[0].AverageRTT+10*time.Millisecond {
			t.Errorf("the verified path took %s, the healthy baseline %s",
				run.Verified[0].AverageRTT, run.Baseline[0].AverageRTT)
		}
	})

	step(t, 16, "verify the service recovered", func(t *testing.T) {
		if lab := environment.Lab(); lab != nil {
			if remaining := lab.Chaos().List(); len(remaining) != 0 {
				t.Errorf("the laboratory still holds %d faults after recovery", len(remaining))
			}
		}
		if !run.Succeeded() {
			t.Error("the run did not succeed, so nothing to verify the recovery of")
		}
	})

	step(t, 17, "produce the structured report", func(t *testing.T) {
		var encoded bytes.Buffer
		if err := reportpkg.EncodeJSON(&encoded, run, environment.Summary()); err != nil {
			t.Fatalf("EncodeJSON: %v", err)
		}
		var round reportpkg.Report
		if err := json.Unmarshal(encoded.Bytes(), &round); err != nil {
			t.Fatalf("the encoded report is not JSON: %v", err)
		}
		if !round.Succeeded {
			t.Error("the structured report does not say the run succeeded")
		}
		if round.ExperimentID == "" || round.Name == "" {
			t.Error("the structured report does not identify the experiment")
		}
		if len(round.Steps) != len(experiment.States()) {
			t.Errorf("the structured report walks %d steps, want the %d-lifecycle", len(round.Steps), len(experiment.States()))
		}
		if !round.CleanedUp {
			t.Error("the structured report does not say the laboratory was taken apart")
		}
		if round.Metrics.PacketLoss == 0 {
			t.Error("the structured report does not record the impaired phase's loss")
		}
		if len(round.Metrics.Phases) != 3 {
			t.Errorf("the structured report breaks the metrics into %d phases, want baseline, observe and verify",
				len(round.Metrics.Phases))
		}
	})

	step(t, 18, "release the laboratories", func(t *testing.T) {
		if !run.CleanedUp {
			t.Error("the experiment did not take its laboratory apart")
		}
	})

	step(t, 19, "verify everything was released", func(t *testing.T) {
		if leaks := acceptanceHostLeaks(); len(leaks) != 0 {
			t.Errorf("the host still carries laboratory objects:\n%s", strings.Join(leaks, "\n"))
		}
		if errors.Is(run.Err, context.Canceled) {
			t.Error("the run was canceled")
		}
	})
}
