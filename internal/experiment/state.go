package experiment

import (
	"strconv"
	"strings"
	"time"

	"github.com/codingseeker/netforge/internal/apperr"
)

type State string

const (
	StateSetup State = "setup"

	StateBaseline State = "baseline"

	StateFault State = "fault"

	StateObserve State = "observe"

	StateDiagnose State = "diagnose"

	StateRecover State = "recover"

	StateVerify State = "verify"

	StateReport State = "report"

	StateCleanup State = "cleanup"
)

func States() []State {
	return []State{
		StateSetup,
		StateBaseline,
		StateFault,
		StateObserve,
		StateDiagnose,
		StateRecover,
		StateVerify,
		StateReport,
		StateCleanup,
	}
}

func (s State) String() string { return string(s) }

func (s State) Valid() bool {
	for _, known := range States() {
		if s == known {
			return true
		}
	}
	return false
}

func (s State) next() (State, bool) {
	states := States()
	for i, known := range states {
		if s == known && i+1 < len(states) {
			return states[i+1], true
		}
	}
	return "", false
}

func (s State) Previous() (State, bool) {
	states := States()
	for i, known := range states {
		if s == known {
			if i == 0 {
				return "", false
			}
			return states[i-1], true
		}
	}
	return "", false
}

func Transition(from, to State) error {
	const op = "experiment.Transition"
	if !from.Valid() {
		return apperr.Validation(op, strconv.Quote(string(from))+" is not a state of an experiment", nil)
	}
	if !to.Valid() {
		return apperr.Validation(op, strconv.Quote(string(to))+" is not a state of an experiment", nil)
	}
	if next, ok := from.next(); ok && next == to {
		return nil
	}
	if from == StateCleanup {
		return apperr.Conflict(op, "the experiment is already cleaned up", nil)
	}
	return apperr.Validation(op, "an experiment cannot move from "+string(from)+" to "+string(to)+
		", the next state after "+string(from)+" is "+string(from.nextName()), nil)
}

func (s State) nextName() string {
	next, ok := s.next()
	if !ok {
		return "none: " + string(s) + " is the last state"
	}
	return string(next)
}

func (s State) CleansUp() bool { return s.Valid() }

func CleanupFrom(failed State) (State, error) {
	const op = "experiment.CleanupFrom"
	if !failed.CleansUp() {
		return "", apperr.Validation(op, strconv.Quote(string(failed))+" is not a state of an experiment", nil)
	}
	return StateCleanup, nil
}

type Step struct {
	State State

	Detail string

	Recovery string

	Deadline time.Duration
}

func (s Step) Describe() string {
	if s.Recovery != "" {
		return "perform " + s.Recovery
	}
	return s.Detail
}

func Render(steps []Step) string {
	var out strings.Builder
	for i, step := range steps {
		if i > 0 {
			out.WriteString("\n")
		}
		out.WriteString(step.State.String())
		if step.Detail != "" {
			out.WriteString(": " + step.Detail)
		}
	}
	return out.String()
}
