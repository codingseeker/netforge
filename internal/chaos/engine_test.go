package chaos

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/netforge/netforge/internal/apperr"
)

type fakeKernel struct {
	mu      sync.Mutex
	devices map[string]*fakeDevice
	log     []string

	failNext string

	lie bool
}

type fakeDevice struct {
	kind   string
	handle string

	options map[string]any
	present bool
}

func newFakeKernel() *fakeKernel {
	return &fakeKernel{devices: map[string]*fakeDevice{}}
}

func (f *fakeKernel) Run(namespace string, argv ...string) (string, string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, namespace+" "+strings.Join(argv, " "))

	if len(argv) < 2 || argv[0] != "tc" {
		return "", "chaos may only run tc", 1, nil
	}
	device := argumentAfter(argv, "dev")
	if device == "" {
		return "", "Error: missing device", 1, nil
	}
	f.devices[device] = ensureDevice(f.devices, device)

	switch {
	case argv[1] == "-j" && argv[2] == "qdisc" && argv[3] == "show":
		return f.show(device), "", 0, nil
	case argv[1] == "qdisc" && argv[2] == "del":
		if f.failNext == "del" {
			f.failNext = ""
			return "", "Error: RTNETLINK answers: No such file or directory", 2, nil
		}
		delete(f.devices, device)
		return "", "", 0, nil
	case argv[1] == "qdisc" && argv[2] == "replace":
		if f.failNext == "replace" {
			f.failNext = ""
			return "", "Error: Invalid qdisc name", 1, nil
		}
		f.install(device, argv)
		return "", "", 0, nil
	}
	return "", "Error: unsupported command", 1, nil
}

func ensureDevice(devices map[string]*fakeDevice, name string) *fakeDevice {
	if existing, ok := devices[name]; ok {
		return existing
	}
	created := &fakeDevice{kind: "noqueue", handle: "0:"}
	devices[name] = created
	return created
}

func (f *fakeKernel) install(device string, argv []string) {
	target := f.devices[device]
	kind := qdiscKind(argv)
	if kind == "" {
		return
	}
	if !target.present || target.kind != kind {
		target.handle = nextHandle()
	}
	target.kind = kind
	target.present = true
	target.options = kernelOptions(kind, argv, f.lie)
}

func (f *fakeKernel) show(device string) string {
	target, ok := f.devices[device]
	if !ok || !target.present {
		return `[{"kind":"noqueue","handle":"0:","root":true,"refcnt":2,"options":{}}]`
	}
	entry := map[string]any{
		"kind":    target.kind,
		"handle":  target.handle,
		"root":    true,
		"refcnt":  13,
		"options": target.options,
	}
	data, _ := json.Marshal([]map[string]any{entry})
	return string(data)
}

func (f *fakeKernel) installed(device string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	target, ok := f.devices[device]
	return ok && target.present
}

func (f *fakeKernel) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

type fakeTopology struct {
	links map[string]Endpoint
	names map[string][]string
}

func newFakeTopology() *fakeTopology {
	return &fakeTopology{
		links: map[string]Endpoint{
			"nf-client0-tc/nfc0":  {Namespace: "nf-router-tc", Interface: "nfr-c0"},
			"nf-server0-tc/nfs0":  {Namespace: "nf-router-tc", Interface: "nfr-s0"},
			"nf-router-tc/nfr-c0": {Namespace: "nf-client0-tc", Interface: "nfc0"},
			"nf-router-tc/nfr-s0": {Namespace: "nf-server0-tc", Interface: "nfs0"},
		},
		names: map[string][]string{
			"nf-client0-tc": {"nfc0"},
			"nf-server0-tc": {"nfs0"},
			"nf-router-tc":  {"nfr-c0", "nfr-s0"},
		},
	}
}

func (f *fakeTopology) Devices(namespace string) ([]string, error) {
	return f.names[namespace], nil
}

func (f *fakeTopology) Peer(namespace, device string) (Endpoint, bool) {
	peer, ok := f.links[namespace+"/"+device]
	return peer, ok
}

func newTestEngine(t *testing.T, limits Limits) (*Engine, *fakeKernel, func()) {
	t.Helper()
	kernel := newFakeKernel()
	scope := Scope{
		Network:    mustPrefix(t, "10.42.0.0/24"),
		Namespaces: []string{"nf-client0-tc", "nf-server0-tc", "nf-router-tc"},
	}
	engine, err := New(context.Background(), scope, limits, kernel, newFakeTopology())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return engine, kernel, func() { _ = engine.Close() }
}

func TestEngineAppliesAndVerifiesEveryKind(t *testing.T) {
	cases := map[Kind]Spec{
		KindLatency: {
			Target: "nf-client0-tc", Kind: KindLatency,
			Parameters: Parameters{Latency: Duration(50 * time.Millisecond)},
		},
		KindJitter: {
			Target: "nf-client0-tc", Kind: KindJitter,
			Parameters: Parameters{Latency: Duration(50 * time.Millisecond), Jitter: Duration(8 * time.Millisecond)},
		},
		KindPacketLoss: {
			Target: "nf-client0-tc", Kind: KindPacketLoss,
			Parameters: Parameters{Loss: 10},
		},
		KindBandwidth: {
			Target: "nf-client0-tc", Kind: KindBandwidth,
			Parameters: Parameters{Rate: 200_000},
		},
		KindDuplication: {
			Target: "nf-client0-tc", Kind: KindDuplication,
			Parameters: Parameters{Duplication: 20},
		},
		KindReordering: {
			Target: "nf-client0-tc", Kind: KindReordering,
			Parameters: Parameters{Reordering: 25, Latency: Duration(20 * time.Millisecond), Gap: 3},
		},
	}
	for kind, spec := range cases {
		t.Run(string(kind), func(t *testing.T) {
			engine, kernel, done := newTestEngine(t, Limits{})
			defer done()

			fault, err := engine.Apply(context.Background(), "exp-1", spec)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if fault.Status != StatusActive || !fault.Verified {
				t.Fatalf("got status %q verified %v, want an active verified fault", fault.Status, fault.Verified)
			}
			if !fault.Start.Before(fault.Expires) {
				t.Errorf("got start %s expires %s, want a deadline after the start", fault.Start, fault.Expires)
			}
			if fault.Expires.Sub(fault.Start) != fault.Duration.Std() {
				t.Errorf("the deadline does not match the duration %s", fault.Duration)
			}
			if fault.Source != (Endpoint{Namespace: "nf-client0-tc", Interface: "nfc0"}) {
				t.Errorf("got source %s, want the client device", fault.Source)
			}
			if fault.Destination != (Endpoint{Namespace: "nf-router-tc", Interface: "nfr-c0"}) {
				t.Errorf("got destination %s, want the far end of the link", fault.Destination)
			}
			if !kernel.installed("nfc0") {
				t.Error("the kernel was left without the fault")
			}

			if fault.Observed.Kind == "" {
				t.Error("the record carries no kernel observation")
			}
			if err := verify(fault.Observed, spec.Parameters); err != nil {
				t.Errorf("the observed state does not match the request: %v", err)
			}
			if len(fault.Commands) == 0 {
				t.Error("the record does not say which commands were run")
			}
		})
	}
}

func TestEngineInstallsAndRemoves(t *testing.T) {
	engine, kernel, done := newTestEngine(t, Limits{})
	defer done()

	fault, err := engine.Apply(context.Background(), "exp-1", Spec{
		Target: "nf-client0-tc", Kind: KindPacketLoss, Parameters: Parameters{Loss: 10},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !kernel.installed("nfc0") {
		t.Fatal("the fault was not installed")
	}
	if engine.Active() != 1 {
		t.Errorf("got %d active faults, want 1", engine.Active())
	}

	removed, err := engine.Remove(fault.ID)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if removed.Status != StatusRemoved {
		t.Errorf("got status %q, want %q", removed.Status, StatusRemoved)
	}
	if kernel.installed("nfc0") {
		t.Error("the fault is still installed after removal")
	}
	if removed.Observed.Kind != "" {
		t.Error("a removed fault must not keep claiming a kernel observation")
	}

	again, err := engine.Remove(fault.ID)
	if err != nil {
		t.Fatalf("removing a removed fault must succeed, got %v", err)
	}
	if again.Status != StatusRemoved {
		t.Errorf("got status %q, want %q", again.Status, StatusRemoved)
	}
	if _, err := engine.Remove("no-such-fault"); !apperr.IsKind(err, apperr.KindNotFound) {
		t.Errorf("got %v, want a not found error", err)
	}
}

func TestEngineRefusesAnythingOutsideTheScope(t *testing.T) {
	engine, kernel, done := newTestEngine(t, Limits{})
	defer done()

	_, err := engine.Apply(context.Background(), "exp-1", Spec{
		Target: "nf-host", Kind: KindPacketLoss, Parameters: Parameters{Loss: 10},
	})
	if !apperr.IsKind(err, apperr.KindPermission) {
		t.Fatalf("got %v, want a permission error", err)
	}
	if len(kernel.commands()) != 0 {
		t.Errorf("a refused fault still ran %v", kernel.commands())
	}

	if _, _, _, err := engine.run("host", "tc", "qdisc", "show", "dev", "eth0"); !apperr.IsKind(err, apperr.KindPermission) {
		t.Errorf("got %v, want a permission error", err)
	}
	if _, _, _, err := engine.run("nf-client0-tc", "ip", "link", "show"); !apperr.IsKind(err, apperr.KindPermission) {
		t.Errorf("the engine ran a program other than tc: %v", err)
	}
}

func TestEngineRefusesUnknownDevices(t *testing.T) {
	engine, kernel, done := newTestEngine(t, Limits{})
	defer done()

	_, err := engine.Apply(context.Background(), "exp-1", Spec{
		Target: "nf-client0-tc", Interface: "eth9", Kind: KindPacketLoss, Parameters: Parameters{Loss: 10},
	})
	if !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("got %v, want a not found error", err)
	}
	if len(kernel.commands()) != 0 {
		t.Errorf("an unknown device still ran %v", kernel.commands())
	}

	_, err = engine.Apply(context.Background(), "exp-1", Spec{
		Target: "nf-router-tc", Kind: KindLatency, Parameters: Parameters{Latency: Duration(time.Millisecond)},
	})
	if !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("got %v, want the ambiguity to be refused", err)
	}

	if _, err := engine.Apply(context.Background(), "exp-1", Spec{
		Target: "nf-client0-tc", Interface: "nfc0", Kind: KindLatency,
		Parameters: Parameters{Latency: Duration(time.Millisecond)},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

func TestEnginePlacesAnInboundFaultOnThePeer(t *testing.T) {
	engine, _, done := newTestEngine(t, Limits{})
	defer done()

	fault, err := engine.Apply(context.Background(), "exp-1", Spec{
		Target: "nf-client0-tc", Direction: DirectionIn, Kind: KindLatency,
		Parameters: Parameters{Latency: Duration(40 * time.Millisecond)},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if fault.Source != (Endpoint{Namespace: "nf-router-tc", Interface: "nfr-c0"}) {
		t.Errorf("got source %s, want the peer, an inbound fault leaves the far end", fault.Source)
	}
	if fault.Destination != (Endpoint{Namespace: "nf-client0-tc", Interface: "nfc0"}) {
		t.Errorf("got destination %s, want the target", fault.Destination)
	}
	if fault.Target != (Endpoint{Namespace: "nf-client0-tc", Interface: "nfc0"}) {
		t.Errorf("got target %s, want the namespace that feels the fault", fault.Target)
	}
}

func TestEngineKeepsOneFaultPerDevice(t *testing.T) {
	engine, _, done := newTestEngine(t, Limits{})
	defer done()

	if _, err := engine.Apply(context.Background(), "exp-1", Spec{
		Target: "nf-client0-tc", Kind: KindPacketLoss, Parameters: Parameters{Loss: 10},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	_, err := engine.Apply(context.Background(), "exp-1", Spec{
		Target: "nf-client0-tc", Kind: KindLatency, Parameters: Parameters{Latency: Duration(time.Millisecond)},
	})
	if !apperr.IsKind(err, apperr.KindConflict) {
		t.Fatalf("got %v, want a conflict", err)
	}

	if _, err := engine.Apply(context.Background(), "exp-1", Spec{
		Target: "nf-server0-tc", Kind: KindLatency, Parameters: Parameters{Latency: Duration(time.Millisecond)},
	}); err != nil {
		t.Fatalf("a second device must be allowed: %v", err)
	}
}

func TestEngineRefusesADuplicateIdentifier(t *testing.T) {
	engine, _, done := newTestEngine(t, Limits{})
	defer done()

	spec := Spec{
		ID: "loss-1", Target: "nf-client0-tc", Kind: KindPacketLoss, Parameters: Parameters{Loss: 10},
	}
	if _, err := engine.Apply(context.Background(), "exp-1", spec); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	_, err := engine.Apply(context.Background(), "exp-1", spec)
	if !apperr.IsKind(err, apperr.KindConflict) {
		t.Fatalf("got %v, want a conflict", err)
	}
	if _, err := engine.Apply(context.Background(), "exp-1", Spec{
		ID: "loss-2", Target: "nf-server0-tc", Kind: KindPacketLoss, Parameters: Parameters{Loss: 10},
	}); err != nil {
		t.Fatalf("a distinct identifier must be accepted: %v", err)
	}
}

func TestEngineRollsBackAFaultItCannotVerify(t *testing.T) {
	engine, kernel, done := newTestEngine(t, Limits{})
	defer done()
	kernel.lie = true

	_, err := engine.Apply(context.Background(), "exp-1", Spec{
		Target: "nf-client0-tc", Kind: KindLatency, Parameters: Parameters{Latency: Duration(50 * time.Millisecond)},
	})
	if err == nil {
		t.Fatal("a kernel that disagrees with the request was accepted")
	}
	if kernel.installed("nfc0") {
		t.Error("the fault was left installed after a failed verification")
	}
	if len(engine.List()) != 0 {
		t.Errorf("a fault that is not in the kernel was recorded: %v", engine.List())
	}
}

func TestEngineRollsBackAFaultTheKernelRefuses(t *testing.T) {
	engine, kernel, done := newTestEngine(t, Limits{})
	defer done()
	kernel.failNext = "replace"

	if _, err := engine.Apply(context.Background(), "exp-1", Spec{
		Target: "nf-client0-tc", Kind: KindPacketLoss, Parameters: Parameters{Loss: 10},
	}); err == nil {
		t.Fatal("a refused fault was reported as applied")
	}
	if kernel.installed("nfc0") {
		t.Error("the fault survived a failed installation")
	}
	if len(engine.List()) != 0 {
		t.Errorf("a failed fault left a record: %v", engine.List())
	}
}

func TestEngineRemovesEverythingWhenClosed(t *testing.T) {
	engine, kernel, done := newTestEngine(t, Limits{})
	defer done()

	if _, err := engine.Apply(context.Background(), "exp-1", Spec{
		Target: "nf-client0-tc", Kind: KindPacketLoss, Parameters: Parameters{Loss: 10},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := engine.Apply(context.Background(), "exp-1", Spec{
		Target: "nf-server0-tc", Kind: KindLatency, Parameters: Parameters{Latency: Duration(time.Millisecond)},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if err := engine.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if kernel.installed("nfc0") || kernel.installed("nfs0") {
		t.Error("closing the engine left a fault in the kernel")
	}
	if err := engine.Close(); err != nil {
		t.Errorf("closing twice must succeed, got %v", err)
	}

	_, err := engine.Apply(context.Background(), "exp-after-close", Spec{
		Target: "nf-client0-tc", Kind: KindPacketLoss, Parameters: Parameters{Loss: 10},
	})
	if err == nil {
		t.Fatal("a closed engine accepted a fault under a new id")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("got %v, want the refusal to say the engine is closed", err)
	}
	if kernel.installed("nfc0") {
		t.Error("a fault was put in the kernel after the engine was closed")
	}

	if err := engine.RemoveAll(); err != nil {
		t.Errorf("RemoveAll on a closed engine: %v", err)
	}
}

func TestEngineExpiresFaults(t *testing.T) {
	engine, kernel, done := newTestEngine(t, Limits{})
	defer done()

	fault, err := engine.Apply(context.Background(), "exp-1", Spec{
		Target: "nf-client0-tc", Kind: KindPacketLoss, Parameters: Parameters{Loss: 10},
		Duration: Duration(30 * time.Second),
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if engine.Expire(fault.Expires.Add(-time.Second)) != 0 {
		t.Error("a fault expired before its deadline")
	}
	if !kernel.installed("nfc0") {
		t.Fatal("the fault was removed early")
	}
	if removed := engine.Expire(fault.Expires); removed != 1 {
		t.Errorf("got %d removed, want 1", removed)
	}
	if kernel.installed("nfc0") {
		t.Error("the expired fault is still in the kernel")
	}
	stored, ok := engine.Get(fault.ID)
	if !ok {
		t.Fatal("the record of an expired fault was thrown away")
	}
	if stored.Status != StatusExpired {
		t.Errorf("got status %q, want %q", stored.Status, StatusExpired)
	}
	if engine.Expire(fault.Expires.Add(time.Hour)) != 0 {
		t.Error("an expired fault was removed twice")
	}
}

func TestEngineWatcherRemovesExpiredFaultsOnItsOwn(t *testing.T) {
	kernel := newFakeKernel()
	scope := Scope{
		Network:    mustPrefix(t, "10.42.0.0/24"),
		Namespaces: []string{"nf-client0-tc"},
	}
	engine, err := New(context.Background(), scope, Limits{}, kernel, &fakeTopology{
		links: map[string]Endpoint{"nf-client0-tc/nfc0": {Namespace: "nf-router-tc", Interface: "nfr-c0"}},
		names: map[string][]string{"nf-client0-tc": {"nfc0"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = engine.Close() }()

	if _, err := engine.Apply(context.Background(), "exp-1", Spec{
		Target: "nf-client0-tc", Kind: KindPacketLoss, Parameters: Parameters{Loss: 10},
		Duration: Duration(50 * time.Millisecond),
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for kernel.installed("nfc0") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if kernel.installed("nfc0") {
		t.Fatal("the watcher did not remove the fault when it ran out of time")
	}
	if got := engine.List(); len(got) != 1 || got[0].Status != StatusExpired {
		t.Errorf("got %+v, want one expired record", got)
	}
}

func TestEngineEnforcesTheFaultLimit(t *testing.T) {
	engine, _, done := newTestEngine(t, Limits{MaxFaults: 2})
	defer done()

	specs := []Spec{
		{Target: "nf-client0-tc", Kind: KindPacketLoss, Parameters: Parameters{Loss: 10}},
		{Target: "nf-server0-tc", Kind: KindPacketLoss, Parameters: Parameters{Loss: 10}},
		{Target: "nf-router-tc", Interface: "nfr-c0", Kind: KindPacketLoss, Parameters: Parameters{Loss: 10}},
	}
	applied, err := engine.ApplyAll(context.Background(), "exp-1", specs[:2])
	if err != nil {
		t.Fatalf("ApplyAll: %v", err)
	}
	if len(applied) != 2 {
		t.Fatalf("got %d faults, want 2", len(applied))
	}
	_, err = engine.Apply(context.Background(), "exp-1", specs[2])
	if !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Fatalf("got %v, want a limit error", err)
	}
}

func TestEngineRollsBackAWholeRejectedSet(t *testing.T) {
	engine, kernel, done := newTestEngine(t, Limits{})
	defer done()

	_, err := engine.ApplyAll(context.Background(), "exp-1", []Spec{
		{Target: "nf-client0-tc", Kind: KindPacketLoss, Parameters: Parameters{Loss: 10}},
		{Target: "nf-server0-tc", Kind: KindLatency, Parameters: Parameters{Latency: Duration(50 * time.Millisecond)}},
		{Target: "nf-router-tc", Interface: "nfr-c0", Kind: KindLatency, Parameters: Parameters{Latency: Duration(time.Hour)}},
	})
	if !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Fatalf("got %v, want the set to be rejected for its last fault", err)
	}
	if kernel.installed("nfc0") || kernel.installed("nfs0") {
		t.Error("a rejected set left faults behind")
	}
	if got := engine.Active(); got != 0 {
		t.Errorf("got %d active faults, want none", got)
	}
}

func TestEngineDerivesAStableIdentifier(t *testing.T) {
	engine, _, done := newTestEngine(t, Limits{})
	defer done()

	fault, err := engine.Apply(context.Background(), "exp-1", Spec{
		Target: "nf-client0-tc", Kind: KindPacketLoss, Parameters: Parameters{Loss: 10},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := "packet_loss@nf-client0-tc/nfc0:out"
	if fault.ID != want {
		t.Errorf("got %q, want %q", fault.ID, want)
	}
}

func TestEngineRequiresAnExperiment(t *testing.T) {
	engine, _, done := newTestEngine(t, Limits{})
	defer done()

	if _, err := engine.Apply(context.Background(), "  ", Spec{
		Target: "nf-client0-tc", Kind: KindPacketLoss, Parameters: Parameters{Loss: 10},
	}); !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Errorf("got %v, want an invalid argument error", err)
	}
}

func TestEngineRefusesWorkAfterTheContextEnds(t *testing.T) {
	kernel := newFakeKernel()
	scope := Scope{Network: mustPrefix(t, "10.42.0.0/24"), Namespaces: []string{"nf-client0-tc"}}
	ctx, cancel := context.WithCancel(context.Background())
	engine, err := New(ctx, scope, Limits{}, kernel, newFakeTopology())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = engine.Close() }()

	if _, err := engine.Apply(ctx, "exp-1", Spec{
		Target: "nf-client0-tc", Kind: KindPacketLoss, Parameters: Parameters{Loss: 10},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for engine.Active() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if kernel.installed("nfc0") {
		t.Fatal("the fault survived the end of the laboratory context")
	}
}

func TestEngineForgetsOldRecords(t *testing.T) {
	engine, _, done := newTestEngine(t, Limits{MaxFaults: 1})
	defer done()

	for i := 0; i < 6; i++ {
		fault, err := engine.Apply(context.Background(), "exp-1", Spec{
			ID: fmt.Sprintf("loss-%d", i), Target: "nf-client0-tc",
			Kind: KindPacketLoss, Parameters: Parameters{Loss: 10},
		})
		if err != nil {
			t.Fatalf("Apply %d: %v", i, err)
		}
		if _, err := engine.Remove(fault.ID); err != nil {
			t.Fatalf("Remove %d: %v", i, err)
		}
	}
	if got := len(engine.List()); got > 2 {
		t.Errorf("got %d records, the history must stay bounded", got)
	}
}
