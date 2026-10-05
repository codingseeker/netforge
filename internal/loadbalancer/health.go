package loadbalancer

import (
	"context"
	"sync"
	"time"

	"github.com/netforge/netforge/internal/transport"
)

var healthProbe = []byte("netforge.health.v1")

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

func (b *Balancer) checkAll(ctx context.Context) {
	b.mu.Lock()
	targets := make([]*backend, 0, len(b.backends))
	for _, entry := range b.backends {

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

func (b *Balancer) check(ctx context.Context, entry *backend) (error, time.Duration) {
	attempt, cancel := context.WithTimeout(ctx, b.limits.HealthTimeout)
	defer cancel()
	started := time.Now()
	conn, err := transport.Dial(attempt, "", entry.address, b.limits.Transport)
	if err != nil {
		return err, time.Since(started)
	}

	defer conn.Close()
	if _, err := conn.Write(attempt, healthProbe); err != nil {
		return err, time.Since(started)
	}
	if _, err := conn.Read(attempt); err != nil {
		return err, time.Since(started)
	}
	return nil, time.Since(started)
}

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

		return
	}
	if !entry.healthy && entry.successes >= b.limits.RecoveryThreshold {
		entry.healthy = true
		entry.successes = 0
		entry.stats.Recoveries++

		entry.drainUntil = time.Time{}
		entry.stats.DrainingSince = time.Time{}
	}
}

func (b *Balancer) recordFailure(entry *backend, err error, fromCheck bool) {
	b.record(entry, err, 0, fromCheck)
}

func (b *Balancer) beginDrainLocked(entry *backend) {
	now := time.Now()
	entry.healthy = false
	entry.successes = 0
	entry.failures = 0
	entry.drainUntil = now.Add(b.limits.DrainTimeout)
	entry.stats.DrainingSince = now
}

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

		entry.drainUntil = time.Time{}
	}
	b.mu.Unlock()
	for _, active := range expired {
		b.halt(active)
	}
}
