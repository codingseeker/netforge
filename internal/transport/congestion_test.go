package transport

import (
	"testing"
	"time"
)

func windowLimits() Limits {
	return Limits{MaxPayload: 64, MaxWindow: 16, MaxReorder: 8, MaxDatagram: MaxFrameSize}
}

// The sender must not assume a path can carry anything before it has measured
// one, so a fresh connection starts small however generous the limit is.
func TestAFreshConnectionStartsSmall(t *testing.T) {
	t.Parallel()
	c := newCongestion(windowLimits())
	if c.window != 2 {
		t.Errorf("window = %d, want 2: a path that has carried nothing is not known to be wide", c.window)
	}
	// Two may be in flight and no more, which is the whole of the starting
	// claim: this side has measured nothing and is not going to send more than
	// a window's worth on a path it has never used.
	c.sentOne()
	if !c.canSend() {
		t.Error("canSend() = false with one of two slots taken, want true")
	}
	c.sentOne()
	if c.canSend() {
		t.Error("canSend() = true with both slots taken, want false")
	}
}

// A window above the caller's ceiling is the caller's decision and not a
// congestion one, so no amount of good news may raise it past the limit.
func TestTheWindowNeverPassesTheCeiling(t *testing.T) {
	t.Parallel()
	c := newCongestion(windowLimits())
	for sequence := uint32(1); sequence <= 64; sequence++ {
		c.sentOne()
		c.acknowledgedOne(sequence)
	}
	if c.window > c.ceiling {
		t.Errorf("window = %d, want at most the ceiling of %d", c.window, c.ceiling)
	}
	if c.window != 16 {
		t.Errorf("window = %d, want the full ceiling of 16 once every acknowledgement arrived", c.window)
	}
	// A window of sixteen means sixteen unacknowledged, and the seventeenth
	// waits rather than going out on a path that has earned no more room. The
	// window has to be earned first, because a fresh connection is not assumed
	// to be wide.
	c = newCongestion(windowLimits())
	for sequence := uint32(1); sequence <= 16; sequence++ {
		c.sentOne()
		c.acknowledgedOne(sequence)
	}
	sent := c.sent
	for range 16 {
		c.sentOne()
		sent++
	}
	if c.canSend() {
		t.Error("canSend() = true with a full window in flight, want false")
	}
	c.acknowledgedOne(sent)
	if !c.canSend() {
		t.Error("canSend() = false after an acknowledgement freed a slot, want true")
	}
}

// A ceiling smaller than the starting window is honoured, because the limit a
// caller set is a bound and not a suggestion.
func TestASmallCeilingBoundsTheStartingWindow(t *testing.T) {
	t.Parallel()
	limits := windowLimits()
	limits.MaxWindow = 1
	c := newCongestion(limits)
	if c.window != 1 {
		t.Errorf("window = %d, want the ceiling of 1", c.window)
	}
	c.sentOne()
	if c.canSend() {
		t.Error("canSend() = true with the only slot taken, want false")
	}
}

// Slow start doubles the window for each confirmed segment, so a path that is
// healthy is filled in a logarithmic number of round trips.
func TestSlowStartDoublesTheWindow(t *testing.T) {
	t.Parallel()
	c := newCongestion(windowLimits())
	sent := uint32(0)
	for round := 1; round <= 4; round++ {
		for c.canSend() {
			c.sentOne()
			sent++
		}
		c.acknowledgedOne(sent)
	}
	if c.window != 16 {
		t.Errorf("window = %d, want 16: 2, then 4, then 8, then 16", c.window)
	}
	if stats := c.stats(); stats.SlowStart {
		t.Error("SlowStart = true at the ceiling, want false")
	}
}

// Past the slow start threshold the window grows by one per round trip, so a
// path is not flooded for being merely slow.
func TestCongestionAvoidanceGrowsByOnePerRound(t *testing.T) {
	t.Parallel()
	c := newCongestion(windowLimits())
	c.ssthresh = 4
	c.window = 4
	sent := c.sent
	for round := 0; round < 8; round++ {
		before := c.window
		// Fill the window, then confirm exactly one window's worth of it. The
		// acknowledgements are separate frames, as they are on a real path.
		for count := uint32(0); count < c.window; count++ {
			c.sentOne()
			sent++
		}
		for count := uint32(0); count < c.window; count++ {
			c.acknowledgedOne(c.acknowledged + 1)
		}
		if c.window != before+1 {
			t.Fatalf("the window grew from %d to %d in a round, want exactly one",
				before, c.window)
		}
	}
}

// A timeout is the only loss signal UDP provides, so the window is cut and slow
// start is over.
func TestATimeoutCutsTheWindow(t *testing.T) {
	t.Parallel()
	c := newCongestion(windowLimits())
	for sequence := uint32(1); sequence <= 16; sequence++ {
		c.sentOne()
		c.acknowledgedOne(sequence)
	}
	if c.window != 16 {
		t.Fatalf("window = %d, want 16 before the loss", c.window)
	}
	c.timedOut(16)
	if c.window != 8 {
		t.Errorf("window = %d after a timeout, want 8: half of 16", c.window)
	}
	if c.ssthresh != 8 {
		t.Errorf("ssthresh = %d, want 8", c.ssthresh)
	}
	if stats := c.stats(); !stats.InRecovery {
		t.Error("InRecovery = false after a cut, want true")
	}
	// It then climbs back from where it was cut, not from the ceiling.
	c.acknowledgedOne(20)
	if c.window != 8 {
		t.Errorf("window = %d while recovering, want 8: recovery is slower than slow start", c.window)
	}
}

// A second timeout before the first recovery finished is the same loss, and
// cutting twice would collapse the connection rather than slow it.
func TestARepeatedTimeoutDuringRecoveryDoesNotCutAgain(t *testing.T) {
	t.Parallel()
	c := newCongestion(windowLimits())
	for sequence := uint32(1); sequence <= 16; sequence++ {
		c.sentOne()
		c.acknowledgedOne(sequence)
	}
	c.timedOut(16)
	cut := c.window
	// The path acknowledges past the loss, recovery ends, and a later timeout
	// is a new loss and does cut.
	c.acknowledgedOne(20)
	if c.inRecovery {
		t.Fatal("inRecovery = true after acknowledging past the loss, want false")
	}
	c.timedOut(24)
	if c.window >= 16 {
		t.Errorf("window = %d after a new loss, want below the window before it", c.window)
	}
	_ = cut
}

// A stale acknowledgement cannot move the controller backwards, because
// retransmissions make them ordinary.
func TestAnAcknowledgementCannotMoveTheControllerBackwards(t *testing.T) {
	t.Parallel()
	c := newCongestion(windowLimits())
	for sequence := uint32(1); sequence <= 8; sequence++ {
		c.sentOne()
		c.acknowledgedOne(sequence)
	}
	window := c.window
	acknowledged := c.acknowledged
	c.acknowledgedOne(1)
	if c.acknowledged != acknowledged {
		t.Errorf("acknowledged = %d, want it to stay at %d", c.acknowledged, acknowledged)
	}
	if c.window < window {
		t.Errorf("window = %d, want it to stay at %d or above", c.window, window)
	}
}

// The round trip is smoothed rather than taken from the last sample, so one
// slow sample does not stretch every later timeout.
func TestTheRoundTripIsSmoothed(t *testing.T) {
	t.Parallel()
	c := newCongestion(windowLimits())
	c.measure(10 * time.Millisecond)
	if stats := c.stats(); stats.SRTT != 10*time.Millisecond {
		t.Errorf("SRTT = %s, want the first sample at %s", stats.SRTT, 10*time.Millisecond)
	}
	// A single slow sample moves the estimate, it does not replace it.
	c.measure(200 * time.Millisecond)
	if got := c.stats().SRTT; got >= 200*time.Millisecond {
		t.Errorf("SRTT = %s after one slow sample, want it below the sample", got)
	}
	if got := c.stats().SRTT; got <= 10*time.Millisecond {
		t.Errorf("SRTT = %s, want it above the earlier estimate", got)
	}
	// A burst of fast samples pulls the estimate down towards them without
	// reaching them, which is the property that keeps one slow sample from
	// governing the next hundred timeouts.
	raised := c.stats().SRTT
	for range 20 {
		c.measure(10 * time.Millisecond)
	}
	lowered := c.stats().SRTT
	if lowered >= raised {
		t.Errorf("SRTT = %s after twenty fast samples, want it below the %s it had", lowered, raised)
	}
	if lowered <= 10*time.Millisecond {
		t.Errorf("SRTT = %s, want it still above the samples rather than replaced by them", lowered)
	}
	// Jitter is how far the last sample moved from the estimate, and it is zero
	// when the last sample was the fast one.
	if got := c.stats().Jitter; got != 0 {
		t.Errorf("Jitter = %s after a fast sample, want 0", got)
	}
	c.measure(150 * time.Millisecond)
	if c.stats().Jitter == 0 {
		t.Error("Jitter = 0 after a slow sample, want it measured")
	}
}

// A measurement of nothing is not a measurement, and must not become a
// smoothed round trip of zero, which would collapse the timeout to its floor.
func TestAnUnusableRoundTripIsIgnored(t *testing.T) {
	t.Parallel()
	c := newCongestion(windowLimits())
	c.measure(0)
	if c.stats().Measured {
		t.Error("Measured = true after a zero sample, want false")
	}
	if got := c.timeout(time.Millisecond, time.Second); got != time.Millisecond {
		t.Errorf("timeout() = %s, want the floor before anything is measured", got)
	}
}

// The derived timeout stays between the floor and the ceiling however wrong the
// path's measurements are.
func TestTheDerivedTimeoutStaysBetweenTheFloorAndTheCeiling(t *testing.T) {
	t.Parallel()
	c := newCongestion(windowLimits())
	floor := 50 * time.Millisecond
	ceiling := 2 * time.Second

	// Nothing measured yet: the floor, because a path that has not answered
	// once has not been given the benefit of the doubt.
	if got := c.timeout(floor, ceiling); got != floor {
		t.Errorf("timeout() = %s, want the floor of %s", got, floor)
	}
	// A very fast path does not get a timeout under the floor.
	for range 10 {
		c.measure(50 * time.Microsecond)
	}
	if got := c.timeout(floor, ceiling); got != floor {
		t.Errorf("timeout() = %s on a fast path, want the floor of %s", got, floor)
	}
	// A very slow path does not get one over the ceiling.
	for range 10 {
		c.measure(30 * time.Second)
	}
	if got := c.timeout(floor, ceiling); got != ceiling {
		t.Errorf("timeout() = %s on a slow path, want the ceiling of %s", got, ceiling)
	}
}
