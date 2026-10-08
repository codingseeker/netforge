package experiment

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/codingseeker/netforge/internal/logging"
	"github.com/codingseeker/netforge/internal/network"
	"github.com/codingseeker/netforge/internal/recovery"
)

type traceRecord struct {
	Event        string `json:"event"`
	ExperimentID string `json:"experiment_id"`
	State        string `json:"state"`
	Namespace    string `json:"namespace"`
	FaultID      string `json:"fault_id"`
	Action       string `json:"action"`
}

func tracedRun(t *testing.T, plan Plan, env Environment) []traceRecord {
	t.Helper()
	buf := &bytes.Buffer{}
	logger, err := logging.New(buf, logging.Options{Level: slog.LevelDebug, Format: logging.FormatJSON})
	if err != nil {
		t.Fatalf("logging.New() = %v, want nil", err)
	}
	controller, err := New(plan, env, time.Now())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	controller.WithLogger(logging.WithExperiment(logger, plan.ExperimentID))
	_, _ = controller.Run(context.Background())

	var records []traceRecord
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record traceRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("the trace line %q is not a record: %v", line, err)
		}
		records = append(records, record)
	}
	if len(records) == 0 {
		t.Fatalf("the run emitted no records at all:\n%s", buf.String())
	}
	return records
}

func namesOf(records []traceRecord, event string) []string {
	var out []string
	for _, record := range records {
		if record.Event != event {
			continue
		}
		switch event {
		case logging.EventNamespace:
			out = append(out, record.Namespace)
		case logging.EventFaultApplied:
			out = append(out, record.FaultID)
		case logging.EventAction:
			out = append(out, record.Action)
		default:
			out = append(out, record.State)
		}
	}
	return out
}

func TestEveryStageOfTheLifecycleEmitsItsOwnRecord(t *testing.T) {
	plan, err := compile(t, testDocument(t), testObservation(), []recovery.Request{{
		Action:  recovery.ActionRemoveFault,
		Target:  serviceB,
		Reason:  "the operator confirmed the fault is the cause",
		Consent: true,
	}})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	records := tracedRun(t, plan, &fakeEnvironment{broken: true})

	states := States()
	stages := logging.StageEvents()
	if len(states) != len(stages) {
		t.Fatalf("the lifecycle has %d states and the vocabulary names %d stage events",
			len(states), len(stages))
	}
	for i, stage := range stages {
		emitted := namesOf(records, stage)
		if len(emitted) != 1 {
			t.Errorf("the event %q emitted %d records naming %v, want exactly one", stage, len(emitted), emitted)
			continue
		}
		if emitted[0] != states[i].String() {
			t.Errorf("the event %q named the state %q, want %q", stage, emitted[0], states[i].String())
		}
	}
}

func TestEveryRecordNamesTheExperimentAndOnlyOneEvent(t *testing.T) {
	plan := testPlan(t)
	records := tracedRun(t, plan, &fakeEnvironment{})

	for i, record := range records {
		if record.ExperimentID != plan.ExperimentID {
			t.Errorf("record %d (%q) carries experiment %q, want %q",
				i, record.Event, record.ExperimentID, plan.ExperimentID)
		}
		if !logging.IsEvent(record.Event) {
			t.Errorf("record %d carries the event %q, which is not in the vocabulary", i, record.Event)
		}
	}
}

func TestTheTraceNamesWhatTheRunDid(t *testing.T) {
	plan, err := compile(t, testDocument(t), testObservation(), []recovery.Request{{
		Action:  recovery.ActionRemoveFault,
		Target:  serviceB,
		Reason:  "the operator confirmed the fault is the cause",
		Consent: true,
	}})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	records := tracedRun(t, plan, &fakeEnvironment{broken: true})

	if got := namesOf(records, logging.EventFaultApplied); len(got) == 0 {
		t.Errorf("the trace names no fault the run applied:\n%+v", records)
	}
	if got := namesOf(records, logging.EventFaultRemoved); len(got) == 0 {
		t.Errorf("the trace names no fault the run removed:\n%+v", records)
	}
	if got := namesOf(records, logging.EventAction); len(got) != 1 ||
		got[0] != string(recovery.ActionRemoveFault) {
		t.Errorf("the trace names the actions %v, want the one that was authorized", got)
	}
	if got := namesOf(records, logging.EventDiagnosis); len(got) != 1 {
		t.Errorf("the trace holds %d diagnosis records, want 1", len(got))
	}
	if got := namesOf(records, logging.EventVerifiedPass); len(got) != 1 {
		t.Errorf("the trace holds %d verification records, want 1", len(got))
	}
}

type summarizingEnvironment struct {
	fakeEnvironment
	summary *network.Summary
}

func (s *summarizingEnvironment) Summary() *network.Summary { return s.summary }

func TestTheTraceNamesEveryNamespaceTheLaboratoryCreated(t *testing.T) {
	env := &summarizingEnvironment{summary: &network.Summary{
		ID:              "nf-test",
		RouterNamespace: "nf-router-nf-test",
		Nodes: []network.SummaryNode{
			{Role: "client", Namespace: "nf-client0-nf-test"},
			{Role: "server", Namespace: "nf-server0-nf-test"},
		},
		Services: []network.SummaryService{
			{Namespace: "nf-server0-nf-test", Endpoints: []string{"icmp 10.42.0.3"}},
		},
		Balancer: &network.BalancerSummary{Namespace: "nf-balancer0-nf-test"},
	}}
	records := tracedRun(t, testPlan(t), env)

	got := namesOf(records, logging.EventNamespace)
	want := []string{"nf-client0-nf-test", "nf-server0-nf-test",
		"nf-router-nf-test", "nf-balancer0-nf-test", "nf-server0-nf-test"}
	for _, namespace := range want {
		found := false
		for _, named := range got {
			if named == namespace {
				found = true
			}
		}
		if !found {
			t.Errorf("the trace does not name the namespace %s: %v", namespace, got)
		}
	}
}

func TestAFailedRunNamesEveryStageItReached(t *testing.T) {
	env := &fakeEnvironment{fail: StateObserve}
	records := tracedRun(t, testPlan(t), env)

	for i, stage := range logging.StageEvents() {
		states := namesOf(records, stage)
		state := States()[i]
		switch state {
		case StateSetup, StateBaseline, StateFault, StateObserve, StateCleanup:
			if len(states) == 0 {
				t.Errorf("the run reached %s and the stage %q emitted no record", state, stage)
			}
		default:
			if len(states) != 0 {
				t.Errorf("the run failed in %s and the stage %q emitted %v anyway", StateObserve, stage, states)
			}
		}
	}
	if len(namesOf(records, logging.EventVerifiedFailed)) != 0 {
		t.Error("a run that never reached the verification reported a verdict on it")
	}
	if len(namesOf(records, logging.EventVerifiedPass)) != 0 {
		t.Error("a run that never reached the verification reported that it passed")
	}
}
