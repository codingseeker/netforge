// Package experiment runs a declarative experiment through a fixed lifecycle.
//
// An experiment is data first. A document says which network and namespaces it
// is allowed to touch, which faults it injects and how long it may run, and the
// package turns that document into a plan: a complete, ordered list of the
// steps that will be performed, every one of them checked before the first
// namespace is created. Nothing is created, injected or measured until the
// whole plan is known to be runnable, because a laboratory that is half built
// and then refused is a laboratory somebody has to clean up by hand.
//
// The lifecycle is explicit rather than a sequence of calls, so the state an
// experiment is in is a fact a report can state:
//
//	SETUP → BASELINE → FAULT → OBSERVE → DIAGNOSE → RECOVER → VERIFY → REPORT → CLEANUP
//
// Transitions run in one direction only, an invalid one is refused, and every
// state can be cleaned up from. The RECOVER step is where the phase-10 rule
// lives: the state is always visited, and it does only what an operator
// authorized in advance. A diagnosis that suggests a repair does not cause one.
package experiment

import (
	"strconv"
	"strings"
	"time"

	"github.com/netforge/netforge/internal/apperr"
)

// State is where an experiment is in its lifecycle.
type State string

const (
	// StateSetup creates the laboratory. Nothing is measured yet, so there is
	// nothing here that could report a result.
	StateSetup State = "setup"
	// StateBaseline measures the path before anything is impaired. Without it
	// there is no way to tell a fault's effect from the path's own behaviour.
	StateBaseline State = "baseline"
	// StateFault injects the declared faults.
	StateFault State = "fault"
	// StateObserve measures the path while the faults are in place.
	StateObserve State = "observe"
	// StateDiagnose explains what was measured. It produces evidence, not
	// decisions.
	StateDiagnose State = "diagnose"
	// StateRecover performs the actions an operator authorized before the
	// experiment started, and nothing else.
	StateRecover State = "recover"
	// StateVerify measures the path again, so a report can say what recovery
	// actually did rather than what it was supposed to do.
	StateVerify State = "verify"
	// StateReport writes down what happened.
	StateReport State = "report"
	// StateCleanup takes the laboratory apart. It runs whether the experiment
	// finished or failed.
	StateCleanup State = "cleanup"
)

// States lists the lifecycle in order. The order is the only order: a state
// cannot be visited twice or out of turn, so a report reads as a sequence and
// not as a history of retries.
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

// Valid reports whether the state is one this lifecycle has.
func (s State) Valid() bool {
	for _, known := range States() {
		if s == known {
			return true
		}
	}
	return false
}

// next is the state that follows, in lifecycle order.
func (s State) next() (State, bool) {
	states := States()
	for i, known := range states {
		if s == known && i+1 < len(states) {
			return states[i+1], true
		}
	}
	return "", false
}

// Previous is the state before this one, which is how a failure report names
// where it happened.
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

// Transition checks that a lifecycle may move from one state to another.
//
// The lifecycle is a line, so exactly one move is legal from any state but the
// last. The check is written out rather than trusted, because the controller
// walks states from a plan, and a plan is data: if a plan could name a state
// out of order, an experiment would report a lifecycle it did not follow.
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

// Clean'sUp reports whether the cleanup step runs from this state. Every state
// can be cleaned up from, which is the requirement that a failed experiment
// never leaves a laboratory behind: there is no state a failure can strand a
// laboratory in, because cleanup follows from all of them.
func (s State) CleansUp() bool { return s.Valid() }

// CleanupFrom is the state a failure at this state cleans up from. It is the
// cleanup state itself, reached directly rather than by finishing the
// lifecycle, so a failure does not have to pretend the states after it
// happened.
func CleanupFrom(failed State) (State, error) {
	const op = "experiment.CleanupFrom"
	if !failed.CleansUp() {
		return "", apperr.Validation(op, strconv.Quote(string(failed))+" is not a state of an experiment", nil)
	}
	return StateCleanup, nil
}

// Step is one state of the lifecycle together with what the experiment will do
// in it. A plan is a list of these, and a plan is built before anything runs.
type Step struct {
	// State is the lifecycle state this step is.
	State State
	// Detail says in a sentence what the step does, for a report and for the
	// refusal a reader gets when a plan is not runnable.
	Detail string
	// Recovery is the action this step performs, for the recover step. It is
	// empty for a recovery step nothing was authorized for, which is a
	// different thing from a step that was refused.
	Recovery string
	// Deadline bounds the step. It is a ceiling rather than a schedule, and a
	// step that overruns it fails the experiment rather than overrunning the
	// duration the document allowed.
	Deadline time.Duration
}

// Describe renders what the step does, for a report and for the plan a command
// prints. A step that performs actions says so rather than describing an action
// it did not take.
func (s Step) Describe() string {
	if s.Recovery != "" {
		return "perform " + s.Recovery
	}
	return s.Detail
}

// Render renders a plan as the lifecycle a reader can check against the
// document, one state per line with what it does.
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
