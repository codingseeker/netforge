package loadbalancer

import (
	"errors"
	"testing"
	"time"
)

// backendEntry is a registered backend by index, for the tests that exercise the
// state machine directly rather than waiting for a health check to reach it.
func backendEntry(balancer *Balancer, index int) *backend {
	balancer.mu.Lock()
	defer balancer.mu.Unlock()
	return balancer.backends[index]
}

// reportOf is a backend's own report by index.
func reportOf(balancer *Balancer, index int) Backend {
	balancer.mu.Lock()
	defer balancer.mu.Unlock()
	return balancer.snapshot(balancer.backends[index])
}

// A backend that answers is checked on the interval and reported as serving, with
// the latency of the exchange recorded, because a balancer that never checked
// anything would report the same for a backend that was answering and one that was
// not.
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

// A backend that stops answering leaves service once it has failed as many times
// as the threshold names, and the report says when it left, so a laboratory can
// tell a backend that just failed from one that has been out for a while.
func TestABackendThatStoppedAnsweringIsTakenOutOfService(t *testing.T) {
	t.Parallel()
	silent := startSilentBackend(t, "a")
	working := startBackend(t, "b")
	balancer := startBalancer(t, testConfig(PolicyRoundRobin, silent.address, working.address))

	// The working backend is a floor under the test: with one silent backend the
	// balancer is never out of backends, so the draining below is a decision
	// about the silent one rather than a refusal of everything.
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
	// The client that arrives now is served by the one that is still answering,
	// because a balancer that gave up on everything would be a different thing.
	if served := servedBy(t, balancer, "still answered"); served != "b" {
		t.Errorf("the client was served by %q, want the backend that is still answering", served)
	}
}

// A check is a real exchange with its own deadline, and one backend that never
// answers must not cost the others their checks. The interval here is longer than
// the timeout, so a balancer that checked its backends in turn would spend every
// pass waiting on the silent one and would have checked the working one hardly at
// all.
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

// A single failure is not enough, and a threshold is a run of failures rather than
// a total: a backend that fails once in a long while still has to leave service,
// and one that failed once an hour ago is not out of service this minute.
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
	// A success breaks the run of failures even while the backend is out of
	// service, so a backend that comes back is not already at its threshold from
	// the failures that took it out. It is not serving yet either: recovery has a
	// threshold of its own, which the recovery test below is about.
	balancer.record(entry, nil, time.Millisecond, true)
	if report := reportOf(balancer, 0); report.Consecutive != 0 {
		t.Errorf("after one success the run of failures is %d, want 0", report.Consecutive)
	}
}

// A failure that live traffic found is evidence too, and it is counted apart from
// the checks, so a laboratory can tell traffic that broke from a check that failed
// while nothing was being sent.
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

// A backend that comes back is put back into service only after it has answered
// as many times as the recovery threshold names, and a backend that answers once
// and fails again is not a backend that recovered.
func TestRecoveryTakesMoreThanOneSuccess(t *testing.T) {
	t.Parallel()
	balancer := mustBalancer(t, testConfig(PolicyRoundRobin, "127.0.0.1:9203"))
	entry := backendEntry(balancer, 0)
	limits := balancer.Limits()
	// Two failures in a row, which is a run, take it out of service.
	balancer.recordFailure(entry, errors.New("not answering"), false)
	balancer.recordFailure(entry, errors.New("not answering"), false)
	if got := reportOf(balancer, 0).State; got != StateDraining {
		t.Fatalf("state = %s, want %s", got, StateDraining)
	}
	// One success is not a recovery.
	balancer.record(entry, nil, 2*time.Millisecond, true)
	if report := reportOf(balancer, 0); report.State != StateDraining {
		t.Errorf("state = %s after one success, want %s: recovery takes %d successes",
			report.State, StateDraining, limits.RecoveryThreshold)
	}
	// One failure resets the run of successes, so a backend that is failing again
	// cannot recover one answer at a time.
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

// A backend that has gone away and comes back is put back into service by its own
// health checks, and clients reach it again afterwards, with the recovery counted.
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

// A backend that is draining keeps the conversations it already has, because
// cutting off work that is still arriving is the one thing a load balancer must not
// do, and the drain timeout is the deadline that stops the patience being for ever.
func TestADrainingBackendKeepsTheConnectionsItAlreadyHolds(t *testing.T) {
	t.Parallel()
	backend := startBackend(t, "a")
	config := testConfig(PolicyRoundRobin, backend.address)
	balancer := startBalancer(t, config)

	conn := dial(t, balancer)
	roundTrip(t, conn, "while it was serving")

	// The backend is taken out of service by two failures, which is the same thing
	// a failed check does, and it is still answering.
	entry := backendEntry(balancer, 0)
	balancer.recordFailure(entry, errors.New("not answering"), false)
	balancer.recordFailure(entry, errors.New("not answering"), false)
	if got := reportOf(balancer, 0).State; got != StateDraining {
		t.Fatalf("state = %s, want %s", got, StateDraining)
	}
	if report := reportOf(balancer, 0); report.Active != 1 {
		t.Errorf("active = %d, want 1: a connection that is already running is not cut off", report.Active)
	}
	// The backend is answering, so the conversation continues through the drain.
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

// The drain timeout is what ends a drain that the backend never finishes, and the
// connection is closed rather than left to time out on its own.
func TestADrainTimeoutClosesTheConnectionsItWasHolding(t *testing.T) {
	t.Parallel()
	silent := startSilentBackend(t, "a")
	config := testConfig(PolicyRoundRobin, silent.address)
	config.Limits.DrainTimeout = 250 * time.Millisecond
	balancer := startBalancer(t, config)

	conn := dial(t, balancer)
	// A silent backend never answers, so this exchange is what puts a session in
	// place before the drain closes it.
	if _, err := conn.Write(contextTODO(), []byte("hello")); err != nil {
		t.Fatalf("Write() = %v, want nil", err)
	}
	eventually(t, "the session to be placed", func() bool { return reportOf(balancer, 0).Active == 1 })
	eventually(t, "the backend to leave service", func() bool {
		return reportOf(balancer, 0).State == StateDraining
	})

	// The drain deadline is enforced on the health tick, so the connection is
	// closed on the first tick after it rather than exactly on it.
	ctx, cancel := contextWithTimeout(10 * time.Second)
	defer cancel()
	if _, err := conn.Read(ctx); err == nil {
		t.Fatal("Read() after the drain timeout = nil, want the connection to have been closed")
	}
	// The drain is counted under the lock and the connections it closed are
	// halted after it, so the two numbers settle a moment apart. The connection
	// is gone once the client sees the close, so waiting is waiting for the
	// balancer to finish the work it already reported doing.
	eventually(t, "the drained connection to be given back", func() bool {
		return reportOf(balancer, 0).Active == 0 && balancer.Stats().Active == 0
	})
	if report := reportOf(balancer, 0); report.Drained != 1 {
		t.Errorf("drained = %d, want 1: the balancer closed this connection at the deadline", report.Drained)
	}
}

// A drain whose deadline has been consumed does not close the same connection
// again, and a backend that stays down is not counted as having drained the same
// work on every tick after the deadline.
func TestAnExpiredDrainIsNotCountedTwice(t *testing.T) {
	t.Parallel()
	// A silent backend keeps failing, so it stays out of service and its drain
	// deadline is spent once rather than being re-armed by each failure.
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
	// Several more ticks pass, each of which finds the deadline consumed.
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

// A backend that is serving holds no deadline, so a check that happens to fail
// late in a run of successes cannot close the work it is still answering.
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
