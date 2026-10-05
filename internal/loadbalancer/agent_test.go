package loadbalancer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestARunningBalancerReportsWhatItIsServing(t *testing.T) {
	t.Parallel()
	backends := []*backendServer{startBackend(t, "a"), startBackend(t, "b")}
	status := filepath.Join(t.TempDir(), "balancer.json")
	config := testConfig(PolicyRoundRobin, backends[0].address, backends[1].address)
	config.StatusPath = status

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, config) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve() = %v, want nil", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Serve() did not return within ten seconds of its context being cancelled")
		}
	})

	eventually(t, "the balancer to report itself running", func() bool {
		read, err := ReadStatus(status)
		return err == nil && read.State == StateRunning
	})
	running, err := ReadStatus(status)
	if err != nil {
		t.Fatalf("ReadStatus() = %v, want nil", err)
	}
	if served := servedByAt(t, running.Listen, "payload in flight"); served == "" {
		t.Error("the client was served by no backend")
	}

	read, err := ReadStatus(status)
	if err != nil {
		t.Fatalf("ReadStatus() = %v, want nil", err)
	}
	if read.Policy != PolicyRoundRobin.String() {
		t.Errorf("policy = %q, want %q", read.Policy, PolicyRoundRobin)
	}
	if read.Listen == "" {
		t.Error("listen is empty, want the address the balancer is bound to")
	}
	if read.PID == 0 {
		t.Error("pid is unset, want the process that is serving")
	}
	if read.At.IsZero() {
		t.Error("the status carries no time, want when it was written")
	}
	if len(read.Backends) != 2 {
		t.Fatalf("the status lists %d backends, want the 2 that were registered", len(read.Backends))
	}
	for _, entry := range read.Backends {
		if entry.Address != backends[0].address && entry.Address != backends[1].address {
			t.Errorf("the status lists %s, want one of the two that were registered", entry.Address)
		}
		if entry.State != StateHealthy {
			t.Errorf("backend %s is %s in the status, want %s", entry.Address, entry.State, StateHealthy)
		}
	}

	eventually(t, "the status to report the client that was placed", func() bool {
		latest, readErr := ReadStatus(status)
		return readErr == nil && latest.Stats.Served == 1
	})
	latest, err := ReadStatus(status)
	if err != nil {
		t.Fatalf("ReadStatus() = %v, want nil", err)
	}
	if latest.Stats.Served != 1 {
		t.Errorf("served = %d, want the client that was placed", latest.Stats.Served)
	}
	if latest.Stats.Active != 1 {
		t.Errorf("active = %d, want the connection the client is still holding", latest.Stats.Active)
	}
	placed := 0
	for _, entry := range latest.Backends {
		placed += entry.Accepted
	}
	if placed != 1 {
		t.Errorf("the status reports %d connections placed in total, want 1", placed)
	}
}

func TestAStoppedBalancerLeavesAStatusThatSaysItStopped(t *testing.T) {
	t.Parallel()
	backend := startBackend(t, "a")
	status := filepath.Join(t.TempDir(), "balancer.json")
	config := testConfig(PolicyRoundRobin, backend.address)
	config.StatusPath = status

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, config) }()
	eventually(t, "the balancer to report itself running", func() bool {
		read, err := ReadStatus(status)
		return err == nil && read.State == StateRunning
	})
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve() = %v, want nil for a balancer that was asked to stop", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve() did not return within ten seconds of its context being cancelled")
	}
	if read, err := ReadStatus(status); err != nil {
		t.Errorf("ReadStatus() after the balancer stopped = %v, want nil: the last report has to "+
			"outlive the process that wrote it", err)
	} else if read.State != StateRunning {
		t.Errorf("state = %q after the balancer stopped, want %q: it ran until it was asked to stop",
			read.State, StateRunning)
	}
}

func TestABalancerThatCannotStartSaysWhyInItsStatus(t *testing.T) {
	t.Parallel()
	status := filepath.Join(t.TempDir(), "balancer.json")
	config := testConfig(PolicyRoundRobin)
	config.StatusPath = status

	if err := Serve(context.Background(), config); err == nil {
		t.Fatal("Serve() with no backends = nil, want a refusal")
	}
	read, err := ReadStatus(status)
	if err != nil {
		t.Fatalf("ReadStatus() = %v, want nil", err)
	}
	if read.State != StateFailed {
		t.Errorf("state = %q, want %q", read.State, StateFailed)
	}
	if read.Error == "" {
		t.Error("no error was reported, want why the balancer could not start")
	}
	if read.Listen != "" {
		t.Errorf("listen = %q for a balancer that never bound, want empty", read.Listen)
	}
}

func TestTheStatusFileIsWrittenWhole(t *testing.T) {
	t.Parallel()
	status := filepath.Join(t.TempDir(), "nested", "balancer.json")
	config := testConfig(PolicyRoundRobin, "127.0.0.1:9401")
	config.StatusPath = status

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, config) }()

	stop := make(chan struct{})
	reads := make(chan [2]int, 1)
	go func() {
		good, partial := 0, 0
		for {
			select {
			case <-stop:
				reads <- [2]int{good, partial}
				return
			default:
			}
			data, readErr := os.ReadFile(status)
			if readErr != nil {
				continue
			}
			var parsed Status
			if json.Unmarshal(data, &parsed) != nil {
				partial++
				continue
			}
			good++
		}
	}()
	time.Sleep(200 * time.Millisecond)
	close(stop)
	counts := <-reads
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve() = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("Serve() did not return within ten seconds of its context being cancelled")
	}
	if counts[0] == 0 {
		t.Error("no read of the status file succeeded, want a document a reader can use")
	}
	if counts[1] != 0 {
		t.Errorf("%d of %d reads found a half-written document, want none: the file is written whole",
			counts[1], counts[0]+counts[1])
	}

	if _, err := os.Stat(status + ".tmp"); err == nil {
		t.Error("the temporary status file was left behind, want only the finished one")
	}
}

func TestReadingAMissingOrBrokenStatusIsReported(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	if _, err := ReadStatus(filepath.Join(directory, "absent.json")); err == nil {
		t.Error("ReadStatus() of a file that is not there = nil, want an error")
	}
	broken := filepath.Join(directory, "broken.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("the broken status could not be written: %v", err)
	}
	if _, err := ReadStatus(broken); err == nil {
		t.Error("ReadStatus() of a file that is not a status = nil, want an error")
	}
	empty := filepath.Join(directory, "empty.json")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("the empty status could not be written: %v", err)
	}
	if _, err := ReadStatus(empty); err == nil {
		t.Error("ReadStatus() of an empty file = nil, want an error rather than a status with no state")
	}
}

func TestTheAgentOnlyStartsWhenItIsAskedTo(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		nil,
		{},
		{"netforge-service"},
		{"netforge-balancer-extra"},
	} {
		handled, code := Dispatch(args)
		if handled {
			t.Errorf("Dispatch(%q) handled the arguments, want them left to whatever else wants them", args)
		}
		if code != 0 {
			t.Errorf("Dispatch(%q) = %d, want 0 for arguments that are not its own", args, code)
		}
	}
}

func TestTheAgentRefusesAConfigurationItCannotRead(t *testing.T) {
	t.Parallel()
	handled, code := Dispatch([]string{balancerMode})
	if !handled {
		t.Error("Dispatch() with the agent's own mode = not handled, want it handled")
	}
	if code != 2 {
		t.Errorf("Dispatch() with no configuration = %d, want 2", code)
	}
	handled, code = Dispatch([]string{balancerMode, "{not json"})
	if !handled {
		t.Error("Dispatch() with an unreadable configuration = not handled, want it handled")
	}
	if code != 2 {
		t.Errorf("Dispatch() with an unreadable configuration = %d, want 2", code)
	}
}

func TestTheAgentRefusesAConfigurationItCannotServe(t *testing.T) {
	t.Parallel()
	document, err := json.Marshal(testConfig(PolicyRoundRobin))
	if err != nil {
		t.Fatalf("the configuration could not be written: %v", err)
	}
	handled, code := Dispatch([]string{balancerMode, string(document)})
	if !handled {
		t.Error("Dispatch() with an unservable configuration = not handled, want it handled")
	}
	if code != 1 {
		t.Errorf("Dispatch() with an unservable configuration = %d, want 1", code)
	}
}

func TestTheStatusIsNotRewrittenFasterThanTheChecks(t *testing.T) {
	t.Parallel()
	fast := testLimits()
	if got := reportInterval(fast); got < fast.HealthInterval {
		t.Errorf("the report interval is %s, want no faster than the %s health interval",
			got, fast.HealthInterval)
	}
	slow := DefaultLimits()
	slow.HealthInterval = 5 * time.Second
	if got := reportInterval(slow); got != slow.HealthInterval {
		t.Errorf("the report interval is %s, want the %s health interval a slow balancer checks on",
			got, slow.HealthInterval)
	}
}

func TestAStatusKeepsWhatItWasBuiltWith(t *testing.T) {
	t.Parallel()
	status := filepath.Join(t.TempDir(), "balancer.json")
	for _, policy := range []Policy{PolicyRoundRobin, PolicyLeastConnections, PolicyConsistentHash} {
		config := testConfig(policy, "10.42.0.2:7000", "10.42.0.3:7000")
		config.StatusPath = status
		document, err := json.Marshal(config)
		if err != nil {
			t.Fatalf("the configuration could not be written: %v", err)
		}
		var read Config
		if err := json.Unmarshal(document, &read); err != nil {
			t.Fatalf("the configuration could not be read back: %v", err)
		}
		if read.Policy != policy {
			t.Errorf("the policy in a written configuration is %q, want %q", read.Policy, policy)
		}
		if len(read.Backends) != 2 {
			t.Errorf("the configuration carries %d backends, want 2", len(read.Backends))
		}

	}
}

func TestAStatusCarriesWhereEachClientWent(t *testing.T) {
	t.Parallel()
	backends := []*backendServer{startBackend(t, "a"), startBackend(t, "b"), startBackend(t, "c")}
	status := filepath.Join(t.TempDir(), "balancer.json")
	config := testConfig(PolicyRoundRobin, backends[0].address, backends[1].address, backends[2].address)
	config.StatusPath = status
	balancer := startBalancer(t, config)
	for index := range 6 {
		if served := servedBy(t, balancer, "payload "+strings.Repeat("x", index)); served == "" {
			t.Fatalf("client %d was served by no backend", index)
		}
	}

	writeStatus(balancer.config, balancer.Status())

	read, err := ReadStatus(status)
	if err != nil {
		t.Fatalf("ReadStatus() = %v, want nil", err)
	}
	accepted := 0
	for _, entry := range read.Backends {
		accepted += entry.Accepted
	}
	if accepted != 6 {
		t.Errorf("the status reports %d connections placed in total, want 6", accepted)
	}
	if read.Stats.Served != 6 {
		t.Errorf("served = %d, want 6", read.Stats.Served)
	}
}
