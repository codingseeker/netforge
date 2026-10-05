// Package recovery carries out the repair step of an experiment.
//
// The rule this package exists to enforce is that a diagnosis is not a
// permission. The investigator can measure a failure and name the layer it
// belongs to, and everything it produces is evidence, not authority. Repair
// changes a laboratory that is running, so it happens only when an operator
// asked for it by name, inside a scope they declared, and only after the
// action itself has been checked against that scope.
//
// Every authorized action therefore passes the same chain, in this order:
//
//	explicit authorization
//	        +
//	scope validation
//	        +
//	action validation
//	        +
//	actual recovery
//	        +
//	verification
//
// A missing link refuses the action. Nothing here infers consent, and nothing
// here reports success without a measurement behind it.
package recovery

import (
	"strconv"
	"strings"
	"time"

	"github.com/netforge/netforge/internal/apperr"
)

// Action is one repair a recovery may perform.
//
// The set is deliberately small and closed. A recovery that could do
// anything the laboratory can do would be a second, unaudited interface to
// the kernel, so the allowed actions are named here rather than discovered at
// run time.
type Action string

const (
	// ActionRemoveFault takes an injected fault back out of the kernel.
	ActionRemoveFault Action = "remove_fault"
	// ActionRestoreRoute puts a route back the way it was.
	ActionRestoreRoute Action = "restore_route"
	// ActionRestartService restarts a laboratory service process.
	ActionRestartService Action = "restart_service"
	// ActionRemoveBackend takes a backend out of service.
	ActionRemoveBackend Action = "remove_backend"
	// ActionRestoreBackend puts a backend back into service.
	ActionRestoreBackend Action = "restore_backend"
	// ActionDrainConnection stops new work on a connection and lets the
	// established one finish.
	ActionDrainConnection Action = "drain_connection"
	// ActionReEnableBackend returns a drained or removed backend to service.
	ActionReEnableBackend Action = "re_enable_backend"
)

// Actions lists every action a recovery may perform, in a fixed order so a
// refusal can name the alternatives in the same order every time.
func Actions() []Action {
	return []Action{
		ActionRemoveFault,
		ActionRestoreRoute,
		ActionRestartService,
		ActionRemoveBackend,
		ActionRestoreBackend,
		ActionDrainConnection,
		ActionReEnableBackend,
	}
}

// actionsByName indexes Actions for lookup and for error messages.
var actionsByName = func() map[Action]struct{} {
	index := make(map[Action]struct{}, len(Actions()))
	for _, action := range Actions() {
		index[action] = struct{}{}
	}
	return index
}()

func (a Action) String() string { return string(a) }

// Valid reports whether the action is one a recovery may perform.
func (a Action) Valid() bool {
	_, known := actionsByName[a]
	return known
}

// Destructive reports whether the action removes something that is currently
// serving. It is what makes an action worth an explicit authorization: a
// destructive action taken on a suspicion takes the laboratory down, and the
// measurement that suggested the fault may be the fault.
func (a Action) Destructive() bool {
	switch a {
	case ActionRemoveBackend, ActionRestartService:
		return true
	default:
		return false
	}
}

// NeedsAddress reports whether the action is performed on an address rather
// than on a laboratory namespace. The two are kept apart because a namespace
// name and an address are validated differently: a name has to be one the
// scope lists, an address has to fall inside the network the scope grants.
func (a Action) NeedsAddress() bool {
	switch a {
	case ActionRestoreRoute, ActionRemoveBackend, ActionRestoreBackend, ActionDrainConnection, ActionReEnableBackend:
		return true
	default:
		return false
	}
}

func (a Action) check(op, field string) error {
	if a == "" {
		return apperr.Validation(op, field+" is required, want one of "+actionList(), nil)
	}
	if !a.Valid() {
		return apperr.Unsupported(op, field+" "+strconv.Quote(string(a))+" is not an action a recovery may perform, want one of "+actionList(), nil)
	}
	return nil
}

func actionList() string {
	names := make([]string, 0, len(Actions()))
	for _, action := range Actions() {
		names = append(names, string(action))
	}
	return strings.Join(names, ", ")
}

// Limits bounds what a recovery may cost. They are checked once, when the
// recovery is built, so an over-large request is refused before it holds an
// authorization.
type Limits struct {
	// MaxActions is how many authorized actions one recovery may carry.
	MaxActions int
	// MaxReasonLength is the longest justification an authorization may
	// carry. A recovery that has to explain itself in prose is a recovery an
	// operator is expected to read.
	MaxReasonLength int
	// MaxDeadline is how long the verification of one action may take.
	MaxDeadline time.Duration
}

// DefaultLimits returns limits suited to an interactive laboratory.
func DefaultLimits() Limits {
	return Limits{
		MaxActions:      8,
		MaxReasonLength: 512,
		MaxDeadline:     30 * time.Second,
	}
}

func (l Limits) check() (Limits, error) {
	const op = "recovery.Limits"
	switch {
	case l.MaxActions <= 0:
		return l, apperr.InvalidArgument(op, "limits.max_actions must be positive", nil)
	case l.MaxReasonLength <= 0:
		return l, apperr.InvalidArgument(op, "limits.max_reason_length must be positive", nil)
	case l.MaxDeadline <= 0:
		return l, apperr.InvalidArgument(op, "limits.max_deadline must be positive", nil)
	}
	return l, nil
}
