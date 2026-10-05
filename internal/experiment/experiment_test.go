package experiment

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/chaos"
	"github.com/netforge/netforge/internal/config"
	"github.com/netforge/netforge/internal/investigator"
	"github.com/netforge/netforge/internal/probe"
	"github.com/netforge/netforge/internal/recovery"
)

const (
	labNetwork = "10.42.0.0/16"
	client     = "client"
	router     = "router"
	serviceA   = "service-a"
	serviceB   = "service-b"
	target     = "10.42.0.3"
)

func testDocument(t *testing.T) config.Config {
	t.Helper()
	document := []byte(`
name: cascading-failure
seed: 123456
scope:
  network: 10.42.0.0/16
  namespaces: [client, router, service-a, service-b]
limits:
  max_namespaces: 8
  max_interfaces: 16
  max_duration: 5m
  max_bandwidth: 1gbps
  max_packet_rate: 100000
  max_concurrent_probes: 64
faults:
  - target: service-b
    type: packet_loss
    value: 10%
`)
	parsed, err := config.Parse(document)
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	return *parsed
}

func testObservation() Observation {
	return Observation{
		Namespace:  client,
		Target:     target,
		Port:       7000,
		Protocol:   probe.ProtocolTCP,
		Options:    probe.Options{Count: 5, Interval: 10 * time.Millisecond, Size: 64, Timeout: time.Second, Deadline: 5 * time.Second},
		AfterFault: 10 * time.Millisecond,
		Settle:     10 * time.Millisecond,
	}
}

func testPlan(t *testing.T) Plan {
	t.Helper()
	plan, err := Compile(testDocument(t), testObservation(), nil, recovery.DefaultCriteria(), DefaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return plan
}

func compile(t *testing.T, document config.Config, observation Observation, requests []recovery.Request) (Plan, error) {
	t.Helper()
	return Compile(document, observation, requests, recovery.DefaultCriteria(), DefaultLimits(), time.Now())
}

type fakeEnvironment struct {
	mu    sync.Mutex
	steps []State

	fail State

	err error

	outcomes []recovery.Outcome

	authorizations int

	tornDown int

	diagnosis investigator.Report

	broken bool

	noFaults bool
}

func (f *fakeEnvironment) record(state State) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = append(f.steps, state)
	if f.fail == state {
		if f.err != nil {
			return f.err
		}
		return apperr.IO("test.environment", "the "+string(state)+" step failed", nil)
	}
	return nil
}

func (f *fakeEnvironment) seen() []State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]State(nil), f.steps...)
}

func (f *fakeEnvironment) Setup(ctx context.Context) error { return f.record(StateSetup) }
func (f *fakeEnvironment) Teardown(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tornDown++
	f.steps = append(f.steps, StateCleanup)
	return nil
}

func (f *fakeEnvironment) measurement() probe.Measurement {
	measurement := probe.Measurement{
		Target: target, Namespace: client,
		Sent: 5, Received: 5, Loss: 0,
		AverageRTT: 2 * time.Millisecond, MinRTT: time.Millisecond, MaxRTT: 3 * time.Millisecond,
	}
	if f.broken {
		measurement.Received = 1
		measurement.Loss = 80
	}
	return measurement
}

func (f *fakeEnvironment) Baseline(ctx context.Context) ([]probe.Measurement, error) {
	if err := f.record(StateBaseline); err != nil {
		return nil, err
	}
	return []probe.Measurement{f.measurement()}, nil
}

func (f *fakeEnvironment) ApplyFaults(ctx context.Context) ([]chaos.Fault, error) {
	if err := f.record(StateFault); err != nil {
		return nil, err
	}
	if f.noFaults {
		return nil, nil
	}
	return []chaos.Fault{{ID: "fault-1", Experiment: "nf-test", Kind: chaos.KindPacketLoss, Status: chaos.StatusActive}}, nil
}

func (f *fakeEnvironment) Observe(ctx context.Context) ([]probe.Measurement, error) {
	if err := f.record(StateObserve); err != nil {
		return nil, err
	}
	return []probe.Measurement{f.measurement()}, nil
}

func (f *fakeEnvironment) Diagnose(ctx context.Context, observed []probe.Measurement) (investigator.Report, error) {
	if err := f.record(StateDiagnose); err != nil {
		return investigator.Report{}, err
	}
	if f.diagnosis.Diagnosis == "" {
		return investigator.Report{
			Diagnosis: investigator.Confirmed,
			Layer:     "network",
			Summary:   "the path to the target is impaired",
			Rule:      "loss-observed",
			Observation: investigator.Observation{
				Namespace: client,
				Target:    target,
				Failed:    probe.ProtocolTCP,
				Failure:   probe.FailureTimeout,
			},
		}, nil
	}
	return f.diagnosis, nil
}

func (f *fakeEnvironment) Recover(ctx context.Context, authorizations []recovery.Authorization) ([]recovery.Outcome, error) {
	if err := f.record(StateRecover); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.authorizations = len(authorizations)
	if len(authorizations) > 0 && f.outcomes == nil {

		f.broken = false
	}
	f.mu.Unlock()
	if f.outcomes != nil {
		return f.outcomes, nil
	}
	outcomes := make([]recovery.Outcome, 0, len(authorizations))
	for _, authorization := range authorizations {
		outcomes = append(outcomes, recovery.Outcome{
			Authorization: authorization,
			Verification:  recovery.Verification{Passed: true, Reason: "the path recovered"},
		})
	}
	return outcomes, nil
}

func (f *fakeEnvironment) Verify(ctx context.Context) ([]probe.Measurement, error) {
	if err := f.record(StateVerify); err != nil {
		return nil, err
	}
	return []probe.Measurement{f.measurement()}, nil
}

func failureState(report Report) State { return report.Failed }

func run(t *testing.T, plan Plan, env Environment) (Report, error) {
	t.Helper()
	controller, err := New(plan, env, time.Now())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return controller.Run(context.Background())
}

func TestAnExperimentWalksTheWholeLifecycle(t *testing.T) {
	env := &fakeEnvironment{}
	report, err := run(t, testPlan(t), env)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Succeeded() {
		t.Fatalf("the experiment failed in %s: %v", report.Failed, report.Err)
	}

	want := States()
	if len(report.Steps) != len(want) {
		t.Fatalf("the report holds %d steps, want %d", len(report.Steps), len(want))
	}
	for i, step := range report.Steps {
		if step.State != want[i] {
			t.Fatalf("step %d was %s, want %s", i, step.State, want[i])
		}
		if step.Failed() {
			t.Fatalf("step %s failed: %v", step.State, step.Err)
		}
	}
	if len(report.Baseline) != 1 || len(report.Observed) != 1 || len(report.Verified) != 1 {
		t.Errorf("the report holds %d baseline, %d observed and %d verified measurements, want one of each",
			len(report.Baseline), len(report.Observed), len(report.Verified))
	}
	if len(report.Faults) != 1 {
		t.Errorf("%d faults in the report, want 1", len(report.Faults))
	}
	if !report.CleanedUp || env.tornDown != 1 {
		t.Errorf("the laboratory was torn down %d times, want exactly 1", env.tornDown)
	}
}

func TestAFailedStepStillCleansUp(t *testing.T) {

	for _, state := range States() {
		if state == StateCleanup || state == StateReport {
			continue
		}
		t.Run(state.String(), func(t *testing.T) {

			env := &fakeEnvironment{fail: state}
			_ = state
			plan, err := compile(t, testDocument(t), testObservation(), []recovery.Request{{
				Action:  recovery.ActionRemoveFault,
				Target:  serviceB,
				Reason:  "the operator confirmed it",
				Consent: true,
			}})
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			report, err := run(t, plan, env)
			if err == nil {
				t.Fatal("a failing step was reported as a success")
			}
			if report.Failed != state {
				t.Errorf("the report says the experiment failed in %s, want %s", report.Failed, state)
			}
			if env.tornDown != 1 {
				t.Errorf("the laboratory was torn down %d times after a failure in %s, want 1", env.tornDown, state)
			}

			for _, step := range report.Steps {
				if step.State == state && !step.Failed() {
					t.Errorf("the step for %s is not marked as failed", state)
				}
			}
		})
	}
}

func TestACleanupFailureIsReported(t *testing.T) {
	env := &failingTeardown{}
	report, err := run(t, testPlan(t), env)
	if err == nil {
		t.Fatal("a laboratory that could not be taken apart was reported as a clean run")
	}
	if report.CleanedUp {
		t.Error("the report says the laboratory was cleaned up")
	}
	if report.Failed != StateCleanup {
		t.Errorf("the report failed in %s, want %s", report.Failed, StateCleanup)
	}
}

type failingTeardown struct{ fakeEnvironment }

func (f *failingTeardown) Teardown(ctx context.Context) error {
	f.tornDown++
	return apperr.IO("test.environment", "the namespaces are still busy", nil)
}

func TestOnlyOneStateTransitionIsLegal(t *testing.T) {
	if err := Transition(StateSetup, StateBaseline); err != nil {
		t.Errorf("setup to baseline is %v, want it allowed", err)
	}

	states := States()
	for i, from := range states {
		for j, to := range states {
			legal := j == i+1
			err := Transition(from, to)
			if legal && err != nil {
				t.Errorf("Transition(%s, %s) = %v, want it allowed", from, to, err)
			}
			if !legal && err == nil {
				t.Errorf("Transition(%s, %s) = nil, want a refusal", from, to)
			}
		}
	}
	if err := Transition(State("teleport"), StateSetup); !apperr.Is(err, apperr.KindValidation) {
		t.Errorf("a transition from a state that does not exist is %v, want a refusal", err)
	}
	if err := Transition(StateSetup, State("teleport")); !apperr.Is(err, apperr.KindValidation) {
		t.Errorf("a transition to a state that does not exist is %v, want a refusal", err)
	}
}

func TestEveryStateCanBeCleanedUpFrom(t *testing.T) {
	for _, state := range States() {
		if !state.CleansUp() {
			t.Errorf("%s has no cleanup path", state)
		}
		cleanup, err := CleanupFrom(state)
		if err != nil {
			t.Errorf("CleanupFrom(%s) = %v, want the cleanup state", state, err)
		}
		if cleanup != StateCleanup {
			t.Errorf("CleanupFrom(%s) = %s, want %s", state, cleanup, StateCleanup)
		}
	}
	if _, err := CleanupFrom(State("teleport")); err == nil {
		t.Error("a state that does not exist was cleaned up from")
	}
}

func TestTheWholeExperimentIsValidatedBeforeAnythingRuns(t *testing.T) {

	document := testDocument(t)
	env := &fakeEnvironment{}
	_, err := compile(t, document, testObservation(), []recovery.Request{{
		Action:  recovery.ActionRemoveFault,
		Target:  serviceB,
		Reason:  "operator asked for it",
		Consent: false,
	}})
	if err == nil {
		t.Fatal("an experiment with an unauthorized recovery action was compiled")
	}
	if !strings.Contains(err.Error(), "recovery actions are not all authorized") {
		t.Errorf("the refusal does not name the recovery: %v", err)
	}
	if len(env.seen()) != 0 {
		t.Errorf("the environment was used during validation: %v", env.seen())
	}
}

func TestAnInvalidObservationIsRefusedBeforeTheLaboratoryExists(t *testing.T) {
	cases := []struct {
		name    string
		change  func(*Observation)
		wantsIn string
	}{
		{"namespace outside the scope", func(o *Observation) { o.Namespace = "service-z" }, "not granted by scope.namespaces"},
		{"target outside the network", func(o *Observation) { o.Target = "10.99.0.1" }, "outside scope.network"},
		{"unknown protocol", func(o *Observation) { o.Protocol = probe.Protocol("carrier-pigeon") }, "not a protocol a laboratory measures"},
		{"no port for a protocol that uses one", func(o *Observation) { o.Port = 0 }, "port is required"},
		{"too many probes", func(o *Observation) { o.Options.Count = 10_000 }, "observation.options.count"},
		{"payload too large", func(o *Observation) { o.Options.Size = 65_535 }, "observation.options.size"},
		{"no deadline", func(o *Observation) { o.Options.Deadline = 0 }, "deadline must be positive"},
		{"no namespace", func(o *Observation) { o.Namespace = "" }, "namespace is required"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			observation := testObservation()
			testCase.change(&observation)
			_, err := compile(t, testDocument(t), observation, nil)
			if err == nil {
				t.Fatal("the observation was accepted")
			}
			if !strings.Contains(err.Error(), testCase.wantsIn) {
				t.Errorf("the refusal %q does not mention %q", err, testCase.wantsIn)
			}
		})
	}
}

func TestEveryProblemIsReportedAtOnce(t *testing.T) {

	document := testDocument(t)
	document.Name = "Not A Name"
	observation := testObservation()
	observation.Namespace = "service-z"
	observation.Target = "10.99.0.1"
	_, err := compile(t, document, observation, nil)
	if err == nil {
		t.Fatal("a document with three problems was accepted")
	}
	message := err.Error()
	for _, want := range []string{"name", "not granted by scope.namespaces", "outside scope.network"} {
		if !strings.Contains(message, want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

func TestAnExperimentThatCannotFitInItsDurationIsRefused(t *testing.T) {
	document := testDocument(t)
	observation := testObservation()

	document.Limits.MaxDuration = config.Duration(5 * time.Second)
	_, err := compile(t, document, observation, nil)
	if !apperr.Is(err, apperr.KindLimitExceeded) {
		t.Fatalf("an experiment that cannot fit is %v, want a limit refusal: %v", kindName(err), err)
	}
	if !strings.Contains(err.Error(), "max_duration") {
		t.Errorf("the refusal does not name the limit: %v", err)
	}
}

func TestTheControllerCeilingIsEnforced(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxStep = time.Second
	limits.MaxTotal = 2 * time.Second
	_, err := Compile(testDocument(t), testObservation(), nil, recovery.DefaultCriteria(), limits, time.Now())
	if !apperr.Is(err, apperr.KindLimitExceeded) {
		t.Fatalf("an experiment longer than the ceiling is %v, want a limit refusal", err)
	}
	limits = DefaultLimits()
	limits.MaxFaults = 1

	twoFaults, err := config.Parse([]byte(`
name: two-faults
scope:
  network: 10.42.0.0/16
  namespaces: [client, router, service-a, service-b]
limits:
  max_namespaces: 8
  max_interfaces: 16
  max_duration: 5m
  max_bandwidth: 1gbps
  max_packet_rate: 100000
  max_concurrent_probes: 64
faults:
  - target: service-a
    type: latency
    value: 50ms
  - target: service-b
    type: packet_loss
    value: 10%
`))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	_, err = Compile(*twoFaults, testObservation(), nil, recovery.DefaultCriteria(), limits, time.Now())
	if !apperr.Is(err, apperr.KindLimitExceeded) {
		t.Fatalf("too many faults is %v, want a limit refusal", err)
	}
}

func TestTheSeedIsCarriedThroughToTheReport(t *testing.T) {
	plan := testPlan(t)
	if plan.Seed == nil {
		t.Fatal("the plan has no seed, want the document's")
	}
	report, err := run(t, plan, &fakeEnvironment{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Seed == nil || *report.Seed != *plan.Seed {
		t.Errorf("the report carries seed %v, want %d", report.Seed, *plan.Seed)
	}
	if report.ExperimentID != plan.ExperimentID {
		t.Errorf("the report carries experiment %q, want %q", report.ExperimentID, plan.ExperimentID)
	}
}

func TestAnExperimentWithNoRecoveryVisitsTheStepAndDoesNothing(t *testing.T) {

	plan := testPlan(t)
	env := &fakeEnvironment{broken: true}
	report, err := run(t, plan, env)

	if err == nil {
		t.Fatal("a run with nothing authorized reported success on a broken path")
	}
	if failureState(report) != StateVerify {
		t.Errorf("the run failed in %q, want it to fail at the verification", failureState(report))
	}
	recover, ok := plan.StepFor(StateRecover)
	if !ok {
		t.Fatal("the plan has no recover step")
	}
	if !strings.Contains(recover.Detail, "none were authorized") {
		t.Errorf("the recover step says %q, want it to say nothing was authorized", recover.Detail)
	}
	if env.authorizations != 0 {
		t.Errorf("the recover step was given %d authorizations, want 0", env.authorizations)
	}

	if report.Recommendation.Action != recovery.ActionRemoveFault {
		t.Errorf("the recommendation is %q, want a suggestion to remove the fault", report.Recommendation.Action)
	}
	if report.Recommendation.Action.Valid() && !strings.Contains(recover.Detail, "none were authorized") {
		t.Error("the report suggested an action while the run was authorized for none")
	}
}

func TestOnlyAuthorizedActionsArePerformed(t *testing.T) {
	plan, err := compile(t, testDocument(t), testObservation(), []recovery.Request{{
		Action:  recovery.ActionRemoveFault,
		Target:  serviceB,
		Reason:  "the operator confirmed the fault is the cause",
		Consent: true,
	}})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	env := &fakeEnvironment{broken: true}
	report, err := run(t, plan, env)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if env.authorizations != 1 {
		t.Errorf("the recover step was given %d authorizations, want 1", env.authorizations)
	}
	if len(report.Recoveries) != 1 {
		t.Fatalf("%d outcomes, want 1", len(report.Recoveries))
	}
	if got := report.Recoveries[0].Authorization.Action(); got != recovery.ActionRemoveFault {
		t.Errorf("the action performed was %q, want %q", got, recovery.ActionRemoveFault)
	}
	recover, _ := plan.StepFor(StateRecover)
	if !strings.Contains(recover.Recovery, "remove_fault "+serviceB) {
		t.Errorf("the recover step reports %q, want the action it performs", recover.Recovery)
	}
	if !plan.Authorized(recovery.ActionRemoveFault, serviceB) {
		t.Error("the plan does not report the action it was authorized for")
	}
	if plan.Authorized(recovery.ActionRestartService, serviceA) {
		t.Error("the plan reports an action it was never authorized for")
	}
}

func AnAuthorizedActionThatDidNotRecoverFailsTheExperiment(t *testing.T) {
	plan, err := compile(t, testDocument(t), testObservation(), []recovery.Request{{
		Action:  recovery.ActionRemoveFault,
		Target:  serviceB,
		Reason:  "the operator confirmed it",
		Consent: true,
	}})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	env := &fakeEnvironment{broken: true, outcomes: []recovery.Outcome{{
		Authorization: plan.Recoveries[0],
		Verification:  recovery.Verification{Passed: false, Reason: "loss is 80%"},
	}}}
	report, err := run(t, plan, env)
	if err == nil {
		t.Fatal("an experiment whose recovery did not recover was reported as a success")
	}
	if report.Failed != StateRecover {
		t.Errorf("the experiment failed in %s, want %s", report.Failed, StateRecover)
	}
	if env.tornDown != 1 {
		t.Errorf("the laboratory was torn down %d times, want 1", env.tornDown)
	}
}

func TestARecoveryThatPerformedSomethingElseFailsTheExperiment(t *testing.T) {
	plan, err := compile(t, testDocument(t), testObservation(), []recovery.Request{{
		Action:  recovery.ActionRemoveFault,
		Target:  serviceB,
		Reason:  "the operator confirmed it",
		Consent: true,
	}})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	env := &fakeEnvironment{outcomes: []recovery.Outcome{{
		Authorization: plan.Recoveries[0],
		Verification:  recovery.Verification{Passed: true},
	}}}
	env.outcomes = append(env.outcomes, recovery.Outcome{
		Authorization: recovery.Authorization{},
		Verification:  recovery.Verification{Passed: true},
	})
	_, err = run(t, plan, env)
	if err == nil {
		t.Fatal("a recovery that performed an extra action was accepted")
	}
}

func TestAPlanIsFollowedInOrder(t *testing.T) {
	plan := testPlan(t)
	if len(plan.Steps) != len(States()) {
		t.Fatalf("the plan has %d steps, the lifecycle has %d", len(plan.Steps), len(States()))
	}
	for i, step := range plan.Steps {
		if step.State != States()[i] {
			t.Errorf("step %d is %s, want %s", i, step.State, States()[i])
		}
		if step.Deadline <= 0 {
			t.Errorf("step %s has no deadline", step.State)
		}
	}
	if !strings.Contains(Render(plan.Steps), "setup") {
		t.Error("the rendered plan does not name the states")
	}

	broken := plan
	broken.Steps = append([]Step(nil), plan.Steps...)
	broken.Steps[1] = Step{State: StateReport}
	if _, err := New(broken, &fakeEnvironment{}, time.Now()); err == nil {
		t.Error("a plan whose steps are out of order was accepted")
	}
	short := plan
	short.Steps = short.Steps[:3]
	if _, err := New(short, &fakeEnvironment{}, time.Now()); err == nil {
		t.Error("a plan with a truncated lifecycle was accepted")
	}
	empty := plan
	empty.Steps = nil
	if _, err := New(empty, &fakeEnvironment{}, time.Now()); err == nil {
		t.Error("a plan with no steps was accepted")
	}
	if _, err := New(plan, nil, time.Now()); !apperr.Is(err, apperr.KindInvalidArgument) {
		t.Errorf("a controller with no environment is %v, want a refusal", err)
	}
}

func TestTheControllerReportsWhereItIs(t *testing.T) {
	plan := testPlan(t)
	controller, err := New(plan, &fakeEnvironment{}, time.Now())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if controller.State() != StateSetup {
		t.Errorf("the controller starts in %s, want %s", controller.State(), StateSetup)
	}
	if controller.Plan().ExperimentID != plan.ExperimentID {
		t.Error("the controller reports a different plan from the one it was given")
	}

	if _, err := controller.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if controller.State() != StateCleanup {
		t.Errorf("the controller ended in %s, want %s", controller.State(), StateCleanup)
	}
}

func TestACancelledExperimentStillCleansUp(t *testing.T) {

	ctx, cancel := context.WithCancel(context.Background())
	env := &fakeEnvironment{}
	controller, err := New(testPlan(t), env, time.Now())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cancel()
	report, err := controller.Run(ctx)
	if !apperr.Is(err, apperr.KindCanceled) {
		t.Fatalf("a cancelled experiment is %v, want %s: %v", err, apperr.KindCanceled, err)
	}
	if env.tornDown != 1 {
		t.Errorf("the laboratory was torn down %d times after a cancellation, want 1", env.tornDown)
	}
	if !report.CleanedUp {
		t.Error("the report says a cancelled experiment was not cleaned up")
	}
}

func TestADocumentWithNoFaultsStillRunsTheFaultStep(t *testing.T) {
	document := testDocument(t)
	document.Faults = nil
	plan, err := compile(t, document, testObservation(), nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if plan.HasFaults() {
		t.Fatal("the plan has faults, want none")
	}
	step, ok := plan.StepFor(StateFault)
	if !ok {
		t.Fatal("the plan has no fault step")
	}
	if !strings.Contains(step.Detail, "declares none") {
		t.Errorf("the fault step says %q, want it to say there are no faults", step.Detail)
	}
	report, err := run(t, plan, &fakeEnvironment{noFaults: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Faults) != 0 {
		t.Errorf("%d faults in the report, want 0", len(report.Faults))
	}
}

func TestADocumentWithNoNameIsRefused(t *testing.T) {
	document := testDocument(t)
	document.Name = ""
	if _, err := compile(t, document, testObservation(), nil); err == nil {
		t.Error("a document with no name was accepted")
	}
}

func kindName(err error) string {
	kind, ok := apperr.KindOf(err)
	if !ok {
		return "untyped"
	}
	return kind.String()
}
