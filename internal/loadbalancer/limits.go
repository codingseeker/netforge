package loadbalancer

import (
	"fmt"
	"time"

	"github.com/codingseeker/netforge/internal/apperr"
	"github.com/codingseeker/netforge/internal/transport"
)

const (
	MaxBackendCeiling = 256

	MaxConnectionCeiling = 4096

	MaxReplicaCeiling = 4096

	MaxHealthInterval = 10 * time.Minute
	MaxHealthTimeout  = time.Minute

	MaxDrainTimeout = 10 * time.Minute

	MaxExchangeTimeout = 10 * time.Minute
)

type Limits struct {
	MaxBackends int

	MaxConnections int

	MaxBackendConnections int

	HealthInterval time.Duration

	HealthTimeout time.Duration

	FailureThreshold int

	RecoveryThreshold int

	DrainTimeout time.Duration

	ExchangeTimeout time.Duration

	Replicas int

	Transport transport.Limits
}

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

func (l Limits) Validate() (Limits, error) {
	const op = "loadbalancer.Limits.Validate"
	fallback := DefaultLimits()

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

	if l.HealthTimeout >= l.HealthInterval {
		return l, apperr.InvalidArgument(op, fmt.Sprintf(
			"the health timeout is %s, which is not shorter than the %s interval it is spaced by",
			l.HealthTimeout, l.HealthInterval), nil)
	}

	if l.DrainTimeout < l.HealthInterval {
		return l, apperr.InvalidArgument(op, fmt.Sprintf(
			"the drain timeout is %s, which is shorter than the %s health interval it is measured in",
			l.DrainTimeout, l.HealthInterval), nil)
	}

	l.Transport.MaxConnections = l.MaxConnections
	checked, err := l.Transport.Validate()
	if err != nil {

		kind, known := apperr.KindOf(err)
		if !known {
			kind = apperr.KindInvalidArgument
		}
		return l, apperr.Wrap(err, kind, op, "the transport limits are not usable")
	}
	l.Transport = checked
	return l, nil
}
