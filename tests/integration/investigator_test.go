package integration

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/codingseeker/netforge/internal/chaos"
	"github.com/codingseeker/netforge/internal/investigator"
	"github.com/codingseeker/netforge/internal/network"
	"github.com/codingseeker/netforge/internal/probe"
)

func diagnosisLab(t *testing.T, id, cidr string, faults ...chaos.Spec) (*network.Lab, network.Plan) {
	t.Helper()
	spec, plan := probeLabSpec(t, id, cidr)
	spec.Faults = faults
	lab, err := network.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("Create() = %v, want nil", err)
	}
	t.Cleanup(func() { destroy(t, lab) })
	return lab, plan
}

func investigate(t *testing.T, lab *network.Lab, plan network.Plan, given investigator.Observation) investigator.Report {
	t.Helper()
	given.Experiment = "phase6"
	if given.Namespace == "" {
		given.Namespace = plan.Nodes[0].Namespace
	}
	if given.Target == "" {
		given.Target = plan.Nodes[1].Address.Addr().String()
	}
	if given.At.IsZero() {
		given.At = time.Now()
	}
	engine, err := lab.Investigator(investigator.DefaultLimits())
	if err != nil {
		t.Fatalf("Investigator() = %v, want nil", err)
	}
	report, err := engine.Investigate(context.Background(), given)
	if err != nil {
		t.Fatalf("Investigate() = %v, want nil", err)
	}
	return report
}

func TestARefusedServiceIsDiagnosedAtTheTransportLayer(t *testing.T) {
	t.Parallel()
	lab, plan := diagnosisLab(t, "phase6-refused", "10.56.0.0/24")

	report := investigate(t, lab, plan, investigator.Observation{
		Failed:  probe.ProtocolTCP,
		Port:    9999,
		Failure: probe.FailureRefused,
		Error:   "connect: connection refused",
	})

	if report.Layer != "transport" {
		t.Errorf("Layer = %q, want transport", report.Layer)
	}
	if report.Diagnosis == investigator.Unknown {
		t.Errorf("Diagnosis = %s, want at least a probable: the host answered and refused",
			report.Diagnosis)
	}
	if !strings.Contains(report.Summary, "9999") {
		t.Errorf("Summary = %q, want it to name the port that refused", report.Summary)
	}
	if report.FailureDomain == "" {
		t.Error("FailureDomain is empty, want the path the failure is on")
	}

	working := 0
	for _, item := range report.Evidence {
		if item.Supports() {
			working++
		}
		if !item.Probed && item.Layer != "routing" {
			t.Errorf("evidence for the %s layer was not probed and is not a route: %+v", item.Layer, item)
		}
	}
	if working == 0 {
		t.Error("no evidence shows a layer working, so the diagnosis rests on nothing")
	}
	if report.ProbesRun == 0 {
		t.Error("ProbesRun = 0, want the investigation to have measured something")
	}

	for _, hypothesis := range report.Hypotheses {
		if !hypothesis.Concludes {
			continue
		}
		if len(hypothesis.Evidence) == 0 {
			t.Errorf("the conclusion %q cites no evidence", hypothesis.Statement)
		}
	}
}

func TestARefusedResolverIsDiagnosedAtTheDNSLayer(t *testing.T) {
	t.Parallel()
	lab, plan := diagnosisLab(t, "phase6-dns", "10.56.1.0/24")

	report := investigate(t, lab, plan, investigator.Observation{
		Failed:  probe.ProtocolDNS,
		Port:    9999,
		Name:    "service.lab",
		Failure: probe.FailureRefused,
		Error:   "the resolver refused the datagram",
	})

	if report.Layer != "dns" {
		t.Errorf("Layer = %q, want dns", report.Layer)
	}
	if report.Diagnosis == investigator.Unknown {
		t.Errorf("Diagnosis = %s, want at least a probable: the host answered and refused",
			report.Diagnosis)
	}
	if !strings.Contains(report.Summary, "9999") {
		t.Errorf("Summary = %q, want it to name the port that refused", report.Summary)
	}

	var pathWorking bool
	for _, item := range report.Evidence {
		if item.Layer == "network" && item.Supports() {
			pathWorking = true
		}
	}
	if !pathWorking {
		t.Error("the network layer was not measured working, so the resolver cannot be blamed")
	}
}

func TestARefusedHandshakeIsDiagnosedAtTheTransportLayer(t *testing.T) {
	t.Parallel()
	lab, plan := diagnosisLab(t, "phase6-tls", "10.56.2.0/24")

	report := investigate(t, lab, plan, investigator.Observation{
		Failed:  probe.ProtocolTLS,
		Port:    9999,
		Name:    "netforge.lab",
		Failure: probe.FailureRefused,
		Error:   "connect: connection refused",
	})

	if report.Layer != "transport" {
		t.Errorf("Layer = %q, want transport: the connection was refused before any "+
			"handshake could start, so this is not a TLS failure", report.Layer)
	}

	if report.Diagnosis != investigator.Confirmed {
		t.Errorf("Diagnosis = %s, want confirmed: a repeated refusal from a reachable host "+
			"is a positive answer", report.Diagnosis)
	}

	if !strings.Contains(report.Summary, "never really reached") &&
		!strings.Contains(report.Summary, "below it is the one that failed") {
		t.Errorf("Summary = %q, want it to say the failure happened below the observed layer",
			report.Summary)
	}
}

func TestAnErrorStatusIsDiagnosedAtTheHTTPLayer(t *testing.T) {
	t.Parallel()
	lab, plan := diagnosisLab(t, "phase6-http", "10.56.3.0/24")

	report := investigate(t, lab, plan, investigator.Observation{
		Failed:  probe.ProtocolHTTP,
		Port:    9104,
		Path:    "/nothing-here",
		Failure: probe.FailureHTTP,
		Error:   "HTTP 404",
	})

	if report.Layer != "http" {
		t.Errorf("Layer = %q, want http", report.Layer)
	}
	if report.Diagnosis == investigator.Unknown {
		t.Errorf("Diagnosis = %s, want at least a probable: the server answered with a status",
			report.Diagnosis)
	}
	if !strings.Contains(report.Summary, "404") {
		t.Errorf("Summary = %q, want it to carry the status the server returned", report.Summary)
	}

	for _, layer := range []string{"network", "transport"} {
		var found bool
		for _, item := range report.Evidence {
			if item.Layer == layer && item.Supports() {
				found = true
			}
		}
		if !found {
			t.Errorf("the %s layer is not shown working, so blaming http would be a guess", layer)
		}
	}
}

func TestAHealthyServiceIsNotDiagnosedAsBroken(t *testing.T) {
	t.Parallel()
	lab, plan := diagnosisLab(t, "phase6-healthy", "10.56.4.0/24")

	report := investigate(t, lab, plan, investigator.Observation{
		Failed:  probe.ProtocolHTTP,
		Port:    9104,
		Path:    "/health",
		Failure: probe.FailureTimeout,
		Error:   "the request timed out",
	})

	if report.Diagnosis == investigator.Confirmed {
		t.Errorf("Diagnosis = %s, want less than confirmed: the service answered when re-measured",
			report.Diagnosis)
	}
	if report.Rule != "did-not-reproduce" && report.Rule != "inconclusive" {
		t.Errorf("Rule = %q, want the report to say the failure did not come back", report.Rule)
	}
	if !strings.Contains(report.Summary, string(report.Diagnosis)) {
		t.Errorf("Summary = %q, want it to open with the classification %s", report.Summary, report.Diagnosis)
	}
}

func TestMeasuredLossIsDiagnosedAsADegradation(t *testing.T) {
	t.Parallel()

	spec, plan := probeLabSpec(t, "phase6-loss", "10.56.5.0/24")
	spec.Faults = []chaos.Spec{{
		Target:    plan.Nodes[0].Namespace,
		Kind:      chaos.KindPacketLoss,
		Direction: chaos.DirectionOut,
		Parameters: chaos.Parameters{
			Loss: 50,
		},
		Duration: chaos.Duration(time.Minute),
	}}
	lab, err := network.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("Create() = %v, want nil", err)
	}
	defer destroy(t, lab)

	report := investigate(t, lab, plan, investigator.Observation{
		Failed:  probe.ProtocolHTTP,
		Port:    9104,
		Path:    "/health",
		Failure: probe.FailureTimeout,
		Error:   "the request timed out",
	})

	switch {
	case report.Rule == "degraded-path":
		if report.Diagnosis == investigator.Confirmed {
			t.Errorf("Diagnosis = %s, want less than confirmed for a degraded path",
				report.Diagnosis)
		}
		if !strings.Contains(report.Summary, "%") && !strings.Contains(report.Summary, "loss") {
			t.Errorf("Summary = %q, want it to carry the loss that was measured", report.Summary)
		}
	case report.Diagnosis == investigator.Confirmed:
		t.Errorf("Diagnosis = %s, want less than confirmed: half the packets were dropped, "+
			"which is not a single broken layer", report.Diagnosis)
	case report.Diagnosis == investigator.Unknown:
		if !strings.Contains(report.Summary, "cannot") && !strings.Contains(report.Summary, "UNKNOWN") {
			t.Errorf("Summary = %q, want it to say the evidence separates nothing", report.Summary)
		}
	default:
		t.Logf("report = %s, layer %q, rule %q", report.Diagnosis, report.Layer, report.Rule)
	}

	if report.Rule == "degraded-path" {
		measured := false
		for _, item := range report.Evidence {
			if item.Layer == "network" && item.Probed && item.Result.Succeeded() {
				measured = true
			}
		}
		if !measured {
			t.Errorf("the report calls the path degraded with no reply received to measure it "+
				"(rule=%q diagnosis=%s summary=%q): %+v", report.Rule, report.Diagnosis,
				report.Summary, report.Evidence)
		}
	}
}

func TestAConfiguredFaultIsNotReportedAsAMeasurement(t *testing.T) {
	t.Parallel()
	const configured = 37
	spec, plan := probeLabSpec(t, "phase6-configured", "10.56.6.0/24")
	spec.Faults = []chaos.Spec{{
		Target:    plan.Nodes[0].Namespace,
		Kind:      chaos.KindPacketLoss,
		Direction: chaos.DirectionOut,
		Parameters: chaos.Parameters{
			Loss: configured,
		},
		Duration: chaos.Duration(time.Minute),
	}}
	lab, err := network.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("Create() = %v, want nil", err)
	}
	defer destroy(t, lab)

	report := investigate(t, lab, plan, investigator.Observation{
		Failed:  probe.ProtocolHTTP,
		Port:    9104,
		Path:    "/health",
		Failure: probe.FailureTimeout,
		Error:   "the request timed out",
	})

	declared := lab.DeclaredFaults()
	if len(declared) != 1 {
		t.Fatalf("DeclaredFaults() = %d, want 1", len(declared))
	}
	if got := declared[0].Parameters.Loss; float64(got) != configured {
		t.Errorf("the declared loss is %v percent, want %d", got, configured)
	}

	measured, ok := measuredNetworkLoss(report)
	if !ok {
		t.Fatalf("the investigation measured no network layer, so it has no number to report: %+v", report.Evidence)
	}
	for _, text := range reportSentences(report) {
		for _, quoted := range quotedPercentages(text) {
			if quoted != measured {
				t.Errorf("the report quotes %s%% of loss where the probe measured %s%%: %q",
					quoted, measured, text)
			}
		}
	}
	if reported := fmt.Sprintf("%.1f", float64(configured)); reported == measured {
		t.Errorf("the measured loss is %s%%, the same figure as the configured fault; "+
			"this check cannot tell a measurement from a configured value", measured)
	}
}

func measuredNetworkLoss(report investigator.Report) (string, bool) {
	var measured string
	for _, item := range report.Evidence {
		if item.Layer != "network" || !item.Probed || item.Result.Sent <= 0 {
			continue
		}
		measured = fmt.Sprintf("%.1f", item.Result.Loss)
	}
	return measured, measured != ""
}

func reportSentences(report investigator.Report) []string {
	sentences := []string{report.Summary}
	for _, hypothesis := range report.Hypotheses {
		sentences = append(sentences, hypothesis.Statement)
	}
	for _, item := range report.Evidence {
		sentences = append(sentences, item.Note, item.Result.Detail)
	}
	return sentences
}

func quotedPercentages(sentence string) (found []string) {
	for _, match := range percentagePattern.FindAllStringSubmatch(sentence, -1) {
		found = append(found, match[1])
	}
	return found
}

var percentagePattern = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*%`)

func TestAMissingRouteIsConfirmedFromTheKernel(t *testing.T) {
	t.Parallel()
	lab, plan := diagnosisLab(t, "phase6-noroute", "10.56.7.0/24")

	if _, stderr, code, err := lab.Run(plan.Nodes[0].Namespace, "ip", "route", "del", "default"); err != nil || code != 0 {
		t.Fatalf("ip route del default = code %d, %q, %v, want nil", code, stderr, err)
	}
	report := investigate(t, lab, plan, investigator.Observation{
		Target:  "203.0.113.7",
		Failed:  probe.ProtocolTCP,
		Port:    9100,
		Failure: probe.FailureUnreachable,
		Error:   "connect: network is unreachable",
	})

	if report.Diagnosis != investigator.Confirmed {
		t.Errorf("Diagnosis = %s, want confirmed: the kernel's routing table is the evidence",
			report.Diagnosis)
	}
	if report.Layer != "routing" {
		t.Errorf("Layer = %q, want routing", report.Layer)
	}
	if report.Rule != "no-route" {
		t.Errorf("Rule = %q, want no-route", report.Rule)
	}
	if !strings.Contains(report.Summary, "203.0.113.7") {
		t.Errorf("Summary = %q, want it to name the address", report.Summary)
	}
}

func TestNothingListeningIsNotResolvedIntoOneLayer(t *testing.T) {
	t.Parallel()
	lab, plan := diagnosisLab(t, "phase6-silent", "10.56.8.0/24")

	report := investigate(t, lab, plan, investigator.Observation{
		Failed:  probe.ProtocolTCP,
		Port:    9998,
		Failure: probe.FailureRefused,
		Error:   "connect: connection refused",
	})

	if report.Layer != "transport" {
		t.Errorf("Layer = %q, want transport: a host that answers and refuses is not a dead host",
			report.Layer)
	}

	if report.Diagnosis == investigator.Unknown {
		t.Errorf("Diagnosis = %s, want at least a probable for a repeated refusal",
			report.Diagnosis)
	}
}

func TestAnInvestigationCannotLeaveTheLaboratory(t *testing.T) {
	t.Parallel()
	lab, plan := diagnosisLab(t, "phase6-scope", "10.56.9.0/24")

	engine, err := lab.Investigator(investigator.DefaultLimits())
	if err != nil {
		t.Fatalf("Investigator() = %v, want nil", err)
	}

	report, err := engine.Investigate(context.Background(), investigator.Observation{
		Experiment: "phase6-scope",
		Namespace:  plan.Nodes[0].Namespace,
		Target:     "203.0.113.7",
		Port:       9100,
		Failed:     probe.ProtocolTCP,
		Failure:    probe.FailureUnreachable,
		Error:      "connect: network is unreachable",
		At:         time.Now(),
	})
	if err != nil {
		t.Fatalf("Investigate() = %v, want nil", err)
	}
	if report.Diagnosis == investigator.Confirmed && report.Layer == "transport" {
		t.Errorf("the report blamed the transport layer for a target outside the "+
			"laboratory: %s", report.Summary)
	}

	if report.Diagnosis != investigator.Unknown {
		t.Errorf("Diagnosis = %s, want unknown: the target is outside the laboratory and "+
			"nothing was measured", report.Diagnosis)
	}
	if report.Layer != "" {
		t.Errorf("Layer = %q, want no layer named for a target that was never measured",
			report.Layer)
	}
	if !strings.Contains(report.Summary, "outside the scope") {
		t.Errorf("Summary = %q, want it to carry why the measurement was refused", report.Summary)
	}

	for _, item := range report.Evidence {
		if item.Supports() || item.Refutes() {
			t.Errorf("evidence for the %s layer claims a measurement (%q), want a refusal",
				item.Layer, item.Note)
		}
	}

	if report.Rule != "measurement-failed" {
		t.Errorf("Rule = %q, want measurement-failed", report.Rule)
	}
}

func TestAnInvestigatorNeedsALaboratoryWithAProbeEngine(t *testing.T) {
	t.Parallel()
	lab, plan := diagnosisLab(t, "phase6-noprober", "10.56.10.0/24")
	if lab.Prober() == nil {
		t.Fatal("Prober() = nil, want the laboratory to have one")
	}

	report := investigate(t, lab, plan, investigator.Observation{
		Failed:  probe.ProtocolTCP,
		Port:    9997,
		Failure: probe.FailureRefused,
		Error:   "connect: connection refused",
	})
	if report.Layer == "" && report.Diagnosis != investigator.Unknown {
		t.Errorf("Layer = %q with %s, want either a layer or an unknown conclusion",
			report.Layer, report.Diagnosis)
	}
}
