package transport

import "time"

type congestion struct {
	window uint32

	ceiling uint32

	ssthresh uint32

	inRecovery bool

	recoveryAt uint32

	sent uint32

	acknowledged uint32

	timeouts int

	rtt time.Duration

	srtt time.Duration

	avoided uint32

	measured bool

	start uint32
}

func newCongestion(limits Limits) *congestion {

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

func (c *congestion) inFlight() uint32 {
	if c.acknowledged >= c.sent {
		return 0
	}
	return c.sent - c.acknowledged
}

func (c *congestion) canSend() bool {
	return c.inFlight() < c.window && c.window <= c.ceiling
}

func (c *congestion) sentOne() { c.sent++ }

func (c *congestion) acknowledgedOne(sequence uint32) {
	if sequence > c.acknowledged {
		c.acknowledged = sequence
	}
	if c.inRecovery {
		if c.acknowledged >= c.recoveryAt {

			c.inRecovery = false
		}
		return
	}
	if c.window < c.ssthresh {
		c.grow(c.window)
		return
	}

	c.avoided++
	if c.avoided >= c.window {
		c.avoided = 0
		c.grow(1)
	}
}

func (c *congestion) grow(by uint32) {
	c.window += by
	if c.window > c.ceiling {
		c.window = c.ceiling
	}
}

func (c *congestion) timedOut(sequence uint32) {
	c.timeouts++
	if !c.inRecovery || sequence > c.recoveryAt {

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

	c.srtt = (7*c.srtt + sample) / 8
}

func (c *congestion) timeout(floor, ceiling time.Duration) time.Duration {
	if !c.measured {
		return floor
	}

	wait := 4 * c.srtt
	if wait < floor {
		wait = floor
	}
	if wait > ceiling {
		wait = ceiling
	}
	return wait
}

func (c *congestion) rttVariance() time.Duration {
	if !c.measured || c.rtt <= c.srtt {
		return 0
	}
	return c.rtt - c.srtt
}

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
