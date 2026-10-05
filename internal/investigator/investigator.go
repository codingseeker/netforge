// Package investigator turns a failed measurement into a diagnosis.
//
// A probe reports what happened on the wire. That is evidence, not an
// explanation: "the HTTP request timed out" does not say whether the path
// broke, the connection was refused, the certificate was wrong or the server
// answered with an error. This package walks the stack below the failure,
// collects what each layer does, generates the explanations that evidence
// supports, and reports how confident it is allowed to be.
//
// The confidence levels are the point. A diagnosis that says CONFIRMED when
// the evidence only narrows the field is worse than no diagnosis, because it
// stops the next question from being asked. Every classification here is
// derived from named evidence, and a hypothesis whose supporting evidence is
// missing is never reported above UNKNOWN.
package investigator

import (
	"fmt"
	"strings"
	"time"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/probe"
)

// Classification is how much the evidence actually supports a diagnosis.
type Classification string

const (
	// Confirmed means the failing layer was measured failing and every layer
	// below it was measured working, with the failure reproduced. Nothing
	// else fits the evidence.
	Confirmed Classification = "CONFIRMED"
	// Probable means one explanation fits the evidence and the alternatives
	// are unlikely but not excluded.
	Probable Classification = "PROBABLE"
	// Possible means the evidence narrows the field without choosing between
	// the explanations that remain.
	Possible Classification = "POSSIBLE"
	// Unknown means the evidence does not separate the explanations at all.
	Unknown Classification = "UNKNOWN"
)

// rank orders the levels so a report never claims more confidence than its
// weakest supporting hypothesis.
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

// weaker returns the lower of two levels.
func (c Classification) weaker(other Classification) Classification {
	if other.rank() < c.rank() {
		return other
	}
	return c
}

// clamp checks a report's classification against the hypothesis it is
// concluded from. This is where "never represent an unsupported hypothesis as
// confirmed" is enforced rather than merely intended: a rule that reached for a
// level its own conclusion does not carry, or that cites no evidence at all, has
// it taken away here. The weaker alternatives in the report are left alone, since
// reporting them is the point of carrying them.
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
		// A report that reaches a conclusion without naming one has nothing to
		// be confident about.
		report.Diagnosis = Unknown
	case !conclusionSupported(report):
		report.Diagnosis = Unknown
	default:
		report.Diagnosis = report.Diagnosis.weaker(weakest)
	}
}

// conclusionSupported reports whether the hypothesis a report is concluded from
// has real measurements behind it.
func conclusionSupported(report *Report) bool {
	for _, hypothesis := range report.Hypotheses {
		if !hypothesis.Concludes {
			continue
		}
		if hypothesis.Supported() {
			return true
		}
		// A hypothesis with no probe of its own can still stand, but only on the
		// kernel's own answer rather than on a measurement that was not taken.
		if len(hypothesis.Evidence) == 0 {
			continue
		}
		return true
	}
	return false
}

// String renders the classification for a report to be read by a person.
func (c Classification) String() string { return string(c) }

// Observation is what a caller noticed going wrong. It says which target
// failed and how, and nothing more: the investigator works out the rest.
type Observation struct {
	// Experiment is the experiment the failure belongs to.
	Experiment string
	// Namespace is the laboratory namespace the failure was seen from.
	Namespace string
	// Target is the address that failed.
	Target string
	// Port is the port that failed, for the layers that use one.
	Port uint16
	// Name is the name that was being resolved or verified, when the
	// observation is about one.
	Name string
	// Path is the resource that was requested, for an HTTP observation.
	Path string
	// Failed is the protocol that failed.
	Failed probe.Protocol
	// Failure is what that protocol reported, which is the first piece of
	// evidence there is.
	Failure probe.Failure
	// Error is the message that came with it.
	Error string
	// At is when the failure was seen.
	At time.Time
}

// Validate checks an observation before any probe is run on its behalf.
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

// Evidence is one measurement the investigator took, and what it showed. A
// report cites evidence by index, so every statement in a report can be traced
// to a measurement that produced it.
type Evidence struct {
	// Layer is the layer the evidence speaks about.
	Layer string
	// What describes the measurement in words, such as "echo" or "route".
	What string
	// Result is the probe result, when the evidence came from a probe. A
	// routing measurement has no probe result and carries its answer in Note.
	Result probe.Result
	// Attempted reports whether a probe was asked for this evidence.
	Attempted bool
	// Probed reports whether a measurement came back. A probe that was asked
	// for and refused has Attempted set and Probed clear, because a measurement
	// that was not taken is not a measurement that found nothing: reading one
	// as the other is how a laboratory problem turns into a blamed layer.
	Probed bool
	// Note is what the evidence showed, in words. It is written from the
	// measurement rather than from an assumption about it.
	Note string
	// At is when the evidence was collected.
	At time.Time
}

// Supports reports whether this evidence shows the layer working.
func (e Evidence) Supports() bool {
	return e.Probed && e.Result.Succeeded()
}

// Refutes reports whether this evidence shows the layer failing. Only a
// measurement that came back can refute anything.
func (e Evidence) Refutes() bool {
	return e.Probed && !e.Result.Succeeded()
}

// Taken reports whether a measurement came back for this evidence.
func (e Evidence) Taken() bool { return e.Probed }

// Hypothesis is a candidate explanation and the evidence behind it.
type Hypothesis struct {
	// Layer is the layer the explanation blames.
	Layer string
	// Statement is the explanation in one sentence.
	Statement string
	// Evidence lists the indexes in the report that support it.
	Evidence []int
	// RefutedBy lists the indexes that argue against it, with the reason in
	// Refutation.
	RefutedBy []int
	// Refutation is why the hypothesis does not hold.
	Refutation string
	// Confidence is how far the evidence carries this hypothesis on its own.
	Confidence Classification
	// Concludes marks the one hypothesis the report is concluded from. A report
	// carries weaker hypotheses too, on purpose: they are the alternatives that
	// were considered and did not fit, and a reader can see why each was
	// dropped. Only the concluding one may raise the report's confidence.
	Concludes bool
}

// Supported reports whether anything supports the hypothesis.
func (h Hypothesis) Supported() bool { return len(h.Evidence) > 0 }
