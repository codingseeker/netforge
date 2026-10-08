package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/codingseeker/netforge/internal/apperr"
	"github.com/codingseeker/netforge/internal/chaos"
	"github.com/codingseeker/netforge/internal/config"
	"github.com/codingseeker/netforge/internal/experiment"
	"github.com/codingseeker/netforge/internal/investigator"
	"github.com/codingseeker/netforge/internal/network"
	"github.com/codingseeker/netforge/internal/probe"
	"github.com/codingseeker/netforge/internal/recovery"
)

const phase9LabID = "nf-phase9"

var (
	phase9Router  = "nf-router-" + phase9LabID
	phase9Client  = "nf-client0-" + phase9LabID
	phase9Server  = "nf-server0-" + phase9LabID
	phase9Address = "10.42.0.6"
)

func experimentSpec() network.Spec {
	spec := labSpec(phase9LabID)
	spec.Chaos = chaos.Limits{
		MaxFaults: 8,

		MaxDuration:  5 * time.Minute,
		MaxLatency:   5 * time.Second,
		MaxBandwidth: 10_000_000,
		MaxQueue:     1024,
		MaxGap:       100,
	}
	spec.Probes = probe.Limits{
		MaxProbes: 100, MaxRate: 200, MaxSize: 1400,

		MaxDuration: 90 * time.Second, MaxConcurrent: 2,
	}
	return granted(spec)
}

func buildLab(t *testing.T) func(context.Context, network.Spec) (*network.Lab, error) {
	t.Helper()
	return func(ctx context.Context, spec network.Spec) (*network.Lab, error) {
		spec.ID = phase9LabID
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

func observationFor() experiment.Observation {
	return experiment.Observation{
		Namespace:  phase9Client,
		Target:     phase9Address,
		Protocol:   probe.ProtocolICMP,
		AfterFault: 200 * time.Millisecond,
		Settle:     200 * time.Millisecond,
		Options: probe.Options{
			Count:    12,
			Size:     64,
			Timeout:  200 * time.Millisecond,
			Deadline: 15 * time.Second,
		},
	}
}

func experimentDocument(t *testing.T, name string) config.Config {
	t.Helper()
	raw := "name: " + name + "\n" +
		"scope:\n  network: 10.42.0.0/24\n  namespaces: [" +
		phase9Router + ", " + phase9Client + ", " + phase9Server + "]\n" +
		"limits:\n  max_namespaces: 8\n  max_interfaces: 16\n  max_duration: 5m\n" +
		"  max_bandwidth: 1Gbps\n  max_packet_rate: 100000\n  max_concurrent_probes: 64\n" +

		"faults:\n  - target: " + phase9Server + "\n    type: packet_loss\n    value: 90%\n"
	document, err := config.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	return *document
}

func runExperiment(t *testing.T, document config.Config, requests []recovery.Request) (experiment.Report, experiment.LabEnvironment) {
	t.Helper()
	plan, err := experiment.Compile(document, observationFor(), requests,
		recovery.DefaultCriteria(), experiment.DefaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	environment, err := experiment.NewLabEnvironment(plan, experimentSpec(), buildLab(t))
	if err != nil {
		t.Fatalf("NewLabEnvironment: %v", err)
	}
	controller, err := experiment.New(plan, environment, time.Now())
	if err != nil {
		t.Fatalf("experiment.New: %v", err)
	}
	report, _ := controller.Run(context.Background())
	return report, *environment
}

func TestAnExperimentMeasuresAHealthyPathThenAnImpairedOne(t *testing.T) {
	document := experimentDocument(t, "experiment-impaired")
	plan, err := experiment.Compile(document, observationFor(), nil,
		recovery.DefaultCriteria(), experiment.DefaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !plan.HasFaults() {
		t.Fatal("the plan claims no faults, want the document's packet loss")
	}

	report, environment := runExperiment(t, document, nil)
	if report.Succeeded() {
		t.Error("the report says the experiment succeeded although nothing repaired the path")
	}
	if report.Failed != experiment.StateVerify && report.Failed != experiment.StateRecover {
		t.Errorf("the experiment failed in %q, want the verification to judge the repair", report.Failed)
	}

	want := experiment.States()

	failed := -1
	for i, step := range report.Steps {
		if step.State == report.Failed {
			failed = i
			break
		}
	}
	if report.Failed == "" || failed < 0 {
		t.Fatalf("the report says the run failed in %q, but no step recorded it", report.Failed)
	}
	for i := 0; i < failed; i++ {
		if report.Steps[i].State != want[i] {
			t.Fatalf("step %d was %s, want %s", i, report.Steps[i].State, want[i])
		}
	}
	last := report.Steps[len(report.Steps)-1]
	if last.State != experiment.StateCleanup {
		t.Errorf("the last step was %s, want the cleanup to be the last thing to happen", last.State)
	}
	if report.Failed != experiment.StateVerify {
		t.Errorf("the run failed in %q, want the verification to be what caught the unrepaired path",
			report.Failed)
	}

	if len(report.Baseline) == 0 {
		t.Fatal("the report holds no baseline measurement")
	}
	if len(report.Observed) == 0 {
		t.Fatal("the report holds no observation")
	}

	if report.Observed[0].Loss <= report.Baseline[0].Loss {
		t.Errorf("the impaired path lost %.1f%% of its probes and the healthy one lost %.1f%%, "+
			"want the fault to show", report.Observed[0].Loss, report.Baseline[0].Loss)
	}
	if !report.CleanedUp {
		t.Error("the laboratory was not taken apart after a failed experiment")
	}

	if lab := environment.Lab(); lab != nil {
		if remaining := lab.Chaos().List(); len(remaining) != 0 {
			t.Errorf("the laboratory still holds %d faults after the experiment", len(remaining))
		}
	}
}

func TestAnExperimentWithoutConsentDiagnosesButDoesNotRepair(t *testing.T) {

	report, _ := runExperiment(t, experimentDocument(t, "experiment-unconsented"), nil)

	switch report.Diagnosis.Diagnosis {
	case investigator.Confirmed, investigator.Probable, investigator.Unknown:
	default:
		t.Errorf("the diagnosis is %q, which is not a classification", report.Diagnosis.Diagnosis)
	}
	if report.Diagnosis.Layer != "network" {
		t.Errorf("the diagnosis is at layer %q, want the network layer for packet loss", report.Diagnosis.Layer)
	}
	if report.Diagnosis.Summary == "" {
		t.Error("the diagnosis carries no summary, so the report says nothing about what it saw")
	}
	if len(report.Recoveries) != 0 {
		t.Errorf("the report holds %d recoveries, want none: nothing was authorized", len(report.Recoveries))
	}

	if report.Recommendation.Action != "" {
		scope, err := recovery.NewScope(report.ExperimentID, "10.42.0.0/24",
			[]string{phase9Router, phase9Client, phase9Server})
		if err != nil {
			t.Fatalf("NewScope: %v", err)
		}
		if !report.Recommendation.Usable(scope) {
			t.Errorf("the recommendation %q is not usable within the experiment's own scope",
				report.Recommendation.Action)
		}
	}
	if report.Succeeded() {
		t.Error("the experiment succeeded although nothing repaired the path")
	}
	if !report.CleanedUp {
		t.Error("the laboratory was not taken apart")
	}
}

func TestAnExperimentWithAnAuthorizedRecoveryVerifiesItsRepair(t *testing.T) {
	requests := []recovery.Request{{
		Action:  recovery.ActionRemoveFault,
		Target:  phase9Server,
		Reason:  "the operator authorized removing the fault this experiment declared",
		Consent: true,
	}}
	document := experimentDocument(t, "experiment-recovered")
	plan, err := experiment.Compile(document, observationFor(), requests,
		recovery.DefaultCriteria(), experiment.DefaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !plan.Authorized(recovery.ActionRemoveFault, phase9Server) {
		t.Fatal("the plan does not carry the authorization it was given")
	}

	report, _ := runExperiment(t, document, requests)

	if len(report.Recoveries) != 1 {
		t.Fatalf("the recovery performed %d actions, want the one that was authorized", len(report.Recoveries))
	}
	outcome := report.Recoveries[0]
	if outcome.Err != nil {
		t.Errorf("the authorized recovery failed: %v", outcome.Err)
	}
	if outcome.Verification.Measurement.Sent == 0 {
		t.Error("the recovery was not verified by a measurement")
	}
	if outcome.Verification.Reason == "" {
		t.Error("the verification carries no verdict, so nothing said whether the repair worked")
	}
	if outcome.Verification.Measurement.Loss >= report.Observed[0].Loss {
		t.Errorf("the verified loss is %.1f%% and the impaired measurement was %.1f%%, "+
			"want the fault's removal to show in the measurement",
			outcome.Verification.Measurement.Loss, report.Observed[0].Loss)
	}
	if report.Failed != "" {
		t.Errorf("the experiment failed in %s: %v, want the repair to be verified", report.Failed, report.Err)
	}
	if !report.CleanedUp {
		t.Error("the laboratory was not taken apart")
	}
}

func TestAnExperimentRefusesToRunWithoutAnObservationItCanMake(t *testing.T) {
	document := experimentDocument(t, "experiment-unobserved")

	observation := observationFor()
	observation.Target = "10.99.0.3"

	_, err := experiment.Compile(document, observation, nil,
		recovery.DefaultCriteria(), experiment.DefaultLimits(), time.Now())
	if err == nil {
		t.Fatal("an observation of an address outside the scope was compiled")
	}
	if !strings.Contains(err.Error(), "outside scope.network") {
		t.Errorf("the refusal %q does not name the network the address is outside", err)
	}
}

func TestAnExperimentRefusesARequestItWasNotGiven(t *testing.T) {
	document := experimentDocument(t, "experiment-unrequested")

	requests := []recovery.Request{{
		Action:  recovery.ActionRestartService,
		Target:  phase9Server,
		Reason:  "the operator authorized this",
		Consent: true,
	}, {
		Action:  recovery.ActionRemoveFault,
		Target:  "service-z",
		Reason:  "not granted by the document",
		Consent: true,
	}}
	_, err := experiment.Compile(document, observationFor(), requests,
		recovery.DefaultCriteria(), experiment.DefaultLimits(), time.Now())
	if err == nil {
		t.Fatal("a request for a namespace outside the scope was compiled")
	}
	if !apperr.Is(err, apperr.KindPermission) {
		t.Errorf("the refusal %v is a %s, want a permission refusal", err, kindNameOf(err))
	}
	if !strings.Contains(err.Error(), "service-z") {
		t.Errorf("the refusal %q does not name the namespace it refused", err)
	}
}

func TestALaboratoryIsBuiltFromTheDocumentScope(t *testing.T) {
	document := experimentDocument(t, "experiment-scope")
	plan, err := experiment.Compile(document, observationFor(), nil,
		recovery.DefaultCriteria(), experiment.DefaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	for _, namespace := range plan.Scope.Namespaces {
		if namespace == "" {
			t.Fatal("the plan grants an empty namespace name")
		}
	}
	environment, err := experiment.NewLabEnvironment(plan, experimentSpec(), buildLab(t))
	if err != nil {
		t.Fatalf("NewLabEnvironment: %v", err)
	}
	controller, err := experiment.New(plan, environment, time.Now())
	if err != nil {
		t.Fatalf("experiment.New: %v", err)
	}
	report, _ := controller.Run(context.Background())
	if report.Failed == experiment.StateSetup {
		t.Fatalf("the laboratory could not be built from the document's own scope: %v", report.Err)
	}
	if lab := environment.Lab(); lab != nil {
		for _, name := range lab.Names() {
			granted := false
			for _, allowed := range plan.Scope.Namespaces {
				if allowed == name {
					granted = true
					break
				}
			}
			if !granted {
				t.Errorf("the laboratory holds namespace %q, which the document does not grant", name)
			}
		}
	}
}

func kindNameOf(err error) string {
	kind, ok := apperr.KindOf(err)
	if !ok {
		return "untyped"
	}
	return kind.String()
}
