package recovery

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/loadbalancer"
)

// The tests here join two real pieces: a load balancer that is actually serving,
// and a recovery that reaches it through the control document it was configured
// with. Nothing is stubbed between the two, so what is checked is the path an
// action takes in production, including the part where the action is named as an
// address with a port and has to be recognised as a backend the balancer already
// has. The laboratory itself is not here: taking a namespace out for repair is
// the network package's business and is exercised where those namespaces exist.

// balancerExecutor is a laboratory whose only controllable part is the balancer.
// The three namespace actions refuse rather than pretend, so a test cannot
// accidentally pass on an action that was never implemented.
type balancerExecutor struct {
	controller *ControlFile
}

func (b balancerExecutor) RemoveFault(context.Context, string) error {
	return apperr.Unsupported("test.executor", "this test has no laboratory to remove a fault from", nil)
}

func (b balancerExecutor) RestoreRoute(context.Context, string, string) error {
	return apperr.Unsupported("test.executor", "this test has no laboratory to restore a route in", nil)
}

func (b balancerExecutor) RestartService(context.Context, string) error {
	return apperr.Unsupported("test.executor", "this test has no laboratory to restart a service in", nil)
}

func (b balancerExecutor) RemoveBackend(ctx context.Context, _ string, address string) error {
	return b.controller.Remove(ctx, address)
}

func (b balancerExecutor) RestoreBackend(ctx context.Context, _ string, address string) error {
	return b.controller.Restore(ctx, address)
}

func (b balancerExecutor) DrainConnection(ctx context.Context, _ string, address string) error {
	return b.controller.Drain(ctx, address)
}

func (b balancerExecutor) ReEnableBackend(ctx context.Context, _ string, address string) error {
	return b.controller.ReEnable(ctx, address)
}

const (
	balancerExperiment = "nf-test-0001"
	balancerNetwork    = "127.0.0.0/8"
	balancerNamespace  = "lb"
	balancerBackend    = "127.0.0.1:7000"
)

// startBalancer runs a balancer on an unused local port with a control document
// in a directory the test owns, and with the given backends configured. The
// backends are not started: whether a configured address is reachable is the
// balancer's business, and taking one out of service does not require it to have
// been serving first.
func startBalancer(t *testing.T, experiment string, backends ...string) (*loadbalancer.Balancer, string) {
	t.Helper()
	control := filepath.Join(t.TempDir(), "control.json")
	balancer, err := loadbalancer.New(loadbalancer.Config{
		Listen:      "127.0.0.1:0",
		Backends:    backends,
		Policy:      loadbalancer.PolicyRoundRobin,
		ControlPath: control,
		Experiment:  experiment,
		Limits:      balancerLimits(),
	})
	if err != nil {
		t.Fatalf("loadbalancer.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- balancer.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := balancer.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return within five seconds of the balancer being closed")
		}
	})
	return balancer, control
}

// balancerLimits is a healthy balancer's configuration with a health check that
// answers quickly, so a test does not wait out the production interval.
func balancerLimits() loadbalancer.Limits {
	limits := loadbalancer.DefaultLimits()
	limits.HealthInterval = 100 * time.Millisecond
	limits.HealthTimeout = 60 * time.Millisecond
	limits.FailureThreshold = 2
	limits.RecoveryThreshold = 2
	return limits
}

func backendState(balancer *loadbalancer.Balancer, index int) string {
	backends := balancer.Backends()
	if index >= len(backends) {
		return ""
	}
	return backends[index].State
}

func eventually(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// recoveryFor builds a recovery authorized for the given requests, reaching a
// balancer through its control document.
func recoveryFor(t *testing.T, control string, requests ...Request) (*Recovery, balancerExecutor) {
	t.Helper()
	scope, err := NewScope(balancerExperiment, balancerNetwork, []string{balancerNamespace})
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	controller, err := NewControlFile(control, balancerExperiment)
	if err != nil {
		t.Fatalf("NewControlFile: %v", err)
	}
	// The prober is the one piece faked here. What is under test is the path from
	// an authorized action to a change in a running balancer; the measurement
	// that follows an action is checked in the tests that have a laboratory.
	subject, err := New(scope, requests, DefaultCriteria(), DefaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return subject, balancerExecutor{controller: controller}
}

func TestARecoveryRemovesAndRestoresARealBackend(t *testing.T) {
	balancer, control := startBalancer(t, balancerExperiment, balancerBackend)

	subject, executor := recoveryFor(t, control,
		consented(ActionRemoveBackend, balancerBackend, balancerNamespace),
		consented(ActionRestoreBackend, balancerBackend, balancerNamespace))

	outcomes, err := subject.Execute(context.Background(), executor, &fakeProber{measurement: healthy()})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(outcomes) != 2 {
		t.Fatalf("Execute returned %d outcomes, want one per authorized action", len(outcomes))
	}
	for i, outcome := range outcomes {
		if !outcome.Passed() {
			t.Errorf("action %d did not pass: %v", i, outcome.Err)
		}
	}
	eventually(t, "the backend to be checked again", func() bool {
		return backendState(balancer, 0) == loadbalancer.StateHealthy
	})
}

func TestARecoveryRemovesABackendAndTheBalancerStopsUsingIt(t *testing.T) {
	balancer, control := startBalancer(t, balancerExperiment, balancerBackend)

	subject, executor := recoveryFor(t, control,
		consented(ActionRemoveBackend, balancerBackend, balancerNamespace))

	if _, err := subject.Execute(context.Background(), executor, &fakeProber{measurement: healthy()}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	eventually(t, "the backend to leave service", func() bool {
		return backendState(balancer, 0) == loadbalancer.StateRemoved
	})
	if removed := balancer.Stats().Removed; removed != 1 {
		t.Errorf("the balancer counted %d removed backends, want one", removed)
	}
}

func TestASecondRemovalOfTheSameBackendIsNotASecondRemoval(t *testing.T) {
	balancer, control := startBalancer(t, balancerExperiment, balancerBackend)
	subject, executor := recoveryFor(t, control,
		consented(ActionRemoveBackend, balancerBackend, balancerNamespace))

	if _, err := subject.Execute(context.Background(), executor, &fakeProber{measurement: healthy()}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	eventually(t, "the backend to leave service", func() bool {
		return backendState(balancer, 0) == loadbalancer.StateRemoved
	})
	// A second run of the same experiment asks the balancer the same question.
	// It must be an answer rather than a conflict, because a recovery that is
	// retried after a failure is ordinary.
	subject, executor = recoveryFor(t, control,
		consented(ActionRemoveBackend, balancerBackend, balancerNamespace))
	if _, err := subject.Execute(context.Background(), executor, &fakeProber{measurement: healthy()}); err != nil {
		t.Fatalf("Execute a second time: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if state := backendState(balancer, 0); state != loadbalancer.StateRemoved {
		t.Errorf("the backend is %q after a repeated removal, want %q", state, loadbalancer.StateRemoved)
	}
}

func TestABackendTheBalancerDoesNotHaveIsNeverTouched(t *testing.T) {
	balancer, control := startBalancer(t, balancerExperiment, balancerBackend)
	// A second address on another port: inside the scope network, so the scope
	// check passes, but never registered with the balancer. Writing the document
	// succeeds, because the controller cannot know what the balancer has; the
	// balancer is the authority on that, and refusing is what it must do.
	subject, executor := recoveryFor(t, control,
		consented(ActionRemoveBackend, "127.0.0.1:7001", balancerNamespace))

	if _, err := subject.Execute(context.Background(), executor, &fakeProber{measurement: healthy()}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	eventually(t, "the balancer to refuse the command", func() bool {
		return strings.Contains(balancer.Status().ControlError, "7001")
	})
	if state := backendState(balancer, 0); state == loadbalancer.StateRemoved {
		t.Error("the backend the balancer does have was removed, which is not what the command asked for")
	}
	// The refusal says why, so an operator reading the status is not left
	// guessing whether the command was lost or denied.
	if err := balancer.Status().ControlError; !strings.Contains(err, "may not introduce one") {
		t.Errorf("the control error is %q, want it to say a control document may not introduce a backend", err)
	}
}

func TestAControlFileRefusesAnotherExperimentsDocument(t *testing.T) {
	_, control := startBalancer(t, balancerExperiment, balancerBackend)
	// A controller for one run must not write a document another run's balancer
	// would act on, even when the path is right.
	controller, err := NewControlFile(control, "nf-test-0002")
	if err != nil {
		t.Fatalf("NewControlFile: %v", err)
	}
	if err := controller.Remove(context.Background(), balancerBackend); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	raw, err := os.ReadFile(control)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), `"nf-test-0002"`) {
		t.Errorf("the document does not name the controller's own experiment: %s", raw)
	}
	if strings.Contains(string(raw), balancerExperiment) {
		t.Errorf("the document names the other experiment: %s", raw)
	}
}

func TestAControlFileRefusesAMalformedRequest(t *testing.T) {
	_, control := startBalancer(t, balancerExperiment, balancerBackend)
	controller, err := NewControlFile(control, balancerExperiment)
	if err != nil {
		t.Fatalf("NewControlFile: %v", err)
	}
	// A target that is not an endpoint must be refused by the controller rather
	// than written for the balancer to refuse on its next health tick.
	err = controller.Drain(context.Background(), "not an endpoint")
	if err == nil {
		t.Fatal("a drain for something that is not an endpoint was written")
	}
	if !apperr.Is(err, apperr.KindValidation) {
		t.Errorf("a malformed request is %v, want a refusal", err)
	}
	if _, statErr := os.Stat(control); statErr == nil {
		t.Error("a refused request still wrote a document")
	}
}
