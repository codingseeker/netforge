package integration

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/netforge/netforge/internal/chaos"
	"github.com/netforge/netforge/internal/investigator"
	"github.com/netforge/netforge/internal/network"
	"github.com/netforge/netforge/internal/probe"
)

// diagnosisLab builds a laboratory whose server namespace serves every endpoint,
// and optionally applies faults. The far end of every probe is a real listener
// and the faults are real ones in the kernel, so a diagnosis here is a
// diagnosis of a network rather than of a test fixture.
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

// investigate runs an investigation in the client namespace against the server
// and fails the test if the investigation itself could not be run.
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

// A service that is simply not listening is the clearest failure there is, and
// the investigator has to name the transport layer rather than the path.
func TestARefusedServiceIsDiagnosedAtTheTransportLayer(t *testing.T) {
	t.Parallel()
	lab, plan := diagnosisLab(t, "phase6-refused", "10.56.0.0/24")

	// Port 9999 has no listener in the server namespace, so a probe of it gets a
	// positive refusal from a host that is otherwise reachable.
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
	// The evidence has to be real measurements, and the healthy one below the
	// failure is what makes the refusal mean anything.
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
	// Every claim traces to a measurement.
	for _, hypothesis := range report.Hypotheses {
		if !hypothesis.Concludes {
			continue
		}
		if len(hypothesis.Evidence) == 0 {
			t.Errorf("the conclusion %q cites no evidence", hypothesis.Statement)
		}
	}
}

// A resolver on a closed port is not a silent one. The host answers with a port
// unreachable, so this is a positive refusal and the report is allowed to be
// firmer about it than about a datagram that simply never came back.
func TestARefusedResolverIsDiagnosedAtTheDNSLayer(t *testing.T) {
	t.Parallel()
	lab, plan := diagnosisLab(t, "phase6-dns", "10.56.1.0/24")

	// Nothing listens on 9999 as a resolver, so the datagram is refused while
	// the host keeps answering pings.
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
	// The path has to be shown working, or blaming the resolver is a guess.
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

// A handshake against a port that is not listening fails at the transport, not
// at TLS. The connection is refused before any certificate is exchanged, and a
// report that named TLS would send a reader after certificates when the port was
// never open.
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
	// A refusal is a positive answer from a host that answered, measured twice,
	// so it can be confirmed. What is confirmed is the refusal, not a guess.
	if report.Diagnosis != investigator.Confirmed {
		t.Errorf("Diagnosis = %s, want confirmed: a repeated refusal from a reachable host "+
			"is a positive answer", report.Diagnosis)
	}
	// The report has to say the layer that was observed is not the one that broke.
	if !strings.Contains(report.Summary, "never really reached") &&
		!strings.Contains(report.Summary, "below it is the one that failed") {
		t.Errorf("Summary = %q, want it to say the failure happened below the observed layer",
			report.Summary)
	}
}

// A request the server answers with an error is evidence about HTTP, and the
// layers below it are measured working.
func TestAnErrorStatusIsDiagnosedAtTheHTTPLayer(t *testing.T) {
	t.Parallel()
	lab, plan := diagnosisLab(t, "phase6-http", "10.56.3.0/24")

	// The service answers 404 for a path it does not serve. That is a real
	// answer from a real server, not a timeout, and it was measured twice.
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
	// The request really did reach the server, so the layers below must show it.
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

// The healthy laboratory is the case that has to be quiet. A working service is
// evidence that nothing is broken, and a report that named a broken layer here
// would be the most damaging kind of wrong.
func TestAHealthyServiceIsNotDiagnosedAsBroken(t *testing.T) {
	t.Parallel()
	lab, plan := diagnosisLab(t, "phase6-healthy", "10.56.4.0/24")

	// The caller reports a failure that the service does not have. The
	// investigation re-measures and finds the path working, so it must not
	// confirm a fault it could not see.
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

// Measured loss is a degradation, and reporting it as a broken layer would send
// the reader after the wrong thing.
func TestMeasuredLossIsDiagnosedAsADegradation(t *testing.T) {
	t.Parallel()
	// Drop half the packets leaving the client. This is a real netem rule in a
	// real namespace, so the loss the investigator reports is loss on the wire.
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

	// Whatever the loss turns out to be, the report has to be honest about it:
	// either the loss was measured and named as a degradation, or nothing
	// answered and the report says the evidence cannot separate the reasons.
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

	// The percentage the summary reports has to be the percentage a measurement
	// carried, never the fifty the specification asked for, and it can only be
	// carried by a measurement that received something: netem's loss is not one
	// independent coin per datagram, so a ten-datagram probe across a path
	// dropping half its packets comes back with no reply at all a few percent of
	// the time. That is not a loss measurement, it is silence, and the report has
	// to treat it as the case above describes. What must never happen is the
	// report calling such a probe a measured degradation, because then the
	// hundred percent in the summary is the number the probe assumed.
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

// A number in a report is a measurement or it is nothing. This asks the
// laboratory to drop thirty-seven percent of the client's packets and requires
// every percentage in the report to be the one the probe measured.
//
// The configured figure is thirty-seven and not a round fifty because the
// investigation repeats its failing probe exactly once, so the loss it can
// measure is zero, fifty or a hundred percent. A check for the absence of "50"
// would therefore fail on a report that was entirely made of measurements, which
// is why this compares against the measurement rather than against a string.
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

	// The laboratory really was told to drop thirty-seven percent, so a report
	// that never quotes it is a property of the report rather than of an
	// absent fault.
	declared := lab.DeclaredFaults()
	if len(declared) != 1 {
		t.Fatalf("DeclaredFaults() = %d, want 1", len(declared))
	}
	if got := declared[0].Parameters.Loss; float64(got) != configured {
		t.Errorf("the declared loss is %v percent, want %d", got, configured)
	}

	// The measurement the report may quote is the network evidence in the
	// report itself. Without it the comparison below would pass on an empty
	// set of numbers, so a report that measured nothing is a failure here.
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

// measuredNetworkLoss renders the loss the report's own network measurement
// recorded. The repeat of the failing probe is what makes it a measurement
// rather than a single unanswered request, so the evidence it produced is the
// one the report's loss claims have to agree with.
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

// reportSentences is every piece of prose a report writes about the failure:
// the summary and each hypothesis, plus the evidence notes the hypotheses are
// read alongside.
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

// quotedPercentages finds every percentage a sentence quotes, rendered to one
// decimal place so a comparison with a measured figure is exact.
func quotedPercentages(sentence string) (found []string) {
	for _, match := range percentagePattern.FindAllStringSubmatch(sentence, -1) {
		found = append(found, match[1])
	}
	return found
}

var percentagePattern = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*%`)

// An address with no route is the kernel's own answer, and it can be confirmed
// because nothing was inferred.
func TestAMissingRouteIsConfirmedFromTheKernel(t *testing.T) {
	t.Parallel()
	lab, plan := diagnosisLab(t, "phase6-noroute", "10.56.7.0/24")

	// A laboratory client has a default route, so an outside address would have
	// one too. Taking the default away leaves the client with routes only for
	// its own network, which is the situation the rule is for: the kernel is
	// asked about an address it cannot reach, and it says so itself.
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

// A laboratory with nothing on the far end: every layer fails, and the report
// has to decline to choose between a dead host and a dead path.
func TestNothingListeningIsNotResolvedIntoOneLayer(t *testing.T) {
	t.Parallel()
	lab, plan := diagnosisLab(t, "phase6-silent", "10.56.8.0/24")

	// The service is running, but the client aims at a port nothing ever
	// listens on while the host keeps answering. The refusal is positive, so
	// this must not be read as a dead host.
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
	// The measurement was repeated, so a steady refusal can be confirmed.
	if report.Diagnosis == investigator.Unknown {
		t.Errorf("Diagnosis = %s, want at least a probable for a repeated refusal",
			report.Diagnosis)
	}
}

// The investigation stays inside the laboratory it was given: a target outside
// it is refused before a single probe runs.
func TestAnInvestigationCannotLeaveTheLaboratory(t *testing.T) {
	t.Parallel()
	lab, plan := diagnosisLab(t, "phase6-scope", "10.56.9.0/24")

	engine, err := lab.Investigator(investigator.DefaultLimits())
	if err != nil {
		t.Fatalf("Investigator() = %v, want nil", err)
	}
	// The target is outside the network, and the probe engine refuses it. The
	// investigation records the refusal as evidence and concludes nothing from
	// it, rather than reaching past the laboratory to find out.
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
	// The refusal is not a measurement, so the report has to say it could not
	// measure rather than concluding anything about the target.
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
	// No evidence may claim to have measured the network.
	for _, item := range report.Evidence {
		if item.Supports() || item.Refutes() {
			t.Errorf("evidence for the %s layer claims a measurement (%q), want a refusal",
				item.Layer, item.Note)
		}
	}
	// The routing table was still read, because a kernel lookup sends no
	// traffic and is inside the laboratory's own namespace.
	if report.Rule != "measurement-failed" {
		t.Errorf("Rule = %q, want measurement-failed", report.Rule)
	}
}

// An investigation with no probe engine has no way to measure, and says so
// rather than guessing.
func TestAnInvestigatorNeedsALaboratoryWithAProbeEngine(t *testing.T) {
	t.Parallel()
	lab, plan := diagnosisLab(t, "phase6-noprober", "10.56.10.0/24")
	if lab.Prober() == nil {
		t.Fatal("Prober() = nil, want the laboratory to have one")
	}
	// A laboratory without services still probes; a diagnosis of a closed port
	// is still a real diagnosis.
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
