package loadbalancer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codingseeker/netforge/internal/apperr"
)

func controlConfig(t *testing.T, backend string) (Config, string) {
	t.Helper()
	dir := t.TempDir()
	control := filepath.Join(dir, "control.json")
	config := testConfig(PolicyRoundRobin, backend)
	config.ControlPath = control
	config.Experiment = "nf-test-0001"
	return config, control
}

func write(t *testing.T, path string, control Control) {
	t.Helper()
	if err := WriteControl(path, control); err != nil {
		t.Fatalf("WriteControl: %v", err)
	}
}

func appliedState(balancer *Balancer, index int) string {
	backends := balancer.Backends()
	if index >= len(backends) {
		return ""
	}
	return backends[index].State
}

func TestARemovedBackendStopsTakingClients(t *testing.T) {
	t.Parallel()
	a := startBackend(t, "a")
	b := startBackend(t, "b")
	config, control := controlConfig(t, a.address)
	config.Backends = []string{a.address, b.address}
	balancer := startBalancer(t, config)

	if got := appliedState(balancer, 0); got != StateHealthy {
		t.Fatalf("state = %q, want %q", got, StateHealthy)
	}
	write(t, control, Control{
		Experiment: "nf-test-0001",
		Commands:   []Command{{Seq: 1, Operation: OpRemoveBackend, Backend: a.address}},
	})
	eventually(t, "the backend to leave service", func() bool {
		return appliedState(balancer, 0) == StateRemoved
	})

	for range 4 {
		if name, _, _ := strings.Cut(servedBy(t, balancer, "after removal"), "|"); name != "b" {
			t.Fatalf("a client was served by %q, want the backend that was left", name)
		}
	}
	if stats := balancer.Stats(); stats.Removed != 1 {
		t.Errorf("removed = %d, want 1", stats.Removed)
	}
}

func TestARemovedBackendStaysOutEvenWhenItAnswers(t *testing.T) {
	t.Parallel()

	a := startBackend(t, "a")
	b := startBackend(t, "b")
	config, control := controlConfig(t, a.address)
	config.Backends = []string{a.address, b.address}
	balancer := startBalancer(t, config)

	write(t, control, Control{
		Experiment: "nf-test-0001",
		Commands:   []Command{{Seq: 1, Operation: OpRemoveBackend, Backend: a.address}},
	})
	eventually(t, "the backend to be removed", func() bool {
		return appliedState(balancer, 0) == StateRemoved
	})
	time.Sleep(4 * config.Limits.HealthInterval)
	if got := appliedState(balancer, 0); got != StateRemoved {
		t.Errorf("state = %q after several health checks, want %q", got, StateRemoved)
	}

	before := reportOf(balancer, 0).Checks
	time.Sleep(2 * config.Limits.HealthInterval)
	if after := reportOf(balancer, 0).Checks; after != before {
		t.Errorf("checks went from %d to %d, want a removed backend to stop being checked", before, after)
	}
}

func TestARemovedBackendComesBackAfterAHealthCheck(t *testing.T) {
	t.Parallel()
	a := startBackend(t, "a")
	b := startBackend(t, "b")
	config, control := controlConfig(t, a.address)
	config.Backends = []string{a.address, b.address}
	balancer := startBalancer(t, config)

	write(t, control, Control{
		Experiment: "nf-test-0001",
		Commands: []Command{
			{Seq: 1, Operation: OpRemoveBackend, Backend: a.address},
			{Seq: 2, Operation: OpRestoreBackend, Backend: a.address},
		},
	})

	eventually(t, "the restored backend to serve again", func() bool {
		return appliedState(balancer, 0) == StateHealthy
	})
}

func TestADrainStopsNewClientsAndIsNotARemoval(t *testing.T) {
	t.Parallel()
	a := startBackend(t, "a")
	b := startBackend(t, "b")
	config, control := controlConfig(t, a.address)
	config.Backends = []string{a.address, b.address}
	balancer := startBalancer(t, config)

	write(t, control, Control{
		Experiment: "nf-test-0001",
		Commands:   []Command{{Seq: 1, Operation: OpDrainConnection, Backend: a.address}},
	})
	eventually(t, "the backend to drain", func() bool {
		return appliedState(balancer, 0) == StateDraining
	})
	if stats := balancer.Stats(); stats.Removed != 0 {
		t.Errorf("removed = %d after a drain, want 0: a drain is not a removal", stats.Removed)
	}

	write(t, control, Control{
		Experiment: "nf-test-0001",
		Commands:   []Command{{Seq: 2, Operation: OpReEnableBackend, Backend: a.address}},
	})
	eventually(t, "the drain to be cancelled", func() bool {
		return appliedState(balancer, 0) == StateHealthy
	})
}

func TestReEnableRefusesARemovedBackend(t *testing.T) {
	t.Parallel()
	a := startBackend(t, "a")
	config, control := controlConfig(t, a.address)
	balancer := startBalancer(t, config)

	write(t, control, Control{
		Experiment: "nf-test-0001",
		Commands: []Command{
			{Seq: 1, Operation: OpRemoveBackend, Backend: a.address},
			{Seq: 2, Operation: OpReEnableBackend, Backend: a.address},
		},
	})

	eventually(t, "the refusal to be reported", func() bool {
		status := balancer.Status()
		return strings.Contains(status.ControlError, "restore_backend")
	})
	if got := appliedState(balancer, 0); got != StateRemoved {
		t.Errorf("state = %q, want the backend to have stayed removed", got)
	}
}

func TestAControlDocumentCannotIntroduceABackend(t *testing.T) {
	t.Parallel()
	a := startBackend(t, "a")
	b := startBackend(t, "b")
	config, control := controlConfig(t, a.address)
	config.Backends = []string{a.address, b.address}
	balancer := startBalancer(t, config)

	write(t, control, Control{
		Experiment: "nf-test-0001",
		Commands: []Command{
			{Seq: 1, Operation: OpRemoveBackend, Backend: "10.42.0.99:7000"},
		},
	})
	eventually(t, "the refusal to be reported", func() bool {
		return strings.Contains(balancer.Status().ControlError, "may not introduce one")
	})
	if len(balancer.Backends()) != 2 {
		t.Errorf("%d backends after a control document, want the 2 that were configured", len(balancer.Backends()))
	}
	for _, backend := range balancer.Backends() {
		if backend.Address == "10.42.0.99:7000" {
			t.Error("the control document registered a backend")
		}
	}
}

func TestAControlDocumentForAnotherExperimentIsRefused(t *testing.T) {
	t.Parallel()
	a := startBackend(t, "a")
	config, control := controlConfig(t, a.address)
	balancer := startBalancer(t, config)

	write(t, control, Control{
		Experiment: "nf-other-0002",
		Commands:   []Command{{Seq: 1, Operation: OpRemoveBackend, Backend: a.address}},
	})
	eventually(t, "the refusal to be reported", func() bool {
		return strings.Contains(balancer.Status().ControlError, "nf-other-0002")
	})
	if got := appliedState(balancer, 0); got != StateHealthy {
		t.Errorf("state = %q, want the backend to be untouched", got)
	}
}

func TestCommandsAreAppliedOnce(t *testing.T) {
	t.Parallel()
	a := startBackend(t, "a")
	b := startBackend(t, "b")
	config, control := controlConfig(t, a.address)
	config.Backends = []string{a.address, b.address}

	config.Limits.DrainTimeout = 30 * time.Second
	balancer := startBalancer(t, config)

	document := Control{
		Experiment: "nf-test-0001",
		Commands:   []Command{{Seq: 7, Operation: OpDrainConnection, Backend: a.address}},
	}
	write(t, control, document)
	eventually(t, "the drain to start", func() bool {
		return appliedState(balancer, 0) == StateDraining
	})
	first := reportOf(balancer, 0).DrainingSince

	write(t, control, document)
	write(t, control, document)
	if got := balancer.Applied(); got != 7 {
		t.Errorf("applied = %d, want 7", got)
	}
	if again := reportOf(balancer, 0).DrainingSince; !again.Equal(first) {
		t.Errorf("the drain restarted: it began at %v and then at %v", first, again)
	}
}

func TestAMissingControlDocumentIsNotAnError(t *testing.T) {
	t.Parallel()
	a := startBackend(t, "a")
	config, _ := controlConfig(t, a.address)
	balancer := startBalancer(t, config)

	time.Sleep(2 * config.Limits.HealthInterval)
	if got := balancer.Status().ControlError; got != "" {
		t.Errorf("control error = %q, want none", got)
	}
	if got := appliedState(balancer, 0); got != StateHealthy {
		t.Errorf("state = %q, want %q", got, StateHealthy)
	}
}

func TestABrokenControlDocumentIsRefusedAndReportedOnce(t *testing.T) {
	t.Parallel()
	a := startBackend(t, "a")
	config, control := controlConfig(t, a.address)
	balancer := startBalancer(t, config)

	if err := os.WriteFile(control, []byte(`{"commands":[{"seq":1,"operation":"drop_everything","backend":"x"}]}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	eventually(t, "the refusal to be reported", func() bool {
		return balancer.Status().ControlError != ""
	})
	first := balancer.Status().ControlError
	time.Sleep(3 * config.Limits.HealthInterval)
	if again := balancer.Status().ControlError; again != first {
		t.Errorf("the refusal changed from %q to %q, want the same document reported once", first, again)
	}

	if got := appliedState(balancer, 0); got != StateHealthy {
		t.Errorf("state = %q, want the backend untouched", got)
	}
}

func TestAControlDocumentIsDecodedStrictly(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"empty", ``},
		{"unknown field", `{"experiment":"nf-x","commands":[],"surprise":1}`},
		{"unknown operation", `{"experiment":"nf-x","commands":[{"seq":1,"operation":"drop_all","backend":"a"}]}`},
		{"missing backend", `{"experiment":"nf-x","commands":[{"seq":1,"operation":"remove_backend"}]}`},
		{"missing experiment", `{"commands":[{"seq":1,"operation":"remove_backend","backend":"a"}]}`},
		{"empty commands", `{"experiment":"nf-x","commands":[]}`},
		{"trailing content", `{"experiment":"nf-x","commands":[{"seq":1,"operation":"remove_backend","backend":"a"}]} {}`},
		{"wrong type", `{"experiment":"nf-x","commands":"none"}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := ParseControl([]byte(testCase.raw)); err == nil {
				t.Errorf("ParseControl(%q) = nil, want a refusal", testCase.raw)
			}
		})
	}
}

func TestAControlDocumentIsBounded(t *testing.T) {
	huge := make([]Command, MaxCommands+1)
	for i := range huge {
		huge[i] = Command{Seq: uint64(i + 1), Operation: OpRemoveBackend, Backend: "10.42.0.2:7000"}
	}
	if _, err := ParseControl(mustControl(t, Control{Experiment: "nf-x", Commands: huge})); !apperr.Is(err, apperr.KindLimitExceeded) {
		t.Errorf("an oversized document is %v, want a limit refusal", err)
	}
	oversized := make([]byte, MaxControlBytes+1)
	for i := range oversized {
		oversized[i] = 'x'
	}
	if _, err := ParseControl(oversized); !apperr.Is(err, apperr.KindLimitExceeded) {
		t.Errorf("an oversized document is %v, want a limit refusal", err)
	}
}

func mustControl(t *testing.T, control Control) []byte {
	t.Helper()
	data, err := marshalControl(control)
	if err != nil {
		t.Fatalf("marshalControl: %v", err)
	}
	return data
}

func TestAWrittenControlDocumentIsPrivateToItsOwner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "control.json")
	write(t, path, Control{
		Experiment: "nf-x",
		Commands:   []Command{{Seq: 1, Operation: OpRemoveBackend, Backend: "10.42.0.2:7000"}},
	})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 600: a control document decides which backends serve", mode)
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Error("the temporary file was left behind")
	}
}

func TestAControlPathIsValidated(t *testing.T) {
	backend := startBackend(t, "a")
	base := testConfig(PolicyRoundRobin, backend.address)

	relative := base
	relative.ControlPath = "control.json"
	relative.Experiment = "nf-test-0001"
	if _, err := relative.Validate(); !apperr.Is(err, apperr.KindInvalidArgument) {
		t.Errorf("a relative control path is %v, want a refusal", err)
	}

	withoutExperiment := base
	withoutExperiment.ControlPath = filepath.Join(t.TempDir(), "control.json")
	if _, err := withoutExperiment.Validate(); err == nil {
		t.Error("a control path with no experiment was accepted")
	}

	both := base
	both.ControlPath = filepath.Join(t.TempDir(), "shared.json")
	both.StatusPath = both.ControlPath
	both.Experiment = "nf-test-0001"
	if _, err := both.Validate(); err == nil {
		t.Error("a control path that is also the status path was accepted")
	}

	orphan := base
	orphan.Experiment = "nf-test-0001"
	if _, err := orphan.Validate(); err == nil {
		t.Error("an experiment with no control path was accepted")
	}

	fine := base
	fine.ControlPath = filepath.Join(t.TempDir(), "control.json")
	fine.Experiment = "nf-test-0001"
	checked, err := fine.Validate()
	if err != nil {
		t.Fatalf("a valid control path was refused: %v", err)
	}
	if checked.ExperimentID() != "nf-test-0001" {
		t.Errorf("experiment = %q, want nf-test-0001", checked.ExperimentID())
	}
}

func TestWriteControlRefusesADocumentItCouldNotApply(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.json")
	if err := WriteControl(path, Control{}); err == nil {
		t.Error("an empty document was written")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a refused document left a file behind")
	}
	if err := WriteControl("", Control{Experiment: "nf-x"}); err == nil {
		t.Error("a document was written to an empty path")
	}
}
