package loadbalancer

import (
	"errors"
	"testing"
	"time"
)

func backendEntry(balancer *Balancer, index int) *backend {
	balancer.mu.Lock()
	defer balancer.mu.Unlock()
	return balancer.backends[index]
}

func reportOf(balancer *Balancer, index int) Backend {
	balancer.mu.Lock()
	defer balancer.mu.Unlock()
	return balancer.snapshot(balancer.backends[index])
}

func TestABackendThatAnswersIsCheckedAndReported(t *testing.T) {
	t.Parallel()
	backend := startBackend(t, "a")
	balancer := startBalancer(t, testConfig(PolicyRoundRobin, backend.address))

	eventually(t, "the first health check", func() bool { return reportOf(balancer, 0).Checks > 0 })
	report := reportOf(balancer, 0)
	if report.State != StateHealthy {
		t.Errorf("state = %s, want %s for a backend that answers every check", report.State, StateHealthy)
	}
	if report.FailedChecks != 0 {
		t.Errorf("failed checks = %d, want 0", report.FailedChecks)
	}
	if report.LastError != "" {
		t.Errorf("last error = %q, want none for a backend that is answering", report.LastError)
	}
	if report.LastChecked.IsZero() {
		t.Error("last checked is unset, want the time of the check a laboratory reads")
	}
	if report.Latency <= 0 {
		t.Errorf("latency = %s, want the cost of the exchange that was made", report.Latency)
	}
	if report.DrainingSince != (time.Time{}) {
		t.Errorf("draining since = %s, want unset for a backend that is serving", report.DrainingSince)
	}
	if stats := balancer.Stats(); stats.Healthy != 1 || stats.Checks == 0 || stats.FailedChecks != 0 {
		t.Errorf("stats = %+v, want one healthy backend and its checks counted", stats)
	}
}

func TestABackendThatStoppedAnsweringIsTakenOutOfService(t *testing.T) {
	t.Parallel()
	silent := startSilentBackend(t, "a")
	working := startBackend(t, "b")
	balancer := startBalancer(t, testConfig(PolicyRoundRobin, silent.address, working.address))

	eventually(t, "the silent backend to leave service", func() bool {
		return reportOf(balancer, 0).State == StateDraining
	})
	report := reportOf(balancer, 0)
	if report.FailedChecks < balancer.Limits().FailureThreshold {
		t.Errorf("failed checks = %d, want at least the threshold of %d",
			report.FailedChecks, balancer.Limits().FailureThreshold)
	}
	if report.DrainingSince.IsZero() {
		t.Error("draining since is unset, want the moment it left service")
	}
	if report.LastError == "" {
		t.Error("last error is empty, want what the check found")
	}
	if got := reportOf(balancer, 1).State; got != StateHealthy {
		t.Errorf("the backend that answers is %s, want %s", got, StateHealthy)
	}
	if stats := balancer.Stats(); stats.Draining != 1 || stats.Healthy != 1 {
		t.Errorf("stats = %+v, want one draining and one healthy backend", stats)
	}

	if served := servedBy(t, balancer, "still answered"); served != "b" {
		t.Errorf("the client was served by %q, want the backend that is still answering", served)
	}
}

func TestACheckThatIsNeverAnsweredCostsOnlyItsOwnBackend(t *testing.T) {
	t.Parallel()
	silent := startSilentBackend(t, "a")
	working := startBackend(t, "b")
	balancer := startBalancer(t, testConfig(PolicyRoundRobin, silent.address, working.address))

	limits := balancer.Limits()
	window := 10 * limits.HealthInterval
	if window <= 3*limits.HealthTimeout {
		t.Fatalf("a pass of %s with a timeout of %s leaves no room to tell the two apart",
			limits.HealthInterval, limits.HealthTimeout)
	}
	eventually(t, "the working backend to be checked through several passes", func() bool {
		return reportOf(balancer, 1).Checks >= 3
	})
	if checks := reportOf(balancer, 1).Checks; checks < 3 {
		t.Errorf("the working backend was checked %d times, want at least 3 within %s", checks, window)
	}
	if reportOf(balancer, 0).FailedChecks < 2 {
		t.Errorf("the silent backend was failed %d times, want it to keep being checked and failing",
			reportOf(balancer, 0).FailedChecks)
	}
}

func TestTheFailureThresholdIsARunOfFailures(t *testing.T) {
	t.Parallel()
	balancer := mustBalancer(t, testConfig(PolicyRoundRobin, "127.0.0.1:9201"))
	entry := backendEntry(balancer, 0)
	threshold := balancer.Limits().FailureThreshold

	for attempt := 1; attempt < threshold; attempt++ {
		balancer.recordFailure(entry, errors.New("the backend did not answer"), false)
		if got := reportOf(balancer, 0).State; got != StateHealthy {
			t.Fatalf("the backend is %s after %d of %d failures, want %s until the threshold is reached",
				got, attempt, threshold, StateHealthy)
		}
	}
	balancer.recordFailure(entry, errors.New("the backend did not answer"), false)
	if got := reportOf(balancer, 0).State; got != StateDraining {
		t.Errorf("the backend is %s after %d failures, want %s", got, threshold, StateDraining)
	}

	balancer.record(entry, nil, time.Millisecond, true)
	if report := reportOf(balancer, 0); report.Consecutive != 0 {
		t.Errorf("after one success the run of failures is %d, want 0", report.Consecutive)
	}
}

func TestAFailureFoundByLiveTrafficIsCountedApartFromTheChecks(t *testing.T) {
	t.Parallel()
	balancer := mustBalancer(t, testConfig(PolicyRoundRobin, "127.0.0.1:9202"))
	entry := backendEntry(balancer, 0)
	before := reportOf(balancer, 0)

	balancer.recordFailure(entry, errors.New("the connection to the backend was closed"), false)

	report := reportOf(balancer, 0)
	if report.Consecutive != 1 {
		t.Errorf("consecutive failures = %d, want the one that live traffic found", report.Consecutive)
	}
	if report.Checks != before.Checks || report.FailedChecks != before.FailedChecks {
		t.Errorf("checks went from %d to %d and failed checks from %d to %d, want both unchanged: "+
			"nothing was checked", before.Checks, report.Checks, before.FailedChecks, report.FailedChecks)
	}
	if report.LastError == "" {
		t.Error("last error is empty, want what the traffic found")
	}
}

func TestRecoveryTakesMoreThanOneSuccess(t *testing.T) {
	t.Parallel()
	balancer := mustBalancer(t, testConfig(PolicyRoundRobin, "127.0.0.1:9203"))
	entry := backendEntry(balancer, 0)
	limits := balancer.Limits()

	balancer.recordFailure(entry, errors.New("not answering"), false)
	balancer.recordFailure(entry, errors.New("not answering"), false)
	if got := reportOf(balancer, 0).State; got != StateDraining {
		t.Fatalf("state = %s, want %s", got, StateDraining)
	}

	balancer.record(entry, nil, 2*time.Millisecond, true)
	if report := reportOf(balancer, 0); report.State != StateDraining {
		t.Errorf("state = %s after one success, want %s: recovery takes %d successes",
			report.State, StateDraining, limits.RecoveryThreshold)
	}

	balancer.recordFailure(entry, errors.New("not answering"), false)
	balancer.record(entry, nil, time.Millisecond, true)
	if report := reportOf(balancer, 0); report.State != StateDraining {
		t.Errorf("state = %s after a failure and then a success, want %s", report.State, StateDraining)
	}
	for range limits.RecoveryThreshold {
		balancer.record(entry, nil, 3*time.Millisecond, true)
	}
	report := reportOf(balancer, 0)
	if report.State != StateHealthy {
		t.Errorf("state = %s after %d successes, want %s", report.State, limits.RecoveryThreshold, StateHealthy)
	}
	if report.Recoveries != 1 {
		t.Errorf("recoveries = %d, want 1", report.Recoveries)
	}
	if !report.DrainingSince.IsZero() {
		t.Errorf("draining since = %s, want unset once it is serving again: a deadline set before it "+
			"recovered would close work it can now answer", report.DrainingSince)
	}
	if report.LastError != "" {
		t.Errorf("last error = %q, want none once it is answering", report.LastError)
	}
	if report.Latency != 3*time.Millisecond {
		t.Errorf("latency = %s, want the cost of the last exchange", report.Latency)
	}
}

func TestABackendThatComesBackIsUsedAgain(t *testing.T) {
	t.Parallel()
	backend := startBackend(t, "a")
	balancer := startBalancer(t, testConfig(PolicyRoundRobin, backend.address))

	if served := servedBy(t, balancer, "before"); served != "a" {
		t.Fatalf("the client was served by %q, want a", served)
	}
	backend.stop()
	eventually(t, "the backend to leave service", func() bool {
		return reportOf(balancer, 0).State == StateDraining
	})
	backend.restart(t)

	eventually(t, "the backend to be put back into service", func() bool {
		return reportOf(balancer, 0).State == StateHealthy
	})
	if report := reportOf(balancer, 0); report.Recoveries != 1 {
		t.Errorf("recoveries = %d, want 1", report.Recoveries)
	}
	if served := servedBy(t, balancer, "after"); served != "a" {
		t.Errorf("the client was served by %q, want a once the backend was back", served)
	}
}

func TestADrainingBackendKeepsTheConnectionsItAlreadyHolds(t *testing.T) {
	t.Parallel()
	backend := startBackend(t, "a")
	config := testConfig(PolicyRoundRobin, backend.address)
	balancer := startBalancer(t, config)

	conn := dial(t, balancer)
	roundTrip(t, conn, "while it was serving")

	entry := backendEntry(balancer, 0)
	balancer.recordFailure(entry, errors.New("not answering"), false)
	balancer.recordFailure(entry, errors.New("not answering"), false)
	if got := reportOf(balancer, 0).State; got != StateDraining {
		t.Fatalf("state = %s, want %s", got, StateDraining)
	}
	if report := reportOf(balancer, 0); report.Active != 1 {
		t.Errorf("active = %d, want 1: a connection that is already running is not cut off", report.Active)
	}

	for range 3 {
		answer := roundTrip(t, conn, "still talking")
		if answer != "a|still talking" {
			t.Fatalf("answer = %q, want the backend to keep answering the session it holds", answer)
		}
	}
	if report := reportOf(balancer, 0); report.Drained != 0 {
		t.Errorf("drained = %d, want 0 before the deadline", report.Drained)
	}
	if report := reportOf(balancer, 0); report.Active != 1 {
		t.Errorf("active = %d, want the connection still held after the exchange", report.Active)
	}
}

func TestADrainTimeoutClosesTheConnectionsItWasHolding(t *testing.T) {
	t.Parallel()
	silent := startSilentBackend(t, "a")
	config := testConfig(PolicyRoundRobin, silent.address)
	config.Limits.DrainTimeout = 250 * time.Millisecond
	balancer := startBalancer(t, config)

	conn := dial(t, balancer)

	if _, err := conn.Write(contextTODO(), []byte("hello")); err != nil {
		t.Fatalf("Write() = %v, want nil", err)
	}
	eventually(t, "the session to be placed", func() bool { return reportOf(balancer, 0).Active == 1 })
	eventually(t, "the backend to leave service", func() bool {
		return reportOf(balancer, 0).State == StateDraining
	})

	ctx, cancel := contextWithTimeout(10 * time.Second)
	defer cancel()
	if _, err := conn.Read(ctx); err == nil {
		t.Fatal("Read() after the drain timeout = nil, want the connection to have been closed")
	}

	eventually(t, "the drained connection to be given back", func() bool {
		return reportOf(balancer, 0).Active == 0 && balancer.Stats().Active == 0
	})
	if report := reportOf(balancer, 0); report.Drained != 1 {
		t.Errorf("drained = %d, want 1: the balancer closed this connection at the deadline", report.Drained)
	}
}

func TestAnExpiredDrainIsNotCountedTwice(t *testing.T) {
	t.Parallel()

	silent := startSilentBackend(t, "a")
	config := testConfig(PolicyRoundRobin, silent.address)
	config.Limits.DrainTimeout = 150 * time.Millisecond
	balancer := startBalancer(t, config)

	conn := dial(t, balancer)
	if _, err := conn.Write(contextTODO(), []byte("one connection")); err != nil {
		t.Fatalf("Write() = %v, want nil", err)
	}
	eventually(t, "the session to be placed", func() bool { return reportOf(balancer, 0).Active == 1 })
	eventually(t, "the drain to close the connection at its deadline", func() bool {
		return reportOf(balancer, 0).Drained == 1 && reportOf(balancer, 0).Active == 0
	})
	if report := reportOf(balancer, 0); report.Active != 0 {
		t.Errorf("active = %d, want 0 once the drain has closed it", report.Active)
	}

	time.Sleep(4 * config.Limits.HealthInterval)
	after := reportOf(balancer, 0)
	if after.Drained != 1 {
		t.Errorf("drained = %d after several more ticks, want 1: a consumed deadline closes nothing again",
			after.Drained)
	}
	if after.State != StateDraining {
		t.Errorf("state = %s, want %s: a backend that is still not answering stays out of service",
			after.State, StateDraining)
	}
	_ = conn.Close()
}

func TestAServingBackendHoldsNoDrainDeadline(t *testing.T) {
	t.Parallel()
	balancer := mustBalancer(t, testConfig(PolicyRoundRobin, "127.0.0.1:9204"))
	entry := backendEntry(balancer, 0)
	balancer.expireDrains()
	balancer.mu.Lock()
	until := entry.drainUntil
	balancer.mu.Unlock()
	if !until.IsZero() {
		t.Errorf("drain deadline = %s on a backend that is serving, want none", until)
	}
}
