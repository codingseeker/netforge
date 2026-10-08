package recovery

import (
	"strconv"
	"strings"
	"time"

	"github.com/codingseeker/netforge/internal/apperr"
)

type Action string

const (
	ActionRemoveFault Action = "remove_fault"

	ActionRestoreRoute Action = "restore_route"

	ActionRestartService Action = "restart_service"

	ActionRemoveBackend Action = "remove_backend"

	ActionRestoreBackend Action = "restore_backend"

	ActionDrainConnection Action = "drain_connection"

	ActionReEnableBackend Action = "re_enable_backend"
)

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

func (a Action) NeedsBalancer() bool {
	switch a {
	case ActionRemoveBackend, ActionRestoreBackend, ActionDrainConnection, ActionReEnableBackend:
		return true
	default:
		return false
	}
}

var actionsByName = func() map[Action]struct{} {
	index := make(map[Action]struct{}, len(Actions()))
	for _, action := range Actions() {
		index[action] = struct{}{}
	}
	return index
}()

func (a Action) String() string { return string(a) }

func (a Action) Valid() bool {
	_, known := actionsByName[a]
	return known
}

func (a Action) Destructive() bool {
	switch a {
	case ActionRemoveBackend, ActionRestartService:
		return true
	default:
		return false
	}
}

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

type Limits struct {
	MaxActions int

	MaxReasonLength int

	MaxDeadline time.Duration
}

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
