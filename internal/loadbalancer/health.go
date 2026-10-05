package loadbalancer

import (
	"context"
	"sync"
	"time"

	"github.com/netforge/netforge/internal/transport"
)

// healthProbe is the payload one health check carries. It is fixed so a check
// costs the same bytes every time, and small so a check never competes with real
// traffic for the window it shares with it.
//
// A check is a real exchange rather than a dial, because a balancer's transport
// is datagram based: opening a socket towards an address that is not listening
// succeeds, so a check that only dialled would call a host that has been switched
// off healthy for ever. The check therefore sends this payload and requires
// something to come back inside the health timeout, which is the only evidence
// that can tell a serving backend from a reachable one.
var healthProbe = []byte("netforge.health.v1")

// watchHealth checks every backend on a timer and enforces the drain deadlines.
// One goroutine runs it for the life of the balancer, and it is stopped by
// cancelling that life, so a closed balancer leaves no check running.
func (b *Balancer) watchHealth(ctx context.Context) {
	ticker := time.NewTicker(b.limits.HealthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.checkAll(ctx)
			b.expireDrains()
			b.pollControl()
		}
	}
}

// checkAll takes one health check of every backend. The checks run at the same
// time rather than in turn, because in turn a pass costs the health timeout for
// every backend that has stopped and the backends that are serving would be
// checked that much less often. The number of checks at once is bounded by the
// backend ceiling and each one is bounded by the health timeout, so a pass cannot
// take longer than one check.
func (b *Balancer) checkAll(ctx context.Context) {
	b.mu.Lock()
	targets := make([]*backend, 0, len(b.backends))
	for _, entry := range b.backends {
		// A removed backend is not checked. It is out of service on purpose, and
		// a check that passed would either waste the budget or, worse, look like
		// a reason to serve it again.
		if entry.removed {
			continue
		}
		targets = append(targets, entry)
	}
	b.mu.Unlock()

	group := new(sync.WaitGroup)
	for _, entry := range targets {
		group.Add(1)
		go func(entry *backend) {
			defer group.Done()
			err, elapsed := b.check(ctx, entry)
			b.record(entry, err, elapsed, true)
		}(entry)
	}
	group.Wait()
}

// check takes one health check of a backend and reports what it cost. The check
// has its own context, so a backend that never answers costs the health timeout
// rather than the balancer's life, and a balancer that is closing ends the check
// as well.
func (b *Balancer) check(ctx context.Context, entry *backend) (error, time.Duration) {
	attempt, cancel := context.WithTimeout(ctx, b.limits.HealthTimeout)
	defer cancel()
	started := time.Now()
	conn, err := transport.Dial(attempt, "", entry.address, b.limits.Transport)
	if err != nil {
		return err, time.Since(started)
	}
	// The connection is closed before the check's context is cancelled, so the
	// health check's own deadline is not what tears the connection down.
	defer conn.Close()
	if _, err := conn.Write(attempt, healthProbe); err != nil {
		return err, time.Since(started)
	}
	if _, err := conn.Read(attempt); err != nil {
		return err, time.Since(started)
	}
	return nil, time.Since(started)
}

// record applies the outcome of a check or of live traffic to one backend and
// moves it between serving and draining.
//
// The runs of failures and successes are counted, not the totals: a backend that
// fails once an hour must still leave service, and a backend that answered once
// an hour ago is not serving. Recovery needs more successes than failure needs
// failures, because a backend that comes back and fails again is worse than one
// that stays out for a while.
func (b *Balancer) record(entry *backend, err error, elapsed time.Duration, fromCheck bool) {
	if entry == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if fromCheck {
		entry.stats.Checks++
		entry.stats.LastChecked = time.Now()
	}
	if err != nil {
		entry.successes = 0
		entry.failures++
		entry.stats.LastError = err.Error()
		if fromCheck {
			entry.stats.FailedChecks++
		}
		if entry.healthy && entry.failures >= b.limits.FailureThreshold {
			b.beginDrainLocked(entry)
		}
		return
	}
	entry.failures = 0
	entry.successes++
	entry.stats.LastError = ""
	entry.stats.Latency = elapsed
	if entry.removed {
		// The check is recorded, but it does not bring the backend back. A
		// removal is a decision about whether this backend should be serving, and
		// a check that says it could be is not a decision to serve it.
		return
	}
	if !entry.healthy && entry.successes >= b.limits.RecoveryThreshold {
		entry.healthy = true
		entry.successes = 0
		entry.stats.Recoveries++
		// A backend that is serving again is not holding a drain open. Its
		// connections are the ones it can answer, and a deadline set before it
		// recovered would close work it is now able to do.
		entry.drainUntil = time.Time{}
		entry.stats.DrainingSince = time.Time{}
	}
}

// recordFailure is the same accounting for a failure that live traffic found
// rather than a check.
func (b *Balancer) recordFailure(entry *backend, err error, fromCheck bool) {
	b.record(entry, err, 0, fromCheck)
}

// beginDrainLocked takes a backend out of service. The connections it already
// holds are left alone and given the drain timeout, because cutting a
// conversation off is the one thing a load balancer must not do to work that is
// still arriving; the deadline is what stops that patience from being for ever.
func (b *Balancer) beginDrainLocked(entry *backend) {
	now := time.Now()
	entry.healthy = false
	entry.successes = 0
	entry.failures = 0
	entry.drainUntil = now.Add(b.limits.DrainTimeout)
	entry.stats.DrainingSince = now
}

// expireDrains closes the connections of the backends that have been draining for
// longer than the drain timeout. It runs on the health tick rather than on a
// timer per backend, because the tick is already there and a timer per drain is a
// goroutine per failure to stop again on close.
//
// The drain timeout is a ceiling rather than a schedule: the tick is the health
// interval, so a connection is closed on the first tick after its deadline rather
// than exactly on it.
func (b *Balancer) expireDrains() {
	now := time.Now()
	b.mu.Lock()
	var expired []*session
	for _, entry := range b.backends {
		if entry.healthy || entry.drainUntil.IsZero() || now.Before(entry.drainUntil) {
			continue
		}
		for active := range entry.sessions {
			expired = append(expired, active)
		}
		entry.stats.Drained += len(entry.sessions)
		// The deadline is consumed, so a backend that stays down does not close
		// the same connection twice or keep counting it.
		entry.drainUntil = time.Time{}
	}
	b.mu.Unlock()
	for _, active := range expired {
		b.halt(active)
	}
}
