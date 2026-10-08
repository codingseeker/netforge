package chaos

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/codingseeker/netforge/internal/apperr"
	"github.com/codingseeker/netforge/internal/iface"
	"github.com/codingseeker/netforge/internal/namespace"
)

type Kind string

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

func Kinds() []Kind {
	out := make([]Kind, len(kinds))
	copy(out, kinds[:])
	return out
}

func (k Kind) Valid() bool {
	for _, known := range kinds {
		if k == known {
			return true
		}
	}
	return false
}

func (k Kind) String() string { return string(k) }

type Direction string

const (
	DirectionOut Direction = "out"

	DirectionIn Direction = "in"
)

var directions = [...]Direction{DirectionOut, DirectionIn}

func (d Direction) Valid() bool {
	for _, known := range directions {
		if d == known {
			return true
		}
	}
	return false
}

func (d Direction) String() string { return string(d) }

type Status string

const (
	StatusPending Status = "pending"

	StatusActive Status = "active"

	StatusExpired Status = "expired"

	StatusRemoved Status = "removed"

	StatusFailed Status = "failed"
)

func (s Status) Finished() bool {
	return s == StatusExpired || s == StatusRemoved || s == StatusFailed
}

func (s Status) String() string { return string(s) }

type Percent float64

func (p Percent) Fraction() float64 { return float64(p) / 100 }

func (p Percent) Valid() bool {
	value := float64(p)
	return !math.IsNaN(value) && !math.IsInf(value, 0) && p > 0 && p <= 100
}

func (p Percent) String() string {
	return strconv.FormatFloat(float64(p), 'f', -1, 64) + "%"
}

type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

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

type Parameters struct {
	Latency Duration `json:"latency,omitempty"`

	Jitter Duration `json:"jitter,omitempty"`

	Correlation Percent `json:"correlation,omitempty"`

	Loss Percent `json:"loss,omitempty"`

	Duplication Percent `json:"duplication,omitempty"`

	Reordering Percent `json:"reordering,omitempty"`

	Gap int `json:"gap,omitempty"`

	Rate uint64 `json:"rate,omitempty"`

	Queue int `json:"queue,omitempty"`
}

func (p Parameters) empty() bool {
	return p == Parameters{}
}

type Endpoint struct {
	Namespace string `json:"namespace"`
	Interface string `json:"interface"`
}

func (e Endpoint) String() string { return e.Namespace + "/" + e.Interface }

func (e Endpoint) valid() bool {
	return namespace.ValidateName("chaos", "namespace", e.Namespace) == nil &&
		iface.ValidateName("chaos", "interface", e.Interface) == nil
}

type Spec struct {
	ID string `json:"id,omitempty"`

	Target string `json:"target"`

	Interface string `json:"interface,omitempty"`

	Direction Direction `json:"direction,omitempty"`

	Kind Kind `json:"type"`

	Parameters Parameters `json:"parameters"`

	Duration Duration `json:"duration,omitempty"`
}

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

func (f Fault) Active() bool { return f.Status == StatusActive }

func (s Spec) Validate(l Limits) (Spec, error) {
	checked, err := l.check()
	if err != nil {
		return Spec{}, err
	}
	return s.check(checked)
}

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
