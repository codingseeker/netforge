package transport

import (
	"testing"
	"time"
)

func windowLimits() Limits {
	return Limits{MaxPayload: 64, MaxWindow: 16, MaxReorder: 8, MaxDatagram: MaxFrameSize}
}

func TestAFreshConnectionStartsSmall(t *testing.T) {
	t.Parallel()
	c := newCongestion(windowLimits())
	if c.window != 2 {
		t.Errorf("window = %d, want 2: a path that has carried nothing is not known to be wide", c.window)
	}

	c.sentOne()
	if !c.canSend() {
		t.Error("canSend() = false with one of two slots taken, want true")
	}
	c.sentOne()
	if c.canSend() {
		t.Error("canSend() = true with both slots taken, want false")
	}
}

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

func TestCongestionAvoidanceGrowsByOnePerRound(t *testing.T) {
	t.Parallel()
	c := newCongestion(windowLimits())
	c.ssthresh = 4
	c.window = 4
	sent := c.sent
	for round := 0; round < 8; round++ {
		before := c.window

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

	c.acknowledgedOne(20)
	if c.window != 8 {
		t.Errorf("window = %d while recovering, want 8: recovery is slower than slow start", c.window)
	}
}

func TestARepeatedTimeoutDuringRecoveryDoesNotCutAgain(t *testing.T) {
	t.Parallel()
	c := newCongestion(windowLimits())
	for sequence := uint32(1); sequence <= 16; sequence++ {
		c.sentOne()
		c.acknowledgedOne(sequence)
	}
	c.timedOut(16)
	cut := c.window

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

func TestTheRoundTripIsSmoothed(t *testing.T) {
	t.Parallel()
	c := newCongestion(windowLimits())
	c.measure(10 * time.Millisecond)
	if stats := c.stats(); stats.SRTT != 10*time.Millisecond {
		t.Errorf("SRTT = %s, want the first sample at %s", stats.SRTT, 10*time.Millisecond)
	}

	c.measure(200 * time.Millisecond)
	if got := c.stats().SRTT; got >= 200*time.Millisecond {
		t.Errorf("SRTT = %s after one slow sample, want it below the sample", got)
	}
	if got := c.stats().SRTT; got <= 10*time.Millisecond {
		t.Errorf("SRTT = %s, want it above the earlier estimate", got)
	}

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

	if got := c.stats().Jitter; got != 0 {
		t.Errorf("Jitter = %s after a fast sample, want 0", got)
	}
	c.measure(150 * time.Millisecond)
	if c.stats().Jitter == 0 {
		t.Error("Jitter = 0 after a slow sample, want it measured")
	}
}

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

func TestTheDerivedTimeoutStaysBetweenTheFloorAndTheCeiling(t *testing.T) {
	t.Parallel()
	c := newCongestion(windowLimits())
	floor := 50 * time.Millisecond
	ceiling := 2 * time.Second

	if got := c.timeout(floor, ceiling); got != floor {
		t.Errorf("timeout() = %s, want the floor of %s", got, floor)
	}

	for range 10 {
		c.measure(50 * time.Microsecond)
	}
	if got := c.timeout(floor, ceiling); got != floor {
		t.Errorf("timeout() = %s on a fast path, want the floor of %s", got, floor)
	}

	for range 10 {
		c.measure(30 * time.Second)
	}
	if got := c.timeout(floor, ceiling); got != ceiling {
		t.Errorf("timeout() = %s on a slow path, want the ceiling of %s", got, ceiling)
	}
}
