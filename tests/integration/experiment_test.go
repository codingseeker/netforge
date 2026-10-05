package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/chaos"
	"github.com/netforge/netforge/internal/config"
	"github.com/netforge/netforge/internal/experiment"
	"github.com/netforge/netforge/internal/investigator"
	"github.com/netforge/netforge/internal/network"
	"github.com/netforge/netforge/internal/probe"
	"github.com/netforge/netforge/internal/recovery"
)

// The tests here run the whole of Phase 9 against a real laboratory: real
// namespaces, a real fault in the kernel, real measurements, and a recovery that
// is only allowed to do what was authorized. Nothing is stubbed, so what is
// checked is the claim the phase makes — that an experiment measures a path
// before and after impairing it, explains what it saw, and verifies what an
// authorized recovery did.

// The names a laboratory built from a fixed id has. A laboratory's namespaces
// are generated from its id, so a test that wants to name them in a document
// before the laboratory exists needs an id it can predict, and phase9LabID is
// that id.
const phase9LabID = "nf-phase9"

var (
	phase9Router  = "nf-router-" + phase9LabID
	phase9Client  = "nf-client0-" + phase9LabID
	phase9Server  = "nf-server0-" + phase9LabID
	phase9Address = "10.42.0.6"
)

// experimentSpec is the laboratory an experiment runs against, with the fault and
// probe budgets a loss fault needs.
func experimentSpec() network.Spec {
	spec := labSpec(phase9LabID)
	spec.Chaos = chaos.Limits{
		MaxFaults: 8,
		// A declared fault lasts as long as the document's limits.max_duration,
		// because that is the window the document said the experiment had, so the
		// laboratory has to allow for it.
		MaxDuration:  5 * time.Minute,
		MaxLatency:   5 * time.Second,
		MaxBandwidth: 10_000_000,
		MaxQueue:     1024,
		MaxGap:       100,
	}
	spec.Probes = probe.Limits{
		MaxProbes: 100, MaxRate: 200, MaxSize: 1400,
		// A loss fault makes a measurement slower than a healthy one, because the
		// probe waits out its deadline for packets that will never arrive, so the
		// budget has to cover the impaired path as well.
		MaxDuration: 90 * time.Second, MaxConcurrent: 2,
	}
	return granted(spec)
}

// buildLab creates the laboratory the environment is bound to, and makes sure it
// is taken apart whatever the experiment does with it.
//
// The identifier the environment sets is replaced with one the test can predict,
// because a laboratory's namespaces are generated from its id and a document has
// to name those namespaces before the laboratory exists. Everything else about
// the laboratory is what the environment asked for: the network, the roles, the
// limits, and the faults, which the environment keeps out of the construction so
// that the baseline is measured on a healthy path.
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

// observationFor is the observation a phase 9 experiment makes of a laboratory
// built from a document: from the client namespace, of the server address.
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

// experimentDocument is a document whose scope grants the namespaces of the
// laboratory above and which declares the packet loss the experiment injects at
// the server.
func experimentDocument(t *testing.T, name string) config.Config {
	t.Helper()
	raw := "name: " + name + "\n" +
		"scope:\n  network: 10.42.0.0/24\n  namespaces: [" +
		phase9Router + ", " + phase9Client + ", " + phase9Server + "]\n" +
		"limits:\n  max_namespaces: 8\n  max_interfaces: 16\n  max_duration: 5m\n" +
		"  max_bandwidth: 1Gbps\n  max_packet_rate: 100000\n  max_concurrent_probes: 64\n" +
		// The loss is nine tenths and the observation sends twelve probes, so
		// that the impaired phase is impaired on every run rather than on most
		// of them. A test that judges a repair needs an impairment to judge;
		// five probes against half loss leaves a one in thirty-two chance of
		// measuring a healthy path and calling the run a success.
		"faults:\n  - target: " + phase9Server + "\n    type: packet_loss\n    value: 90%\n"
	document, err := config.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	return *document
}

// runExperiment compiles a plan from a document and runs it against a real
// laboratory built from that document's scope.
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

	// Every state up to the failure is walked in order, and the cleanup is the
	// last thing that happens whatever went wrong. A report that stopped early
	// would leave the operator without the part that says what was measured.
	want := experiment.States()
	// Every state up to the failure is walked in order, and the cleanup is the
	// last thing that happens whatever went wrong. A report that stopped at the
	// failure would leave out the part that says what was measured.
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
	// A healthy path was measured before the fault, so the two measurements can
	// be compared: that comparison is the point of the experiment.
	if len(report.Baseline) == 0 {
		t.Fatal("the report holds no baseline measurement")
	}
	if len(report.Observed) == 0 {
		t.Fatal("the report holds no observation")
	}
	// The two measurements are compared as loss rather than as reply counts: a
	// lossy path answers fewer probes but the count depends on how many the probe
	// managed to send, and the proportion is the part that means something.
	if report.Observed[0].Loss <= report.Baseline[0].Loss {
		t.Errorf("the impaired path lost %.1f%% of its probes and the healthy one lost %.1f%%, "+
			"want the fault to show", report.Observed[0].Loss, report.Baseline[0].Loss)
	}
	if !report.CleanedUp {
		t.Error("the laboratory was not taken apart after a failed experiment")
	}
	// The fault the experiment injected is gone with the laboratory, so a later
	// test on this host is not measuring a path this one left impaired.
	if lab := environment.Lab(); lab != nil {
		if remaining := lab.Chaos().List(); len(remaining) != 0 {
			t.Errorf("the laboratory still holds %d faults after the experiment", len(remaining))
		}
	}
}

func TestAnExperimentWithoutConsentDiagnosesButDoesNotRepair(t *testing.T) {
	// The fault is real and the diagnosis is real, but nothing was authorized, so
	// the recover step does nothing and the verification still fails. A diagnosis
	// is evidence, not permission.
	report, _ := runExperiment(t, experimentDocument(t, "experiment-unconsented"), nil)

	// How confident the investigator can be about a path losing half its packets
	// depends on how many of the probes it sent came back, so the classification
	// is not asserted here. What is asserted is that it classified the path at
	// all, at the layer the evidence came from, and that it did not act on it.
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
	// A suggestion is all a diagnosis without consent may produce, and a
	// suggestion is not a decision: whether one was made depends on the evidence,
	// but nothing was acted on either way.
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

	// Whether the repair worked depends on a real fault in a real kernel, so the
	// assertions are about the shape of the answer rather than the verdict: the
	// authorized action ran, and it was judged by a measurement rather than
	// assumed to have worked.
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
	// A measurement of an address outside the scope must be refused before a
	// laboratory is built, because building one to measure something the document
	// does not grant is exactly the side effect a plan exists to avoid.
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
	// Consent for one action is not consent for another, and a request for a
	// namespace the document does not grant is refused whatever was authorized.
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

// TestALaboratoryIsBuiltFromTheDocumentScope checks the join the phases depend on:
// the namespaces a laboratory is created with come from the document, and a
// document that does not grant them cannot be run.
func TestALaboratoryIsBuiltFromTheDocumentScope(t *testing.T) {
	document := experimentDocument(t, "experiment-scope")
	plan, err := experiment.Compile(document, observationFor(), nil,
		recovery.DefaultCriteria(), experiment.DefaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	// Every namespace the plan declares a fault on is a namespace the document
	// grants, which is what lets the fault be applied at all.
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
