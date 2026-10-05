package investigator

import (
	"fmt"
	"strings"
	"time"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/probe"
)

type Classification string

const (
	Confirmed Classification = "CONFIRMED"

	Probable Classification = "PROBABLE"

	Possible Classification = "POSSIBLE"

	Unknown Classification = "UNKNOWN"
)

func (c Classification) rank() int {
	switch c {
	case Confirmed:
		return 3
	case Probable:
		return 2
	case Possible:
		return 1
	default:
		return 0
	}
}

func (c Classification) weaker(other Classification) Classification {
	if other.rank() < c.rank() {
		return other
	}
	return c
}

func clamp(report *Report) {
	concluded := false
	weakest := Confirmed
	for _, hypothesis := range report.Hypotheses {
		if !hypothesis.Concludes {
			continue
		}
		concluded = true
		weakest = weakest.weaker(hypothesis.Confidence)
	}
	switch {
	case !concluded:

		report.Diagnosis = Unknown
	case !conclusionSupported(report):
		report.Diagnosis = Unknown
	default:
		report.Diagnosis = report.Diagnosis.weaker(weakest)
	}
}

func conclusionSupported(report *Report) bool {
	for _, hypothesis := range report.Hypotheses {
		if !hypothesis.Concludes {
			continue
		}
		if hypothesis.Supported() {
			return true
		}

		if len(hypothesis.Evidence) == 0 {
			continue
		}
		return true
	}
	return false
}

func (c Classification) String() string { return string(c) }

type Observation struct {
	Experiment string

	Namespace string

	Target string

	Port uint16

	Name string

	Path string

	Failed probe.Protocol

	Failure probe.Failure

	Error string

	At time.Time
}

func (o Observation) Validate(limits Limits) (Observation, error) {
	const op = "investigator.Validate"
	if !o.Failed.Valid() {
		return o, apperr.InvalidArgument(op, fmt.Sprintf(
			"%q is not a protocol an investigator can explain", o.Failed), nil)
	}
	if strings.TrimSpace(o.Namespace) == "" {
		return o, apperr.InvalidArgument(op, "an observation needs the namespace it was seen from", nil)
	}
	if strings.TrimSpace(o.Target) == "" {
		return o, apperr.InvalidArgument(op, "an observation needs a target", nil)
	}
	if strings.TrimSpace(o.Experiment) == "" {
		return o, apperr.InvalidArgument(op, "an observation needs an experiment to belong to", nil)
	}
	if len(o.Experiment) > limits.MaxNameLength {
		return o, apperr.Validation(op, fmt.Sprintf(
			"the experiment name is %d characters, the maximum is %d",
			len(o.Experiment), limits.MaxNameLength), nil)
	}
	if o.At.IsZero() {
		o.At = time.Now()
	}
	return o, nil
}

type Evidence struct {
	Layer string

	What string

	Result probe.Result

	Attempted bool

	Probed bool

	Note string

	At time.Time
}

func (e Evidence) Supports() bool {
	return e.Probed && e.Result.Succeeded()
}

func (e Evidence) Refutes() bool {
	return e.Probed && !e.Result.Succeeded()
}

func (e Evidence) Taken() bool { return e.Probed }

type Hypothesis struct {
	Layer string

	Statement string

	Evidence []int

	RefutedBy []int

	Refutation string

	Confidence Classification

	Concludes bool
}

func (h Hypothesis) Supported() bool { return len(h.Evidence) > 0 }
