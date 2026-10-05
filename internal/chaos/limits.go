package chaos

import (
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/namespace"
)

// The compiled-in ceilings. A configuration may lower every limit but may
// never raise one past these values, so a limit cannot be used to widen the
// blast radius of a fault.
const (
	CeilingFaults    = 64
	CeilingDuration  = 24 * time.Hour
	CeilingLatency   = 5 * time.Minute
	CeilingBandwidth = 100_000_000_000
	CeilingQueue     = 65_536
	CeilingGap       = 65_535
)

// Limits bound what the engine is allowed to do.
type Limits struct {
	// MaxFaults is the number of faults that may be active at once.
	MaxFaults int `json:"max_faults"`
	// MaxDuration is the longest lifetime a single fault may request.
	MaxDuration time.Duration `json:"max_duration"`
	// MaxLatency is the largest delay a fault may add.
	MaxLatency time.Duration `json:"max_latency"`
	// MaxBandwidth is the largest rate ceiling a fault may request, in bits
	// per second.
	MaxBandwidth uint64 `json:"max_bandwidth"`
	// MaxQueue is the largest netem queue depth a fault may request.
	MaxQueue int `json:"max_queue"`
	// MaxGap is the largest reorder gap a fault may request.
	MaxGap int `json:"max_gap"`
	// DefaultQueue is the netem queue depth used when a fault does not set one.
	DefaultQueue int `json:"default_queue"`
}

// DefaultLimits returns the limits used when a laboratory does not configure
// any. They are deliberately small: a laboratory is a test fixture, not a
// traffic generator.
func DefaultLimits() Limits {
	return Limits{
		MaxFaults:    16,
		MaxDuration:  10 * time.Minute,
		MaxLatency:   30 * time.Second,
		MaxBandwidth: 1_000_000_000,
		MaxQueue:     4096,
		MaxGap:       1000,
		DefaultQueue: 1000,
	}
}

// WithDefaults fills in every unset limit.
func (l Limits) WithDefaults() Limits {
	defaults := DefaultLimits()
	if l.MaxFaults == 0 {
		l.MaxFaults = defaults.MaxFaults
	}
	if l.MaxDuration == 0 {
		l.MaxDuration = defaults.MaxDuration
	}
	if l.MaxLatency == 0 {
		l.MaxLatency = defaults.MaxLatency
	}
	if l.MaxBandwidth == 0 {
		l.MaxBandwidth = defaults.MaxBandwidth
	}
	if l.MaxQueue == 0 {
		l.MaxQueue = defaults.MaxQueue
	}
	if l.MaxGap == 0 {
		l.MaxGap = defaults.MaxGap
	}
	if l.DefaultQueue == 0 {
		l.DefaultQueue = defaults.DefaultQueue
	}
	return l
}

// check rejects limits that are missing, negative, or above a ceiling, and
// repairs the values that must be consistent with each other.
func (l Limits) check() (Limits, error) {
	const op = "chaos.Limits.check"
	l = l.WithDefaults()

	positive := func(name string, value int, ceiling int) error {
		if value <= 0 {
			return apperr.Validation(op, fmt.Sprintf("the %s limit must be positive, got %d", name, value), nil)
		}
		if value > ceiling {
			return apperr.LimitExceeded(op, fmt.Sprintf(
				"the %s limit of %d exceeds the maximum of %d", name, value, ceiling), nil)
		}
		return nil
	}
	if err := positive("max_faults", l.MaxFaults, CeilingFaults); err != nil {
		return Limits{}, err
	}
	if err := positive("max_queue", l.MaxQueue, CeilingQueue); err != nil {
		return Limits{}, err
	}
	if err := positive("max_gap", l.MaxGap, CeilingGap); err != nil {
		return Limits{}, err
	}
	if err := positive("default_queue", l.DefaultQueue, CeilingQueue); err != nil {
		return Limits{}, err
	}
	if l.DefaultQueue > l.MaxQueue {
		return Limits{}, apperr.Validation(op, fmt.Sprintf(
			"the default queue depth of %d exceeds the maximum of %d", l.DefaultQueue, l.MaxQueue), nil)
	}
	if l.MaxDuration <= 0 {
		return Limits{}, apperr.Validation(op, "the max_duration limit must be positive", nil)
	}
	if l.MaxDuration > CeilingDuration {
		return Limits{}, apperr.LimitExceeded(op, fmt.Sprintf(
			"the max_duration limit of %s exceeds the maximum of %s", l.MaxDuration, CeilingDuration), nil)
	}
	if l.MaxLatency <= 0 {
		return Limits{}, apperr.Validation(op, "the max_latency limit must be positive", nil)
	}
	if l.MaxLatency > CeilingLatency {
		return Limits{}, apperr.LimitExceeded(op, fmt.Sprintf(
			"the max_latency limit of %s exceeds the maximum of %s", l.MaxLatency, CeilingLatency), nil)
	}
	if l.MaxBandwidth == 0 {
		return Limits{}, apperr.Validation(op, "the max_bandwidth limit must be positive", nil)
	}
	if l.MaxBandwidth > CeilingBandwidth {
		return Limits{}, apperr.LimitExceeded(op, fmt.Sprintf(
			"the max_bandwidth limit of %s exceeds the maximum of %s",
			Rate(l.MaxBandwidth), Rate(CeilingBandwidth)), nil)
	}
	return l, nil
}

// Scope is the set of namespaces and the prefix an engine may touch. A target
// outside the scope is refused before any command runs, which is what keeps
// fault injection away from the host network.
type Scope struct {
	// Network is the laboratory prefix the engine serves.
	Network netip.Prefix `json:"network"`
	// Namespaces are the laboratory namespaces an engine may touch.
	Namespaces []string `json:"namespaces"`
}

// check validates the scope itself.
func (s Scope) check() error {
	const op = "chaos.Scope.check"
	if !s.Network.IsValid() || !s.Network.Addr().Is4() {
		return apperr.Validation(op, "the chaos scope needs a valid IPv4 laboratory network", nil)
	}
	if len(s.Namespaces) == 0 {
		return apperr.Validation(op, "the chaos scope must name at least one namespace", nil)
	}
	seen := make(map[string]bool, len(s.Namespaces))
	for _, name := range s.Namespaces {
		if err := checkNamespaceName(op, name); err != nil {
			return err
		}
		if seen[name] {
			return apperr.Validation(op, fmt.Sprintf("the namespace %q appears twice in the scope", name), nil)
		}
		seen[name] = true
	}
	return nil
}

// allows reports whether a namespace is inside the scope.
func (s Scope) allows(namespace string) bool {
	for _, name := range s.Namespaces {
		if name == namespace {
			return true
		}
	}
	return false
}

// laboratoryPrefix is the prefix every laboratory namespace carries. Requiring
// it is what makes a host namespace impossible to put in scope: "host" and the
// namespaces of the host's own workloads do not carry it, so a scope can only
// ever name namespaces the laboratory created for itself.
const laboratoryPrefix = "nf-"

// checkNamespaceName validates a laboratory namespace name. The namespace
// layer owns the naming rules, so its validator is used rather than a second,
// looser copy, and the laboratory prefix is required on top of them.
func checkNamespaceName(op, name string) error {
	if err := namespace.ValidateName(op, "namespace", name); err != nil {
		return apperr.Validation(op, "a scope entry must be a laboratory namespace name", err)
	}
	if !strings.HasPrefix(name, laboratoryPrefix) || len(name) == len(laboratoryPrefix) {
		return apperr.Permission(op, fmt.Sprintf(
			"the namespace %q is not a laboratory namespace, chaos never runs outside a laboratory, "+
				"so the name must start with %q", name, laboratoryPrefix), nil)
	}
	return nil
}
