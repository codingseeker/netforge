package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/netforge/netforge/internal/chaos"
	"github.com/netforge/netforge/internal/experiment"
	"github.com/netforge/netforge/internal/investigator"
	"github.com/netforge/netforge/internal/probe"
	"github.com/netforge/netforge/internal/recovery"
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

// sampleRun is a run that walked the whole lifecycle and repaired the path, so
// every part of the report has a real value to come from. The recovery is made
// through recovery.Authorize because that is the only producer of an
// authorization, and a report built on anything shorter would assert something
// the real code cannot produce.
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
	// The raw document must not contain the numbers the run judged broken: a
	// failed phase is not a phase that measured nothing.
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
	run.Succeeded() // the sample is a success; break it below
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
