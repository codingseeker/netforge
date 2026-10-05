package transport

import "time"

// congestion is the sender's window controller.
//
// It is a pure state machine with no timers and no sockets, so the behaviour
// that matters can be tested by feeding it acknowledgements and timeouts
// directly. A network test can only observe the shape of the curve; this is
// where the curve is decided.
//
// The scheme is the Reno one, because it is the smallest thing that does the
// right thing under loss: grow while the path is healthy, and when loss is
// inferred, halve and back off. Loss is inferred from a retransmission
// timeout, which is the only signal UDP gives.
type congestion struct {
	// window is the most unacknowledged segments allowed in flight.
	window uint32
	// ceiling is the most this connection may ever have in flight, which is
	// the limit the caller agreed to and is not a congestion decision.
	ceiling uint32
	// ssthresh is where slow stop ends and congestion avoidance begins.
	ssthresh uint32
	// inRecovery reports whether the window has been cut and is climbing back.
	inRecovery bool
	// recoveryAt is the sequence number the sender is recovering up to. Every
	// retransmission timeout sets it, so a second timeout before the first
	// recovery finishes does not cut the window again: loss during recovery is
	// the same loss, and cutting twice would collapse the connection instead
	// of slowing it down.
	recoveryAt uint32
	// sent is how many segments have been sent, which is what the round trip
	// time is measured from.
	sent uint32
	// acknowledged is how many have been confirmed.
	acknowledged uint32
	// timeouts counts the retransmission timeouts, which is the sender's own
	// measure of how lossy the path is.
	timeouts int
	// rtt is the most recent round trip measurement.
	rtt time.Duration
	// srtt is the smoothed round trip, which is what the retransmission
	// timeout is derived from.
	srtt time.Duration
	// avoided counts acknowledgements in congestion avoidance, because the
	// window grows by one segment per window acknowledged rather than per
	// acknowledgement. Adding one per acknowledgement would be slow start's
	// rate with slow start's name on it, which is how a path that is merely
	// slow gets flooded.
	avoided uint32
	// measured reports whether a round trip has been measured yet.
	measured bool
	// start is the first sequence number, so a fresh connection begins slow.
	start uint32
}

func newCongestion(limits Limits) *congestion {
	// A fresh connection starts with a small window. Assuming a fat pipe before
	// anything has been measured is how a transport floods a path that has
	// never carried anything.
	window := uint32(2)
	ceiling := uint32(limits.MaxWindow)
	if ceiling < window {
		window = ceiling
	}
	return &congestion{
		window:   window,
		ceiling:  ceiling,
		ssthresh: ceiling,
		start:    1,
	}
}

// inFlight is how many segments are unacknowledged. It cannot go below zero:
// the total acknowledged is the total sent less whatever is still out, and if
// the two ever disagree the answer is that nothing is in flight. Reporting a
// wrapped-around number instead would leave a sender waiting for a window that
// four billion phantom segments were holding, with nothing to move it.
func (c *congestion) inFlight() uint32 {
	if c.acknowledged >= c.sent {
		return 0
	}
	return c.sent - c.acknowledged
}

// canSend reports whether another segment may go out, which is what keeps the
// sender inside the limit the caller set as well as the window the path has
// earned.
func (c *congestion) canSend() bool {
	return c.inFlight() < c.window && c.window <= c.ceiling
}

// sent records that a segment went out, so its round trip can be measured and
// so the window knows how much is outstanding.
func (c *congestion) sentOne() { c.sent++ }

// acknowledged records a confirmed delivery. The window grows by one per
// acknowledged segment during congestion avoidance, and by the number
// acknowledged while the path is still believed healthy, which is slow start.
func (c *congestion) acknowledgedOne(sequence uint32) {
	if sequence > c.acknowledged {
		c.acknowledged = sequence
	}
	if c.inRecovery {
		if c.acknowledged >= c.recoveryAt {
			// The path has confirmed delivery past the loss, so it is healthy
			// again.
			c.inRecovery = false
		}
		return
	}
	if c.window < c.ssthresh {
		c.grow(c.window)
		return
	}
	// Congestion avoidance adds one segment per window acknowledged, which is
	// slow enough not to flood a path that is merely slow.
	c.avoided++
	if c.avoided >= c.window {
		c.avoided = 0
		c.grow(1)
	}
}

// grow raises the window without passing the ceiling.
func (c *congestion) grow(by uint32) {
	c.window += by
	if c.window > c.ceiling {
		c.window = c.ceiling
	}
}

// timedOut records a retransmission timeout, which is the only loss signal UDP
// provides. The window is halved and slow start is over.
func (c *congestion) timedOut(sequence uint32) {
	c.timeouts++
	if !c.inRecovery || sequence > c.recoveryAt {
		// A loss that is not the one already being recovered from is a new
		// loss, and the window is cut once for it.
		half := c.window / 2
		if half < 2 {
			half = 2
		}
		c.ssthresh = half
		c.window = half
		c.avoided = 0
		c.inRecovery = true
		c.recoveryAt = c.sent
	}
}

// measure records a round trip and smooths it. The retransmission timeout is
// derived from the smoothed value rather than from the last sample, so one
// slow sample does not stretch the timeout and a burst of fast samples does not
// shrink it below the path's real delay.
func (c *congestion) measure(sample time.Duration) {
	if sample <= 0 {
		return
	}
	c.rtt = sample
	if !c.measured {
		c.srtt = sample
		c.measured = true
		return
	}
	// A weighted mean, with the new sample at one eighth so a single outlier
	// moves the estimate rather than replacing it.
	c.srtt = (7*c.srtt + sample) / 8
}

// timeout is how long to wait before sending a segment again. It is derived
// from the measured round trip and held between the caller's floor and
// ceiling, so a path that answered quickly once does not cause a burst of
// retransmissions that the path itself then has to carry, and one very slow
// sample cannot stretch the timeout past what the caller agreed to wait.
func (c *congestion) timeout(floor, ceiling time.Duration) time.Duration {
	if !c.measured {
		return floor
	}
	// Four times the smoothed round trip: enough to absorb the variation a
	// queue introduces, small enough that a lost segment is not held for long.
	wait := 4 * c.srtt
	if wait < floor {
		wait = floor
	}
	if wait > ceiling {
		wait = ceiling
	}
	return wait
}

// rttVariance reports how much the round trip moved between measurements, which
// is what jitter is on this path.
func (c *congestion) rttVariance() time.Duration {
	if !c.measured || c.rtt <= c.srtt {
		return 0
	}
	return c.rtt - c.srtt
}

// stats reports the controller's state for the connection's counters and the
// benchmark. These are the sender's own numbers about the path, not a
// configured value: nothing here was asked for, it was observed.
type congestionStats struct {
	Window     uint32
	Ceiling    uint32
	SlowStart  bool
	InRecovery bool
	Timeouts   int
	RTT        time.Duration
	SRTT       time.Duration
	Jitter     time.Duration
	Measured   bool
}

func (c *congestion) stats() congestionStats {
	return congestionStats{
		Window:     c.window,
		Ceiling:    c.ceiling,
		SlowStart:  !c.inRecovery && c.window < c.ssthresh,
		InRecovery: c.inRecovery,
		Timeouts:   c.timeouts,
		RTT:        c.rtt,
		SRTT:       c.srtt,
		Jitter:     c.rttVariance(),
		Measured:   c.measured,
	}
}
