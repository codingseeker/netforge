package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/codingseeker/netforge/internal/chaos"
	"github.com/codingseeker/netforge/internal/experiment"
	"github.com/codingseeker/netforge/internal/investigator"
	"github.com/codingseeker/netforge/internal/probe"
	"github.com/codingseeker/netforge/internal/recovery"
)

func sampleMeasurement(sent, received int, rtt time.Duration, bytes int64) probe.Measurement {
	return probe.Measurement{
		Target:     "10.42.0.6",
		Namespace:  "nf-client0-x",
		Sent:       sent,
		Received:   received,
		MinRTT:     rtt,
		AverageRTT: rtt,
		MaxRTT:     rtt + 10*time.Millisecond,
		Bytes:      bytes,
		Duration:   time.Second,
	}
}

func sampleRun(t *testing.T) experiment.Report {
	t.Helper()
	seed := uint64(7)
	steps := []struct {
		state    experiment.State
		detail   string
		duration time.Duration
	}{
		{experiment.StateSetup, "creating the laboratory", time.Second},
		{experiment.StateBaseline, "measuring the healthy path", 2 * time.Second},
		{experiment.StateFault, "imposing the document's fault", 100 * time.Millisecond},
		{experiment.StateObserve, "measuring the impaired path", 2 * time.Second},
		{experiment.StateDiagnose, "interrogating the evidence", 20 * time.Millisecond},
		{experiment.StateRecover, "applying the authorized action", 50 * time.Millisecond},
		{experiment.StateVerify, "measuring the repaired path", time.Second},
	}
	outcome, err := recovery.Authorize(
		recovery.Scope{
			Network:    netip.MustParsePrefix("10.42.0.0/24"),
			Namespaces: []string{"nf-client0-x", "nf-server0-x"},
			Experiment: "sample",
		},
		recovery.Request{
			Action:  recovery.ActionRemoveFault,
			Target:  "nf-server0-x",
			From:    "nf-client0-x",
			Reason:  "the document authorizes this",
			Consent: true,
		},
		recovery.DefaultCriteria(),
		recovery.DefaultLimits(),
		time.Now(),
	)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	var run experiment.Report
	for _, step := range steps {
		record := experiment.StepReport{
			State: step.state, Detail: step.detail, Duration: step.duration,
		}
		if step.state == experiment.StateRecover {
			record.Recovery = []string{"remove_fault"}
		}
		run.Steps = append(run.Steps, record)
	}
	run.Name = "sample"
	run.ExperimentID = "nf-sample-01234567"
	run.Seed = &seed
	run.Baseline = []probe.Measurement{sampleMeasurement(5, 5, time.Millisecond, 360)}
	run.Observed = []probe.Measurement{sampleMeasurement(5, 0, 0, 0)}
	run.Verified = []probe.Measurement{sampleMeasurement(5, 5, 2*time.Millisecond, 360)}
	run.Faults = []chaos.Fault{
		{
			ID:        "f-1",
			Kind:      chaos.KindPacketLoss,
			Target:    chaos.Endpoint{Namespace: "nf-server0-x", Interface: "eth0"},
			Direction: chaos.DirectionIn,
			Status:    chaos.StatusRemoved,
		},
	}
	run.Diagnosis = investigator.Report{
		Diagnosis: investigator.Confirmed,
		Layer:     "link",
		Summary:   "packets dropped on the path are the observed loss",
		Rule:      "packet_loss_consistent",
		Evidence:  []investigator.Evidence{{Probed: true}},
		ProbesRun: 3,
	}
	run.Recoveries = []recovery.Outcome{{
		Authorization: outcome,
		RecoveredAt:   time.Now(),
		Verification: recovery.Verification{
			Passed: true,
			Reason: "loss is 0% over 5 replies, average round trip 2ms",
		},
	}}
	run.Duration = 30 * time.Second
	run.CleanedUp = true
	return run
}

func TestFromExperimentCarriesTheRun(t *testing.T) {
	report := FromExperiment(sampleRun(t), nil)
	if report.Name != "sample" || report.ExperimentID != "nf-sample-01234567" {
		t.Errorf("the report names %q / %q, want the run's own name", report.Name, report.ExperimentID)
	}
	if !report.Succeeded || report.Failed != "" || report.Error != "" {
		t.Errorf("a finished run was reported as succeeded=%t failed=%q error=%q", report.Succeeded, report.Failed, report.Error)
	}
	if report.Seed == nil || *report.Seed != 7 {
		t.Errorf("Seed = %v, want 7", report.Seed)
	}
	if !report.CleanedUp || report.Duration != "30s" {
		t.Errorf("the run was reported cleaned_up=%t duration=%q", report.CleanedUp, report.Duration)
	}
	if len(report.Steps) != 7 {
		t.Fatalf("the report carries %d steps, want the 7 lifecycle states", len(report.Steps))
	}
	if report.Steps[5].State != "recover" || report.Steps[5].Recovery != "remove_fault" {
		t.Errorf("step 5 is %q with recovery %q, want the recover state to name its actions",
			report.Steps[5].State, report.Steps[5].Recovery)
	}
	if len(report.Faults) != 1 || report.Faults[0].Target != "nf-server0-x/eth0" {
		t.Errorf("Faults = %+v, want the injected fault rendered on its endpoint", report.Faults)
	}
	if report.Diagnosis.Classification != "CONFIRMED" || report.Diagnosis.Evidence != 1 || report.Diagnosis.ProbesRun != 3 {
		t.Errorf("Diagnosis = %+v, want the confirmed classification with the cited evidence", report.Diagnosis)
	}
	if len(report.Recoveries) != 1 {
		t.Fatalf("Recoveries = %d outcomes, want 1", len(report.Recoveries))
	}
	if recovery := report.Recoveries[0]; !recovery.Passed || recovery.Action != "remove_fault" ||
		recovery.Target != "nf-server0-x" || recovery.Reason != "the document authorizes this" ||
		recovery.Result == "" {
		t.Errorf("the recovery outcome %+v does not name the authorized action and its verdict", recovery)
	}
	if report.Metrics.PacketsSent != 15 {
		t.Errorf("the report's metrics count %d packets, want the three phases at 5 each", report.Metrics.PacketsSent)
	}
}

func TestEncodeJSONRoundTrips(t *testing.T) {
	report := FromExperiment(sampleRun(t), nil)
	var buf bytes.Buffer
	if err := EncodeJSON(&buf, sampleRun(t), nil); err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	var decoded Report
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("the encoded report is not JSON: %v\n%s", err, buf.String())
	}
	if decoded.Name != report.Name || decoded.ExperimentID != report.ExperimentID {
		t.Errorf("the round-trip changed the run's identity")
	}
	if decoded.Metrics.PacketsSent != report.Metrics.PacketsSent ||
		decoded.Metrics.Availability != report.Metrics.Availability {
		t.Errorf("the round-trip changed the metrics %+v", decoded.Metrics)
	}
	if len(decoded.Steps) != len(report.Steps) || decoded.Steps[5].Recovery != "remove_fault" {
		t.Errorf("the round-trip changed the lifecycle")
	}
	if decoded.Recommendation != report.Recommendation {
		t.Errorf("the round-trip changed the recommendation:\n%+v\n%+v", decoded.Recommendation, report.Recommendation)
	}
	if !decoded.Recommendation.Diagnosed || decoded.Recommendation.Statement == "" {
		t.Errorf("the round-trip lost the recommendation: %+v", decoded.Recommendation)
	}

	if strings.Contains(buf.String(), `"error"`) {
		t.Errorf("a successful run was encoded with an error field:\n%s", buf.String())
	}
}

func TestEncodeTextNamesTheRunAndEveryMetric(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeText(&buf, sampleRun(t), nil); err != nil {
		t.Fatalf("EncodeText: %v", err)
	}
	text := buf.String()
	for _, want := range []string{
		"experiment: sample",
		"experiment_id: nf-sample-01234567",
		"result: succeeded",
		"cleaned up: true",
		"lifecycle:",
		"metrics:",
		"packets sent:",
		"bytes:",
		"rtt:",
		"packet loss:",
		"retransmissions:",
		"throughput:",
		"connection failures:",
		"availability:",
		"recovery time:",
		"diagnostic duration:",
		"remove_fault",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the text report is missing %q:\n%s", want, text)
		}
	}
}

func TestEncodeTextReportsAFailedRun(t *testing.T) {
	run := sampleRun(t)
	run.Steps[len(run.Steps)-1].Err = errors.New("the verification found loss")
	run.Err = errors.New("the verification found loss")
	run.Failed = experiment.StateVerify
	var buf bytes.Buffer
	if err := EncodeText(&buf, run, nil); err != nil {
		t.Fatalf("EncodeText: %v", err)
	}
	text := buf.String()
	if !strings.Contains(text, "result: failed in verify") || !strings.Contains(text, "the verification found loss") {
		t.Errorf("a failed run is not reported as failed:\n%s", text)
	}
}

func TestBothFormatsCarryTheRecommendation(t *testing.T) {
	run := sampleRun(t)
	run.Diagnosis = investigator.Report{
		Diagnosis: investigator.Probable,
		Layer:     "network",
		Summary:   "the path to the target is impaired rather than broken",
		Rule:      "degraded-path",
		Observation: investigator.Observation{
			Namespace: "nf-client0-x",
			Target:    "10.42.0.6",
		},
	}
	run.Recommendation = recovery.Propose(run.Diagnosis)
	if run.Recommendation.Action == "" {
		t.Fatal("the diagnosis proposes no action, so the test would prove nothing")
	}

	var jsonBuf bytes.Buffer
	if err := EncodeJSON(&jsonBuf, run, nil); err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(jsonBuf.Bytes(), &raw); err != nil {
		t.Fatalf("the report is not JSON: %v", err)
	}
	carried, present := raw["recommendation"]
	if !present {
		t.Fatal("the JSON report has no recommendation key at all")
	}
	fields, ok := carried.(map[string]any)
	if !ok {
		t.Fatalf("the recommendation is %T, want an object", carried)
	}
	if fields["diagnosed"] != true {
		t.Errorf("the recommendation is not marked diagnosed: %+v", fields)
	}
	if fields["flag"] != "-recover remove_fault:nf-client0-x" {
		t.Errorf("the recommendation renders flag %v, want the -recover flag", fields["flag"])
	}
	if fields["from"] != "nf-client0-x" {
		t.Errorf("the recommendation names from %v, want the namespace the probes ran in", fields["from"])
	}
	if statement, _ := fields["statement"].(string); statement == "" {
		t.Error("the recommendation carries no statement")
	}

	var textBuf bytes.Buffer
	if err := EncodeText(&textBuf, run, nil); err != nil {
		t.Fatalf("EncodeText: %v", err)
	}
	text := textBuf.String()
	for _, want := range []string{
		"recommendation:",
		"diagnosed: true",
		"-recover remove_fault:nf-client0-x",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the text report does not carry %q:\n%s", want, text)
		}
	}
}

func TestARunThatDiagnosedNothingSaysSo(t *testing.T) {
	run := sampleRun(t)
	run.Diagnosis = investigator.Report{Diagnosis: investigator.Unknown}
	run.Recommendation = recovery.Propose(run.Diagnosis)

	var buf bytes.Buffer
	if err := EncodeJSON(&buf, run, nil); err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatalf("the report is not JSON: %v", err)
	}
	carried, present := raw["recommendation"]
	if !present {
		t.Fatal("a run that diagnosed nothing omitted the recommendation key instead of saying so")
	}
	fields, ok := carried.(map[string]any)
	if !ok {
		t.Fatalf("the recommendation is %T, want an object", carried)
	}
	if fields["diagnosed"] != false {
		t.Errorf("a run that diagnosed nothing is marked diagnosed: %+v", fields)
	}
	if statement, _ := fields["statement"].(string); !strings.Contains(statement, "nothing was diagnosed") {
		t.Errorf("the statement %q does not say that nothing was diagnosed", statement)
	}
	if _, present := fields["flag"]; present {
		t.Errorf("a run with no recommendation still offers a flag: %+v", fields)
	}

	var textBuf bytes.Buffer
	if err := EncodeText(&textBuf, run, nil); err != nil {
		t.Fatalf("EncodeText: %v", err)
	}
	if !strings.Contains(textBuf.String(), "diagnosed: false") {
		t.Errorf("the text report does not say that nothing was diagnosed:\n%s", textBuf.String())
	}
}

func TestADiagnosedRunWithNoRepairSaysThatToo(t *testing.T) {
	run := sampleRun(t)
	run.Diagnosis = investigator.Report{Diagnosis: investigator.Confirmed, Layer: "link"}
	run.Recommendation = recovery.Propose(run.Diagnosis)
	if run.Recommendation.Action != "" {
		t.Fatal("the link layer proposes an action, so the test would prove nothing")
	}
	report := FromExperiment(run, nil)
	if !report.Recommendation.Diagnosed {
		t.Error("a confirmed diagnosis is not marked diagnosed")
	}
	if !strings.Contains(report.Recommendation.Statement, "no authorized repair") {
		t.Errorf("the statement %q does not say that the evidence names no repair", report.Recommendation.Statement)
	}
	if report.Recommendation.Flag != "" {
		t.Errorf("a report with no recommendation still offers the flag %q", report.Recommendation.Flag)
	}
}

func TestTheRenderedRecommendationIsAFlagTheCommandLineAccepts(t *testing.T) {
	run := sampleRun(t)
	run.Diagnosis = investigator.Report{
		Diagnosis:   investigator.Probable,
		Layer:       "network",
		Summary:     "the path is impaired",
		Observation: investigator.Observation{Namespace: "nf-client0-x", Target: "10.42.0.6"},
	}
	run.Recommendation = recovery.Propose(run.Diagnosis)

	report := FromExperiment(run, nil)
	rendered := report.Recommendation.Flag
	rest, found := strings.CutPrefix(rendered, "-recover ")
	if !found {
		t.Fatalf("the recommendation %q is not a -recover flag", rendered)
	}
	action, target, found := strings.Cut(rest, ":")
	if !found || action == "" || target == "" {
		t.Fatalf("the recommendation %q is not action:target", rendered)
	}
	if action != "remove_fault" || target != "nf-client0-x" {
		t.Errorf("the recommendation parses as %q on %q", action, target)
	}
	if got := recovery.Action(action).String(); got != action {
		t.Errorf("the recommended action %q is not one this build performs, it reads back as %q", action, got)
	}
}
