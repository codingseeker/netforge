package recovery

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/investigator"
	"github.com/netforge/netforge/internal/probe"
)

const (
	labNetwork = "10.42.0.0/16"
	client     = "client"
	router     = "router"
	serviceA   = "service-a"
	outOfScope = "10.99.0.1"
)

func testScope(t *testing.T) Scope {
	t.Helper()
	scope, err := NewScope("nf-test-0001", labNetwork, []string{client, router, serviceA})
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	return scope
}

type fakeExecutor struct {
	mu    sync.Mutex
	calls []string
	fail  string
	extra func(call string) error
}

func (f *fakeExecutor) record(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	if f.fail != "" && call == f.fail {
		return apperr.IO("test.executor", "the laboratory refused "+call, nil)
	}
	if f.extra != nil {
		return f.extra(call)
	}
	return nil
}

func (f *fakeExecutor) done() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeExecutor) RemoveFault(_ context.Context, ns string) error {
	return f.record("remove_fault:" + ns)
}
func (f *fakeExecutor) RestoreRoute(_ context.Context, ns, target string) error {
	return f.record("restore_route:" + ns + ":" + target)
}
func (f *fakeExecutor) RestartService(_ context.Context, ns string) error {
	return f.record("restart_service:" + ns)
}
func (f *fakeExecutor) RemoveBackend(_ context.Context, ns, target string) error {
	return f.record("remove_backend:" + ns + ":" + target)
}
func (f *fakeExecutor) RestoreBackend(_ context.Context, ns, target string) error {
	return f.record("restore_backend:" + ns + ":" + target)
}
func (f *fakeExecutor) DrainConnection(_ context.Context, ns, target string) error {
	return f.record("drain_connection:" + ns + ":" + target)
}
func (f *fakeExecutor) ReEnableBackend(_ context.Context, ns, target string) error {
	return f.record("re_enable_backend:" + ns + ":" + target)
}

type fakeProber struct {
	mu          sync.Mutex
	measurement probe.Measurement
	err         error
	calls       int
}

func (f *fakeProber) Measure(_ context.Context, namespace, target string, _ probe.Options) (probe.Measurement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return probe.Measurement{}, f.err
	}
	measurement := f.measurement
	if measurement.Target == "" {
		measurement.Target = target
	}
	if measurement.Namespace == "" {
		measurement.Namespace = namespace
	}
	return measurement, nil
}

func (f *fakeProber) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func kindName(err error) string {
	kind, ok := apperr.KindOf(err)
	if !ok {
		return "untyped"
	}
	return kind.String()
}

func healthy() probe.Measurement {
	return probe.Measurement{
		Sent:       5,
		Received:   5,
		Loss:       0,
		AverageRTT: 2 * time.Millisecond,
		MinRTT:     time.Millisecond,
		MaxRTT:     3 * time.Millisecond,
	}
}

func consented(action Action, target, from string) Request {
	return Request{
		Action:  action,
		Target:  target,
		From:    from,
		Reason:  "operator confirmed the backend is gone",
		Consent: true,
	}
}

func TestARecoveryNeedsExplicitAuthorization(t *testing.T) {
	scope := testScope(t)
	request := consented(ActionRemoveFault, serviceA, "")
	request.Consent = false

	_, err := New(scope, []Request{request}, DefaultCriteria(), DefaultLimits(), time.Now())
	if err == nil {
		t.Fatal("a recovery without consent was authorized")
	}
	if !apperr.Is(err, apperr.KindPermission) {
		t.Errorf("a refusal for missing consent is %s, want %s", kindName(err), apperr.KindPermission)
	}
	if !strings.Contains(err.Error(), "explicit authorization") {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}
}

func TestADiagnosisCannotAuthorizeItself(t *testing.T) {
	report := investigator.Report{
		Diagnosis: investigator.Confirmed,
		Layer:     "routing",
		Summary:   "the route to 10.42.0.9 is missing",
		Rule:      "route-missing",
		Observation: investigator.Observation{
			Namespace: client,
			Target:    "10.42.0.9",
			Failed:    probe.ProtocolTCP,
			Failure:   probe.FailureUnreachable,
		},
	}
	recommendation := Propose(report)
	if !recommendation.Usable(testScope(t)) {
		t.Fatalf("a confirmed routing failure should be usable, got %q", recommendation.Describe())
	}
	if recommendation.Action != ActionRestoreRoute || recommendation.Target != "10.42.0.9" {
		t.Errorf("the suggestion is %s on %s, want %s on 10.42.0.9",
			recommendation.Action, recommendation.Target, ActionRestoreRoute)
	}

	request := recommendation.Requested(client)
	if request.Consent {
		t.Fatal("a recommendation produced a consented request")
	}
	_, err := New(testScope(t), []Request{request}, DefaultCriteria(), DefaultLimits(), time.Now())
	if !apperr.Is(err, apperr.KindPermission) {
		t.Errorf("the unconsented request is %s, want %s", kindName(err), apperr.KindPermission)
	}
}

func TestAnUnknownDiagnosisSuggestsNothing(t *testing.T) {
	report := investigator.Report{
		Diagnosis: investigator.Unknown,
		Layer:     "routing",
		Summary:   "the evidence does not separate the explanations",
		Observation: investigator.Observation{
			Namespace: client,
			Target:    "10.42.0.9",
			Failed:    probe.ProtocolTCP,
		},
	}
	if recommendation := Propose(report); recommendation.Action != "" {
		t.Errorf("an unknown diagnosis suggested %s, want nothing", recommendation.Action)
	}
	if description := (Recommendation{}).Describe(); !strings.Contains(description, "no recovery is suggested") {
		t.Errorf("an empty recommendation describes itself as %q", description)
	}
}

func TestAnAddressOutsideTheScopeIsRefused(t *testing.T) {
	_, err := New(testScope(t),
		[]Request{consented(ActionRestoreRoute, outOfScope, client)},
		DefaultCriteria(), DefaultLimits(), time.Now())
	if !apperr.Is(err, apperr.KindPermission) {
		t.Fatalf("an out-of-scope target is %s, want %s: %v", kindName(err), apperr.KindPermission, err)
	}
	if !strings.Contains(err.Error(), "outside scope.network") {
		t.Errorf("the refusal does not name the scope: %v", err)
	}
}

func TestANamespaceOutsideTheScopeIsRefused(t *testing.T) {
	_, err := New(testScope(t),
		[]Request{consented(ActionRestartService, "service-z", "")},
		DefaultCriteria(), DefaultLimits(), time.Now())
	if !apperr.Is(err, apperr.KindPermission) {
		t.Fatalf("an out-of-scope namespace is %s, want %s: %v", kindName(err), apperr.KindPermission, err)
	}
}

func TestAnUnknownActionIsRefused(t *testing.T) {
	_, err := New(testScope(t),
		[]Request{consented(Action("delete_everything"), serviceA, "")},
		DefaultCriteria(), DefaultLimits(), time.Now())
	if !apperr.Is(err, apperr.KindUnsupported) {
		t.Fatalf("an unknown action is %s, want %s: %v", kindName(err), apperr.KindUnsupported, err)
	}
	if !strings.Contains(err.Error(), "remove_fault") {
		t.Errorf("the refusal does not list the allowed actions: %v", err)
	}
}

func TestEveryAllowedActionHasAnExecutor(t *testing.T) {

	executor := &fakeExecutor{}
	prober := &fakeProber{measurement: healthy()}
	requests := []Request{
		consented(ActionRemoveFault, serviceA, ""),
		consented(ActionRestoreRoute, "10.42.0.9", client),
		consented(ActionRestartService, serviceA, ""),
		consented(ActionRemoveBackend, "10.42.0.2", client),
		consented(ActionRestoreBackend, "10.42.0.2", client),
		consented(ActionDrainConnection, "10.42.0.2", client),
		consented(ActionReEnableBackend, "10.42.0.2", client),
	}
	recovery, err := New(testScope(t), requests, DefaultCriteria(), DefaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(recovery.Authorizations()) != len(Actions()) {
		t.Fatalf("%d actions were authorized, the phase allows %d", len(recovery.Authorizations()), len(Actions()))
	}
	outcomes, err := recovery.Execute(context.Background(), executor, prober)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(outcomes) != len(requests) {
		t.Fatalf("%d outcomes, want %d", len(outcomes), len(requests))
	}
	for i, outcome := range outcomes {
		if !outcome.Passed() {
			t.Errorf("action %d (%s) did not pass: %v", i, outcome.Authorization, outcome.Err)
		}
	}
	if got := len(executor.done()); got != len(requests) {
		t.Errorf("the executor was called %d times, want %d", got, len(requests))
	}
	if prober.count() != len(requests) {
		t.Errorf("the prober was called %d times, want one verification per action (%d)", prober.count(), len(requests))
	}
}

func TestARecoveryStopsAtTheFirstFailure(t *testing.T) {
	executor := &fakeExecutor{fail: "restart_service:service-a"}
	prober := &fakeProber{measurement: healthy()}
	recovery, err := New(testScope(t), []Request{
		consented(ActionRemoveFault, serviceA, ""),
		consented(ActionRestartService, serviceA, ""),
		consented(ActionRemoveBackend, "10.42.0.2", client),
	}, DefaultCriteria(), DefaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	outcomes, err := recovery.Execute(context.Background(), executor, prober)
	if err == nil {
		t.Fatal("a failing action was reported as a success")
	}
	if len(outcomes) != 2 {
		t.Errorf("%d outcomes were returned, want the two that ran", len(outcomes))
	}
	if outcomes[1].Err == nil {
		t.Error("the failing action has no error in its outcome")
	}
	if got := executor.done(); len(got) != 2 {
		t.Errorf("the executor ran %v, want it to stop after the second action", got)
	}
}

func TestAnActionThatDoesNotRecoverIsAFailure(t *testing.T) {

	executor := &fakeExecutor{}
	broken := healthy()
	broken.Received = 2
	prober := &fakeProber{measurement: broken}
	recovery, err := New(testScope(t),
		[]Request{consented(ActionRemoveFault, serviceA, "")},
		DefaultCriteria(), DefaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	outcomes, err := recovery.Execute(context.Background(), executor, prober)
	if err == nil {
		t.Fatal("a path that still loses packets was reported as recovered")
	}
	if len(outcomes) != 1 || outcomes[0].Passed() {
		t.Fatalf("the outcome is %+v, want a failure carrying the measurement", outcomes)
	}
	if outcomes[0].Verification.Reason == "" {
		t.Error("the failed verification does not say what it measured")
	}
	if !strings.Contains(err.Error(), "did not recover") {
		t.Errorf("the error does not say the path did not recover: %v", err)
	}
}

func testAnUnmeasurablePathIsNotAVerification(t *testing.T) {
	executor := &fakeExecutor{}
	prober := &fakeProber{err: apperr.Timeout("test.prober", "the answer never arrived", nil)}
	recovery, err := New(testScope(t),
		[]Request{consented(ActionRemoveFault, serviceA, "")},
		DefaultCriteria(), DefaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	outcomes, err := recovery.Execute(context.Background(), executor, prober)
	if !apperr.Is(err, apperr.KindTimeout) {
		t.Fatalf("an unmeasurable path is %s, want %s: %v", kindName(err), apperr.KindTimeout, err)
	}
	if outcomes[0].Verification.Passed {
		t.Error("a path that could not be measured was verified as recovered")
	}
}

func TestTheWholeRequestSetIsValidatedBeforeAnyAction(t *testing.T) {

	executor := &fakeExecutor{}
	prober := &fakeProber{measurement: healthy()}
	_, err := New(testScope(t), []Request{
		consented(ActionRemoveFault, serviceA, ""),
		consented(ActionRestoreRoute, outOfScope, client),
	}, DefaultCriteria(), DefaultLimits(), time.Now())
	if err == nil {
		t.Fatal("a set with an invalid second action was accepted")
	}
	if !strings.Contains(err.Error(), "requests[1]") {
		t.Errorf("the refusal does not say which request was invalid: %v", err)
	}
	if got := executor.done(); len(got) != 0 {
		t.Errorf("the executor ran %v before validation finished", got)
	}
	if prober.count() != 0 {
		t.Error("a measurement was taken before validation finished")
	}
}

func TestARepeatedActionIsRefused(t *testing.T) {
	_, err := New(testScope(t), []Request{
		consented(ActionRemoveFault, serviceA, ""),
		consented(ActionRemoveFault, serviceA, ""),
	}, DefaultCriteria(), DefaultLimits(), time.Now())
	if !apperr.Is(err, apperr.KindValidation) {
		t.Fatalf("a repeated action is %s, want %s: %v", kindName(err), apperr.KindValidation, err)
	}
}

func TestAnAddressActionNeedsAVerificationNamespace(t *testing.T) {
	_, err := New(testScope(t),
		[]Request{{Action: ActionRemoveBackend, Target: "10.42.0.2", Reason: "gone", Consent: true}},
		DefaultCriteria(), DefaultLimits(), time.Now())
	if !apperr.Is(err, apperr.KindValidation) {
		t.Fatalf("a missing verification namespace is %s, want %s: %v", kindName(err), apperr.KindValidation, err)
	}
	if !strings.Contains(err.Error(), "from is required") {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}
}

func TestAVerificationNamespaceOutsideTheScopeIsRefused(t *testing.T) {
	_, err := New(testScope(t),
		[]Request{{Action: ActionRemoveBackend, Target: "10.42.0.2", From: "service-z", Reason: "gone", Consent: true}},
		DefaultCriteria(), DefaultLimits(), time.Now())
	if !apperr.Is(err, apperr.KindPermission) {
		t.Fatalf("an out-of-scope verification namespace is %s, want %s: %v",
			kindName(err), apperr.KindPermission, err)
	}
}

func TestAReasonIsRequired(t *testing.T) {
	request := consented(ActionRemoveFault, serviceA, "")
	request.Reason = ""
	_, err := New(testScope(t), []Request{request}, DefaultCriteria(), DefaultLimits(), time.Now())
	if !apperr.Is(err, apperr.KindValidation) {
		t.Fatalf("a missing reason is %s, want %s: %v", kindName(err), apperr.KindValidation, err)
	}
}

func TestTheActionSetIsClosedAndListed(t *testing.T) {

	if len(Actions()) != 7 {
		t.Errorf("%d actions are allowed, the phase names 7", len(Actions()))
	}
	for _, action := range Actions() {
		if !action.Valid() {
			t.Errorf("%s is listed but not valid", action)
		}
	}
	if Action("").Valid() || Action("remove-fault").Valid() {
		t.Error("an empty or misspelled action was accepted")
	}
	if !ActionRestartService.Destructive() || !ActionRemoveBackend.Destructive() {
		t.Error("restarting a service and removing a backend are both destructive")
	}
	if ActionRemoveFault.Destructive() {
		t.Error("removing a fault is not destructive: it restores the configured state")
	}
}

func TestAnAuthorizationIsOnlyIssuedForItsOwnExperiment(t *testing.T) {

	first := testScope(t)
	second, err := NewScope("nf-test-0002", labNetwork, []string{client, router, serviceA})
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	if first.Experiment == second.Experiment {
		t.Fatal("the two scopes share an experiment id")
	}
	authorization, err := Authorize(first, consented(ActionRemoveFault, serviceA, ""), DefaultCriteria(), DefaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if authorization.Scope().Experiment != first.Experiment {
		t.Errorf("the authorization carries experiment %q, want %q", authorization.Scope().Experiment, first.Experiment)
	}
}

func TestTheScopeRefusesAmbiguousAddresses(t *testing.T) {
	scope := testScope(t)

	for _, target := range []string{"10.42.0.2:7000", "10.42.0.2:80", "10.42.0.2"} {
		if err := scope.AllowsTarget(ActionRemoveBackend, target); err != nil {
			t.Errorf("%q was refused as a backend: %v", target, err)
		}
	}

	for _, target := range []string{"10.42.0.2%eth0", "service-a", ":7000", "10.42.0.2:", ""} {
		if err := scope.AllowsTarget(ActionRemoveBackend, target); err == nil {
			t.Errorf("%q was accepted as a backend address", target)
		}
	}

	for _, target := range []string{"10.99.0.2:7000", "10.99.0.2"} {
		err := scope.AllowsTarget(ActionRemoveBackend, target)
		if err == nil || !strings.Contains(err.Error(), "outside scope.network") {
			t.Errorf("%q is %v, want a refusal naming the network", target, err)
		}
	}

	for _, target := range []string{labNetwork, "10.42.0.0/24", "10.42.0.9"} {
		if err := scope.AllowsTarget(ActionRestoreRoute, target); err != nil {
			t.Errorf("%q was refused as a route destination: %v", target, err)
		}
	}

	for _, target := range []string{"10.0.0.0/8", "0.0.0.0/0", "2001:db8::/32", "10.99.0.0/24"} {
		if err := scope.AllowsTarget(ActionRestoreRoute, target); err == nil {
			t.Errorf("%q was accepted as a route destination", target)
		}
	}

	if err := scope.AllowsTarget(ActionRestartService, "10.42.0.2"); err == nil {
		t.Error("an address was accepted as a namespace")
	}
	if err := scope.AllowsTarget(ActionRemoveFault, "service-z"); err == nil {
		t.Error("a namespace outside the scope was accepted")
	}

	for _, target := range []string{"10.42.0.2:7000", "10.42.0.0/24", "10.42.0.2%eth0"} {
		if err := scope.AllowsObservation(target); err == nil {
			t.Errorf("%q was accepted as an observation target", target)
		}
	}
	if err := scope.AllowsObservation("10.42.0.2"); err != nil {
		t.Errorf("an in-network address was refused as an observation: %v", err)
	}
	if err := scope.AllowsObservation("10.99.0.2"); err == nil || !strings.Contains(err.Error(), "outside scope.network") {
		t.Errorf("an address outside the network is %v, want a refusal naming the network", err)
	}

	if !scope.AllowsNamespace(serviceA) || scope.AllowsNamespace("service-z") {
		t.Error("the namespace grant does not match the scope")
	}
	if !strings.Contains(scope.String(), "namespaces=[client,router,service-a]") {
		t.Errorf("the scope renders as %q", scope.String())
	}
}

func TestAScopeNeedsANetworkAndNamespaces(t *testing.T) {
	if _, err := NewScope("nf-x", "0.0.0.0/0", []string{client}); err == nil {
		t.Error("a default route was accepted as a scope")
	}
	if _, err := NewScope("nf-x", labNetwork, nil); err == nil {
		t.Error("a scope with no namespaces was accepted")
	}
	if _, err := NewScope("", labNetwork, []string{client}); err == nil {
		t.Error("a scope with no experiment was accepted")
	}
	if _, err := NewScope("nf-x", labNetwork, []string{client, client}); err == nil {
		t.Error("a scope that lists a namespace twice was accepted")
	}
}

func TestVerifyMeasuresWithoutChangingAnything(t *testing.T) {
	executor := &fakeExecutor{}
	prober := &fakeProber{measurement: healthy()}
	recovery, err := New(testScope(t),
		[]Request{consented(ActionRemoveFault, serviceA, "")},
		DefaultCriteria(), DefaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	verifications, err := recovery.Verify(context.Background(), prober)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(verifications) != 1 || !verifications[0].Passed {
		t.Fatalf("the verification is %+v, want a pass", verifications)
	}
	if got := executor.done(); len(got) != 0 {
		t.Errorf("Verify changed the laboratory: %v", got)
	}
}

func TestTheCriteriaJudgeWhatTheyMeasure(t *testing.T) {
	criteria := DefaultCriteria()
	if passed, reason := criteria.Judge(healthy()); !passed {
		t.Errorf("a clean path was rejected: %s", reason)
	}
	lossy := healthy()
	lossy.Received = 4
	if passed, _ := criteria.Judge(lossy); passed {
		t.Error("a path that lost packets was accepted")
	}
	silent := healthy()
	silent.Received = 0
	if passed, reason := criteria.Judge(silent); passed {
		t.Errorf("a path that answered nothing was accepted: %s", reason)
	}
	slow := healthy()
	slow.AverageRTT = 5 * time.Second
	if passed, _ := criteria.Judge(slow); passed {
		t.Error("a path slower than the criteria was accepted")
	}
	if _, err := (Criteria{MaxLoss: 150}).check("test", DefaultLimits()); !apperr.Is(err, apperr.KindInvalidArgument) {
		t.Errorf("an impossible loss bound is %v, want a refusal", err)
	}
}

func TestCancellationStopsVerification(t *testing.T) {
	executor := &fakeExecutor{}
	prober := &fakeProber{measurement: healthy()}
	recovery, err := New(testScope(t),
		[]Request{consented(ActionRemoveFault, serviceA, "")},
		DefaultCriteria(), DefaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := recovery.Execute(ctx, executor, prober); !apperr.Is(err, apperr.KindCanceled) {
		t.Errorf("a canceled recovery is %v, want %s", err, apperr.KindCanceled)
	}
}

func TestTheLimitsBoundTheSet(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxActions = 1
	_, err := New(testScope(t), []Request{
		consented(ActionRemoveFault, serviceA, ""),
		consented(ActionRestartService, serviceA, ""),
	}, DefaultCriteria(), limits, time.Now())
	if !apperr.Is(err, apperr.KindLimitExceeded) {
		t.Fatalf("an oversized set is %s, want %s: %v", kindName(err), apperr.KindLimitExceeded, err)
	}
	if _, err := New(testScope(t), nil, DefaultCriteria(), limits, time.Now()); err == nil {
		t.Error("a recovery with no action was accepted")
	}
	if _, err := New(testScope(t), []Request{consented(ActionRemoveFault, serviceA, "")},
		DefaultCriteria(), Limits{MaxActions: 1, MaxReasonLength: 1, MaxDeadline: time.Second}, time.Now()); !apperr.Is(err, apperr.KindLimitExceeded) {
		t.Errorf("an over-long reason is %v, want a refusal", err)
	}
}
