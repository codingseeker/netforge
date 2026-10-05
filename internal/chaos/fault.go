// Package chaos injects controlled network faults into an isolated laboratory.
//
// The package is deliberately free of process execution: it builds validated
// argument vectors and interprets kernel state reported back by the namespace
// layer. Every fault is scoped to a laboratory namespace, bounded by limits,
// verified against the kernel after it is applied, and removed again when it
// expires, when the engine is closed, or when the laboratory is destroyed.
package chaos

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/iface"
	"github.com/netforge/netforge/internal/namespace"
)

// Kind identifies a supported fault. The set is closed: an unknown kind is a
// validation error rather than a silently ignored field.
type Kind string

// The supported fault kinds.
const (
	KindLatency     Kind = "latency"
	KindJitter      Kind = "jitter"
	KindPacketLoss  Kind = "packet_loss"
	KindBandwidth   Kind = "bandwidth"
	KindDuplication Kind = "duplication"
	KindReordering  Kind = "reordering"
)

var kinds = [...]Kind{
	KindLatency,
	KindJitter,
	KindPacketLoss,
	KindBandwidth,
	KindDuplication,
	KindReordering,
}

// Kinds returns every supported fault kind.
func Kinds() []Kind {
	out := make([]Kind, len(kinds))
	copy(out, kinds[:])
	return out
}

// Valid reports whether the kind is one of the supported faults.
func (k Kind) Valid() bool {
	for _, known := range kinds {
		if k == known {
			return true
		}
	}
	return false
}

func (k Kind) String() string { return string(k) }

// Direction selects the side of the link that carries the impairment queue.
type Direction string

// The supported directions.
const (
	// DirectionOut shapes traffic leaving the target.
	DirectionOut Direction = "out"
	// DirectionIn shapes traffic arriving at the target, which leaves the peer.
	DirectionIn Direction = "in"
)

var directions = [...]Direction{DirectionOut, DirectionIn}

// Valid reports whether the direction is supported.
func (d Direction) Valid() bool {
	for _, known := range directions {
		if d == known {
			return true
		}
	}
	return false
}

func (d Direction) String() string { return string(d) }

// Status is the lifecycle state of a fault.
type Status string

// The fault lifecycle states.
const (
	// StatusPending means the specification was accepted but not yet applied.
	StatusPending Status = "pending"
	// StatusActive means the kernel reports the fault on the device.
	StatusActive Status = "active"
	// StatusExpired means the duration elapsed and the fault was removed.
	StatusExpired Status = "expired"
	// StatusRemoved means the fault was removed before it expired.
	StatusRemoved Status = "removed"
	// StatusFailed means the fault could not be applied or removed.
	StatusFailed Status = "failed"
)

// Finished reports whether the fault is no longer present in the kernel.
func (s Status) Finished() bool {
	return s == StatusExpired || s == StatusRemoved || s == StatusFailed
}

func (s Status) String() string { return string(s) }

// Percent is a probability on a scale of 0 to 100.
type Percent float64

// Fraction returns the probability on a scale of 0 to 1.
func (p Percent) Fraction() float64 { return float64(p) / 100 }

// Valid reports whether the percentage can be handed to the kernel. Zero is
// rejected on purpose: a fault that provably does nothing is reported as a
// fault would be a lie, and the kernel silently accepts a 0% loss qdisc.
func (p Percent) Valid() bool {
	value := float64(p)
	return !math.IsNaN(value) && !math.IsInf(value, 0) && p > 0 && p <= 100
}

func (p Percent) String() string {
	return strconv.FormatFloat(float64(p), 'f', -1, 64) + "%"
}

// Duration is a time span that is reported as a duration string rather than as
// a bare nanosecond count, so a fault record stays readable.
type Duration time.Duration

// Std returns the duration as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// MarshalJSON encodes the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON accepts a duration string or a nanosecond count.
func (d *Duration) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		parsed, err := time.ParseDuration(text)
		if err != nil {
			return fmt.Errorf("duration %q: %w", text, err)
		}
		*d = Duration(parsed)
		return nil
	}
	var count int64
	if err := json.Unmarshal(data, &count); err != nil {
		return fmt.Errorf("duration must be a string or a nanosecond count: %w", err)
	}
	*d = Duration(count)
	return nil
}

// Parameters are the configured values of a fault. They are what the caller
// asked for; they are never presented as an observation of the network.
type Parameters struct {
	// Latency is the deterministic delay added to a packet.
	Latency Duration `json:"latency,omitempty"`
	// Jitter is the random variation around Latency.
	Jitter Duration `json:"jitter,omitempty"`
	// Correlation couples consecutive packets, on a scale of 0 to 100.
	Correlation Percent `json:"correlation,omitempty"`
	// Loss is the percentage of packets dropped.
	Loss Percent `json:"loss,omitempty"`
	// Duplication is the percentage of packets duplicated.
	Duplication Percent `json:"duplication,omitempty"`
	// Reordering is the percentage of packets sent out of order.
	Reordering Percent `json:"reordering,omitempty"`
	// Gap is the distance in packets between reordered packets.
	Gap int `json:"gap,omitempty"`
	// Rate is the bandwidth ceiling in bits per second.
	Rate uint64 `json:"rate,omitempty"`
	// Queue is the netem queue depth in packets.
	Queue int `json:"queue,omitempty"`
}

// empty reports whether no parameter is set at all.
func (p Parameters) empty() bool {
	return p == Parameters{}
}

// Endpoint is one end of an impaired link.
type Endpoint struct {
	Namespace string `json:"namespace"`
	Interface string `json:"interface"`
}

func (e Endpoint) String() string { return e.Namespace + "/" + e.Interface }

// valid reports whether both halves of the endpoint are well formed.
func (e Endpoint) valid() bool {
	return namespace.ValidateName("chaos", "namespace", e.Namespace) == nil &&
		iface.ValidateName("chaos", "interface", e.Interface) == nil
}

// Spec is a validated request for a fault. It carries no state: the engine
// turns a specification into a Fault record once the kernel agrees.
type Spec struct {
	// ID names the fault. When empty the engine derives a stable identifier.
	ID string `json:"id,omitempty"`
	// Target is the laboratory namespace that feels the fault.
	Target string `json:"target"`
	// Interface is the target data interface. When empty the engine resolves
	// the single data interface of the namespace.
	Interface string `json:"interface,omitempty"`
	// Direction selects which side of the link is impaired.
	Direction Direction `json:"direction,omitempty"`
	// Kind is the fault to inject.
	Kind Kind `json:"type"`
	// Parameters are the configured values of the fault.
	Parameters Parameters `json:"parameters"`
	// Duration bounds how long the fault may live. When zero the engine uses
	// the maximum duration of the limits.
	Duration Duration `json:"duration,omitempty"`
}

// Observed is the state the kernel reported back after a fault was applied. It
// is evidence, kept separate from the configured Parameters on purpose.
type Observed struct {
	Kind        string   `json:"kind"`
	Handle      string   `json:"handle"`
	Queue       int      `json:"queue,omitempty"`
	Latency     Duration `json:"latency,omitempty"`
	Jitter      Duration `json:"jitter,omitempty"`
	Correlation Percent  `json:"correlation,omitempty"`
	Loss        Percent  `json:"loss,omitempty"`
	Duplication Percent  `json:"duplication,omitempty"`
	Reordering  Percent  `json:"reordering,omitempty"`
	Gap         int      `json:"gap,omitempty"`
	Rate        uint64   `json:"rate,omitempty"`
	Burst       uint64   `json:"burst,omitempty"`
}

// Fault is the record of an injected fault. The field names are part of the
// contract: fault_id, experiment_id, source, destination, start_time,
// duration, parameters and status are always present.
type Fault struct {
	ID          string     `json:"fault_id"`
	Experiment  string     `json:"experiment_id"`
	Kind        Kind       `json:"type"`
	Status      Status     `json:"status"`
	Start       time.Time  `json:"start_time"`
	Expires     time.Time  `json:"expires_at"`
	Duration    Duration   `json:"duration"`
	Source      Endpoint   `json:"source"`
	Destination Endpoint   `json:"destination"`
	Target      Endpoint   `json:"target"`
	Direction   Direction  `json:"direction"`
	Parameters  Parameters `json:"parameters"`
	Commands    [][]string `json:"applied_commands"`
	Observed    Observed   `json:"observed"`
	Verified    bool       `json:"verified"`
	Reason      string     `json:"reason,omitempty"`
}

// Active reports whether the fault is currently expected in the kernel.
func (f Fault) Active() bool { return f.Status == StatusActive }

// Validate checks a specification against limits and returns it with its
// defaults filled in. It never touches the kernel and never touches the
// laboratory, so a rejected specification has no effect anywhere.
//
// This is the same check the engine applies before it installs anything. Code
// that turns a request into a specification, such as the configuration
// reader, validates through here rather than repeating the rules, so a
// specification that passes can never be refused at the moment it is applied.
func (s Spec) Validate(l Limits) (Spec, error) {
	checked, err := l.check()
	if err != nil {
		return Spec{}, err
	}
	return s.check(checked)
}

// check validates a specification and fills in the defaults. It never touches
// the kernel and never touches the laboratory, so a rejected specification has
// no effect anywhere.
func (s Spec) check(l Limits) (Spec, error) {
	const op = "chaos.Spec.check"

	if err := namespace.ValidateName(op, "target", s.Target); err != nil {
		return Spec{}, apperr.Validation(op, "the fault target must be a laboratory namespace", err)
	}
	if s.Interface != "" {
		if err := iface.ValidateName(op, "interface", s.Interface); err != nil {
			return Spec{}, apperr.Validation(op, "the fault interface is not a valid device name", err)
		}
	}
	if s.ID != "" {
		if err := validateFaultID(op, s.ID); err != nil {
			return Spec{}, err
		}
	}
	if !s.Kind.Valid() {
		return Spec{}, apperr.Validation(op, fmt.Sprintf("fault type %q is not supported, choose one of %s",
			s.Kind, kindList()), nil)
	}
	if s.Direction == "" {
		s.Direction = DirectionOut
	}
	if !s.Direction.Valid() {
		return Spec{}, apperr.Validation(op, fmt.Sprintf("direction %q is not supported, choose %q or %q",
			s.Direction, DirectionOut, DirectionIn), nil)
	}

	switch {
	case s.Duration == 0:
		s.Duration = Duration(l.MaxDuration)
	case s.Duration < 0:
		return Spec{}, apperr.Validation(op, "the fault duration must be positive", nil)
	case s.Duration.Std() > l.MaxDuration:
		return Spec{}, apperr.LimitExceeded(op, fmt.Sprintf(
			"the fault duration %s exceeds the maximum of %s", s.Duration, Duration(l.MaxDuration)), nil)
	}

	if err := s.checkParameters(l); err != nil {
		return Spec{}, err
	}
	return s, nil
}

// checkParameters enforces the parameters that belong to the chosen kind and
// rejects the ones that do not, so a mislabelled fault cannot reach the kernel.
func (s *Spec) checkParameters(l Limits) error {
	const op = "chaos.Spec.check"
	p := &s.Parameters

	reject := func(name string) error {
		return apperr.Validation(op, fmt.Sprintf(
			"a %q fault cannot carry the %s parameter", s.Kind, name), nil)
	}
	checkLatency := func() error {
		if p.Latency <= 0 {
			return apperr.Validation(op, "a latency must be greater than zero", nil)
		}
		if p.Latency.Std() > l.MaxLatency {
			return apperr.LimitExceeded(op, fmt.Sprintf(
				"the latency %s exceeds the maximum of %s", p.Latency, Duration(l.MaxLatency)), nil)
		}
		return nil
	}
	checkQueue := func() error {
		if p.Queue == 0 {
			p.Queue = l.DefaultQueue
			return nil
		}
		if p.Queue < 0 || p.Queue > l.MaxQueue {
			return apperr.LimitExceeded(op, fmt.Sprintf(
				"the queue depth %d is outside 1 to %d packets", p.Queue, l.MaxQueue), nil)
		}
		return nil
	}

	rest := func() []string {
		var names []string
		if p.Latency != 0 {
			names = append(names, "latency")
		}
		if p.Jitter != 0 {
			names = append(names, "jitter")
		}
		if p.Correlation != 0 {
			names = append(names, "correlation")
		}
		if p.Loss != 0 {
			names = append(names, "loss")
		}
		if p.Duplication != 0 {
			names = append(names, "duplication")
		}
		if p.Reordering != 0 {
			names = append(names, "reordering")
		}
		if p.Gap != 0 {
			names = append(names, "gap")
		}
		if p.Rate != 0 {
			names = append(names, "rate")
		}
		if p.Queue != 0 {
			names = append(names, "queue")
		}
		return names
	}
	unexpected := func(allowed ...string) error {
		for _, name := range rest() {
			if !contains(allowed, name) {
				return reject(name)
			}
		}
		return nil
	}

	switch s.Kind {
	case KindLatency:
		if err := checkLatency(); err != nil {
			return err
		}
		if err := checkQueue(); err != nil {
			return err
		}
		return unexpected("latency", "queue")

	case KindJitter:
		if err := checkLatency(); err != nil {
			return err
		}
		if p.Jitter <= 0 {
			return apperr.Validation(op, "a jitter fault needs a jitter greater than zero", nil)
		}
		if p.Jitter > p.Latency {
			return apperr.Validation(op, fmt.Sprintf(
				"the jitter %s must not exceed the delay %s it varies around", p.Jitter, p.Latency), nil)
		}
		if p.Correlation != 0 && !p.Correlation.Valid() {
			return apperr.Validation(op, fmt.Sprintf("the correlation %s is outside 0 to 100", p.Correlation), nil)
		}
		if err := checkQueue(); err != nil {
			return err
		}
		return unexpected("latency", "jitter", "correlation", "queue")

	case KindPacketLoss:
		if !p.Loss.Valid() {
			return apperr.Validation(op, fmt.Sprintf(
				"the loss %s is outside 0 to 100, and a fault that drops nothing is not a fault", p.Loss), nil)
		}
		if p.Correlation != 0 && !p.Correlation.Valid() {
			return apperr.Validation(op, fmt.Sprintf("the correlation %s is outside 0 to 100", p.Correlation), nil)
		}
		if err := checkQueue(); err != nil {
			return err
		}
		return unexpected("loss", "correlation", "queue")

	case KindBandwidth:
		if p.Rate == 0 {
			return apperr.Validation(op, "a bandwidth fault needs a rate greater than zero", nil)
		}
		if p.Rate > l.MaxBandwidth {
			return apperr.LimitExceeded(op, fmt.Sprintf(
				"the rate %s exceeds the maximum of %s", Rate(p.Rate), Rate(l.MaxBandwidth)), nil)
		}
		return unexpected("rate")

	case KindDuplication:
		if !p.Duplication.Valid() {
			return apperr.Validation(op, fmt.Sprintf(
				"the duplication %s is outside 0 to 100, and a fault that duplicates nothing is not a fault",
				p.Duplication), nil)
		}
		if err := checkQueue(); err != nil {
			return err
		}
		return unexpected("duplication", "queue")

	case KindReordering:
		if !p.Reordering.Valid() {
			return apperr.Validation(op, fmt.Sprintf(
				"the reordering %s is outside 0 to 100, and a fault that reorders nothing is not a fault",
				p.Reordering), nil)
		}
		if err := checkLatency(); err != nil {
			return apperr.Validation(op, "reordering needs a delay to reorder packets against", err)
		}
		if p.Gap == 0 {
			p.Gap = 1
		}
		if p.Gap < 1 || p.Gap > l.MaxGap {
			return apperr.LimitExceeded(op, fmt.Sprintf(
				"the reorder gap %d is outside 1 to %d packets", p.Gap, l.MaxGap), nil)
		}
		if err := checkQueue(); err != nil {
			return err
		}
		return unexpected("latency", "reordering", "correlation", "gap", "queue")
	}
	return apperr.Internal(op, fmt.Sprintf("fault kind %q has no parameter rules", s.Kind), nil)
}

// Rate is a bandwidth in bits per second that prints with a unit suffix.
type Rate uint64

func (r Rate) String() string { return strconv.FormatUint(uint64(r), 10) + "bit" }

func validateFaultID(op, id string) error {
	if len(id) > maxFaultIDLength {
		return apperr.Validation(op, fmt.Sprintf(
			"the fault id is %d characters, the maximum is %d", len(id), maxFaultIDLength), nil)
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == ':', c == '@', c == '/':
		default:
			return apperr.Validation(op, fmt.Sprintf(
				"the fault id contains %q, which is not allowed", string(c)), nil)
		}
	}
	return nil
}

const maxFaultIDLength = 63

func kindList() string {
	names := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		names = append(names, string(kind))
	}
	return strings.Join(names, ", ")
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
