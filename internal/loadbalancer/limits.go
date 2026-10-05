package loadbalancer

import (
	"fmt"
	"time"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/transport"
)

// The ceilings a configuration may not raise. They are constants rather than
// defaults because a limit that a document can raise is a limit a document can
// remove, and every number here bounds something a peer influences: how many
// backends are remembered, how many connections are held open at once, how long
// a check may take, and how many hash points a ring may hold.
const (
	// MaxBackendCeiling is the most backends one balancer may ever hold, however
	// it is configured.
	MaxBackendCeiling = 256
	// MaxConnectionCeiling is the most client connections one balancer may hold.
	MaxConnectionCeiling = 4096
	// MaxReplicaCeiling is the most virtual nodes a backend may occupy on a
	// consistent hash ring. More points spread keys more evenly and cost more
	// memory, which is a trade a laboratory may want to make and a peer may
	// not.
	MaxReplicaCeiling = 4096
	// MaxHealthInterval bounds how long a check may be spaced, and MaxHealthTimeout
	// bounds how long one may take. Together they are how quickly a balancer
	// notices that a backend is gone and how long a dead one may cost.
	MaxHealthInterval = 10 * time.Minute
	MaxHealthTimeout  = time.Minute
	// MaxDrainTimeout bounds how long a connection to a backend that is
	// draining may stay open. A drain that never ends is a connection that
	// outlives the laboratory that made it.
	MaxDrainTimeout = 10 * time.Minute
	// MaxExchangeTimeout bounds how long one direction of one proxied
	// connection may block.
	MaxExchangeTimeout = 10 * time.Minute
)

// Limits bound one balancer. Every number a configuration could turn into an
// allocation or into an unbounded wait is here, and they are checked before a
// socket is opened, so a rejected configuration has no effect anywhere.
type Limits struct {
	// MaxBackends is how many backends may be registered. A balancer that
	// remembers an unbounded number of them is a balancer whose memory is
	// whatever its configuration says it is.
	MaxBackends int
	// MaxConnections is how many client connections may be proxied at once. It
	// is the balancer's own ceiling and the listener is given the same one, so
	// a peer over the limit is refused by the transport before it is refused
	// here.
	MaxConnections int
	// MaxBackendConnections is how many connections one backend may hold, which
	// is what stops a policy that keeps choosing the same backend from turning
	// the others into decoration.
	MaxBackendConnections int
	// HealthInterval is how often every backend is checked.
	HealthInterval time.Duration
	// HealthTimeout bounds one check. A backend that never answers costs this
	// and not the process's life, which is what makes a health check safe to
	// run against a host that has stopped.
	HealthTimeout time.Duration
	// FailureThreshold is how many consecutive failures, whether from a check
	// or from live traffic, move a backend out of service. One failure is not
	// enough: a single lost datagram is a path, not a backend.
	FailureThreshold int
	// RecoveryThreshold is how many consecutive successful checks put a backend
	// back into service. Recovery is slower than failure on purpose, because a
	// backend that comes back and immediately fails again is worse than one
	// that stays out for a while.
	RecoveryThreshold int
	// DrainTimeout is how long the connections already open to a draining
	// backend may finish. After it they are closed, so a backend that stopped
	// answering cannot hold its share of the balancer's connection budget for
	// ever.
	DrainTimeout time.Duration
	// ExchangeTimeout bounds how long one direction of one proxied connection
	// may block. It is what a silent peer costs rather than the life of the
	// goroutine serving it.
	ExchangeTimeout time.Duration
	// Replicas is how many points a backend occupies on a consistent hash ring.
	// It has no meaning for the other two policies and is ignored by them.
	Replicas int
	// Transport bounds the connection to each backend and the listener that
	// takes the clients. It is the reliable transport's own limits, so a proxy
	// connection is bounded by the same numbers a direct one would be.
	Transport transport.Limits
}

// DefaultLimits returns the limits used when a caller configures none. They are
// sized for a laboratory: a handful of backends, checks often enough that a
// killed service is noticed inside a second, and drains long enough for a
// transfer that is nearly finished to end on its own.
func DefaultLimits() Limits {
	return Limits{
		MaxBackends:           16,
		MaxConnections:        64,
		MaxBackendConnections: 16,
		HealthInterval:        500 * time.Millisecond,
		HealthTimeout:         250 * time.Millisecond,
		FailureThreshold:      2,
		RecoveryThreshold:     2,
		DrainTimeout:          10 * time.Second,
		ExchangeTimeout:       30 * time.Second,
		Replicas:              64,
		Transport:             transport.DefaultLimits(),
	}
}

// Validate fills in the defaults and refuses limits that would make the
// balancer's guarantees meaningless. It is called before a socket is opened, so
// a configuration that is refused here never reached the network.
func (l Limits) Validate() (Limits, error) {
	const op = "loadbalancer.Limits.Validate"
	fallback := DefaultLimits()
	// A limit that is not positive is the default, which is the same rule the
	// transport applies to the limits it is given: a document that asks for none
	// gets the numbers this build uses, and one that asks for a number it cannot
	// have is refused below rather than quietly served at the default's pace.
	if l.MaxBackends <= 0 {
		l.MaxBackends = fallback.MaxBackends
	}
	if l.MaxConnections <= 0 {
		l.MaxConnections = fallback.MaxConnections
	}
	if l.MaxBackendConnections <= 0 {
		l.MaxBackendConnections = fallback.MaxBackendConnections
	}
	if l.HealthInterval <= 0 {
		l.HealthInterval = fallback.HealthInterval
	}
	if l.HealthTimeout <= 0 {
		l.HealthTimeout = fallback.HealthTimeout
	}
	if l.FailureThreshold <= 0 {
		l.FailureThreshold = fallback.FailureThreshold
	}
	if l.RecoveryThreshold <= 0 {
		l.RecoveryThreshold = fallback.RecoveryThreshold
	}
	if l.DrainTimeout <= 0 {
		l.DrainTimeout = fallback.DrainTimeout
	}
	if l.ExchangeTimeout <= 0 {
		l.ExchangeTimeout = fallback.ExchangeTimeout
	}
	if l.Replicas <= 0 {
		l.Replicas = fallback.Replicas
	}

	// A ceiling that a configuration could raise is not a ceiling, so each of
	// these is compared against a constant rather than against another limit.
	if l.MaxBackends > MaxBackendCeiling {
		return l, apperr.LimitExceeded(op, fmt.Sprintf(
			"max_backends is %d, above the %d this balancer may hold",
			l.MaxBackends, MaxBackendCeiling), nil)
	}
	if l.MaxConnections > MaxConnectionCeiling {
		return l, apperr.LimitExceeded(op, fmt.Sprintf(
			"max_connections is %d, above the %d this balancer may hold",
			l.MaxConnections, MaxConnectionCeiling), nil)
	}
	// The per-backend limit is bounded by the whole rather than by a constant of
	// its own, because the two describe one fleet: a backend allowed more
	// connections than the balancer holds altogether is a document describing a
	// server that cannot exist, and it leaves a number in the report that no run
	// could ever reach.
	if l.MaxBackendConnections > l.MaxConnections {
		return l, apperr.InvalidArgument(op, fmt.Sprintf(
			"max_backend_connections is %d, more than the %d this balancer holds in total",
			l.MaxBackendConnections, l.MaxConnections), nil)
	}
	if l.Replicas > MaxReplicaCeiling {
		return l, apperr.LimitExceeded(op, fmt.Sprintf(
			"replicas is %d, above the %d a hash ring may hold per backend",
			l.Replicas, MaxReplicaCeiling), nil)
	}
	if l.HealthInterval > MaxHealthInterval {
		return l, apperr.LimitExceeded(op, fmt.Sprintf(
			"the health interval is %s, above the %s a check may be spaced",
			l.HealthInterval, MaxHealthInterval), nil)
	}
	if l.HealthTimeout > MaxHealthTimeout {
		return l, apperr.LimitExceeded(op, fmt.Sprintf(
			"the health timeout is %s, above the %s one check may take",
			l.HealthTimeout, MaxHealthTimeout), nil)
	}
	if l.DrainTimeout > MaxDrainTimeout {
		return l, apperr.LimitExceeded(op, fmt.Sprintf(
			"the drain timeout is %s, above the %s a draining connection may last",
			l.DrainTimeout, MaxDrainTimeout), nil)
	}
	if l.ExchangeTimeout > MaxExchangeTimeout {
		return l, apperr.LimitExceeded(op, fmt.Sprintf(
			"the exchange timeout is %s, above the %s one direction may block",
			l.ExchangeTimeout, MaxExchangeTimeout), nil)
	}
	// A check that can outlast the interval it is spaced by turns the interval
	// into a suggestion: the loop would run checks back to back and the spacing
	// a document asks for would be the one thing the balancer does not honour.
	if l.HealthTimeout >= l.HealthInterval {
		return l, apperr.InvalidArgument(op, fmt.Sprintf(
			"the health timeout is %s, which is not shorter than the %s interval it is spaced by",
			l.HealthTimeout, l.HealthInterval), nil)
	}
	// A drain shorter than the check that recovers from the failure is not a
	// drain, it is an abrupt close, and a document that asks for one should say
	// so by asking for a short drain rather than a long one.
	if l.DrainTimeout < l.HealthInterval {
		return l, apperr.InvalidArgument(op, fmt.Sprintf(
			"the drain timeout is %s, which is shorter than the %s health interval it is measured in",
			l.DrainTimeout, l.HealthInterval), nil)
	}
	// The listener refuses a peer over the connection limit before the balancer
	// sees it, so a transport ceiling below the balancer's own would make the
	// balancer's number describe something that cannot happen. The balancer's
	// limit is given to the transport rather than the other way round: it is the
	// limit the document sets, and one authority is easier to reason about than
	// two that can disagree.
	l.Transport.MaxConnections = l.MaxConnections
	checked, err := l.Transport.Validate()
	if err != nil {
		// The kind is kept rather than restated, so a limit the transport
		// refused is still reported as a limit and not as a bad argument.
		kind, known := apperr.KindOf(err)
		if !known {
			kind = apperr.KindInvalidArgument
		}
		return l, apperr.Wrap(err, kind, op, "the transport limits are not usable")
	}
	l.Transport = checked
	return l, nil
}
