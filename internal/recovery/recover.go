package recovery

import (
	"context"
	"strconv"
	"time"

	"github.com/netforge/netforge/internal/apperr"
)

// Executor is the laboratory a recovery acts on. One method per allowed
// action, so the set of things a recovery can do is visible in this interface
// and nowhere else.
type Executor interface {
	RemoveFault(ctx context.Context, namespace string) error
	RestoreRoute(ctx context.Context, namespace, target string) error
	RestartService(ctx context.Context, namespace string) error
	RemoveBackend(ctx context.Context, namespace, target string) error
	RestoreBackend(ctx context.Context, namespace, target string) error
	DrainConnection(ctx context.Context, namespace, target string) error
	ReEnableBackend(ctx context.Context, namespace, target string) error
}

// Outcome is what one authorized action did, and whether the measurement
// afterwards agreed.
type Outcome struct {
	// Authorization is the action that was performed.
	Authorization Authorization
	// RecoveredAt is when the action finished.
	RecoveredAt time.Time
	// Verification is the measurement taken after the action. It is present
	// even when the action failed, because a path that was still broken when
	// the action gave up is the most useful thing a failed recovery can report.
	Verification Verification
	// Err is why the action failed, if it did.
	Err error
}

// Passed reports whether the action ran and the measurement agreed with it.
func (o Outcome) Passed() bool { return o.Err == nil && o.Verification.Passed }

// Recovery is a set of authorized actions and the measurements that follow
// them.
type Recovery struct {
	authorizations []Authorization
	scope          Scope
	limits         Limits
	clock          func() time.Time
}

// New builds a recovery from a set of requests.
//
// Every request is validated before any of them is run, for the same reason an
// experiment is validated before it starts: a second request that turns out to
// be invalid must not be discovered after the first one has already changed
// the laboratory. The returned error names the first refusal and reports how
// many requests were checked, so a caller can tell a rejected set from a
// partial one.
func New(scope Scope, requests []Request, criteria Criteria, limits Limits, now time.Time) (*Recovery, error) {
	const op = "recovery.New"
	checked, err := limits.check()
	if err != nil {
		return nil, err
	}
	if len(requests) == 0 {
		return nil, apperr.Validation(op, "a recovery needs at least one authorized action", nil)
	}
	if len(requests) > checked.MaxActions {
		return nil, apperr.LimitExceeded(op, "the recovery carries "+strconv.Itoa(len(requests))+
			" actions, the maximum is "+strconv.Itoa(checked.MaxActions), nil)
	}
	// A scope with no network or no namespaces would let a request through
	// validation that grants nothing, so it is refused before the requests
	// are read rather than letting every one of them fail in turn.
	if !scope.Network.IsValid() || len(scope.Namespaces) == 0 {
		return nil, apperr.InvalidArgument(op, "a recovery needs a scope with a network and at least one namespace", nil)
	}

	authorizations := make([]Authorization, 0, len(requests))
	seen := make(map[string]int, len(requests))
	for i, request := range requests {
		field := "requests[" + strconv.Itoa(i) + "]"
		authorization, err := Authorize(scope, request, criteria, checked, now)
		if err != nil {
			return nil, apperr.Wrap(err, kindOf(err), op, field+" is not an authorized recovery action")
		}
		// Two identical actions would be performed twice, and a drain applied
		// twice is a drain that behaves differently the second time.
		key := string(authorization.action) + " " + authorization.target
		if first, duplicate := seen[key]; duplicate {
			return nil, apperr.Validation(op, field+" repeats requests["+strconv.Itoa(first)+"]: "+key, nil)
		}
		seen[key] = i
		authorizations = append(authorizations, authorization)
	}
	return &Recovery{
		authorizations: authorizations,
		scope:          scope,
		limits:         checked,
		clock:          func() time.Time { return now },
	}, nil
}

// Scope returns the scope the recovery is confined to.
func (r *Recovery) Scope() Scope { return r.scope }

// Limits returns the limits the recovery enforces.
func (r *Recovery) Limits() Limits { return r.limits }

// Authorizations returns the actions that were authorized, in request order.
func (r *Recovery) Authorizations() []Authorization {
	return append([]Authorization(nil), r.authorizations...)
}

// Approved reports whether an action and target are covered by this recovery.
// A controller uses it to refuse a step its recovery was never authorized for,
// rather than discovering it halfway through the laboratory.
func (r *Recovery) Approved(action Action, target string) bool {
	for _, authorization := range r.authorizations {
		if authorization.action == action && authorization.target == target {
			return true
		}
	}
	return false
}

// Execute performs every authorized action and verifies each one by
// measurement.
//
// It stops at the first failure. A laboratory that is already half repaired is
// harder to reason about than one that was left alone, so the actions after a
// failure are not attempted, and the caller is told which ones ran.
func (r *Recovery) Execute(ctx context.Context, executor Executor, prober Prober) ([]Outcome, error) {
	const op = "recovery.Execute"
	if executor == nil {
		return nil, apperr.InvalidArgument(op, "a recovery needs an executor to act through", nil)
	}
	if prober == nil {
		return nil, apperr.InvalidArgument(op, "a recovery needs a prober to verify with", nil)
	}

	outcomes := make([]Outcome, 0, len(r.authorizations))
	for i, authorization := range r.authorizations {
		// The scope is checked again here, not only when the authorization was
		// issued. The laboratory that runs the action is the one that knows
		// whether the target is still the thing it was authorized for, and
		// this is the last point at which a refusal is still free.
		if err := authorization.scope.AllowsTarget(authorization.action, authorization.target); err != nil {
			outcomes = append(outcomes, Outcome{Authorization: authorization, Err: err})
			return outcomes, apperr.Wrap(err, kindOf(err), op,
				"action "+strconv.Itoa(i)+" is no longer inside the scope it was authorized in")
		}
		if err := r.perform(ctx, executor, authorization); err != nil {
			outcomes = append(outcomes, Outcome{Authorization: authorization, RecoveredAt: r.clock(), Err: err})
			return outcomes, err
		}
		outcome := Outcome{Authorization: authorization, RecoveredAt: r.clock()}
		verification, err := r.verify(ctx, prober, authorization)
		outcome.Verification = verification
		outcome.Err = err
		outcomes = append(outcomes, outcome)
		if err != nil {
			return outcomes, err
		}
		if !verification.Passed {
			return outcomes, apperr.Validation(op, "action "+authorization.String()+
				" ran but the path did not recover: "+verification.Reason, nil)
		}
	}
	return outcomes, nil
}

// perform dispatches one authorized action. The action was validated twice
// already, so the switch is total and a default is a refusal rather than a
// silent success.
func (r *Recovery) perform(ctx context.Context, executor Executor, authorization Authorization) error {
	const op = "recovery.perform"
	target := authorization.target
	switch authorization.action {
	case ActionRemoveFault:
		return executor.RemoveFault(ctx, target)
	case ActionRestoreRoute:
		return executor.RestoreRoute(ctx, authorization.from, target)
	case ActionRestartService:
		return executor.RestartService(ctx, target)
	case ActionRemoveBackend:
		return executor.RemoveBackend(ctx, authorization.from, target)
	case ActionRestoreBackend:
		return executor.RestoreBackend(ctx, authorization.from, target)
	case ActionDrainConnection:
		return executor.DrainConnection(ctx, authorization.from, target)
	case ActionReEnableBackend:
		return executor.ReEnableBackend(ctx, authorization.from, target)
	default:
		return apperr.Unsupported(op, strconv.Quote(string(authorization.action))+" has no executor", nil)
	}
}

// verify measures the path the action was meant to repair. A measurement that
// cannot be taken is a verification that failed, not a verification that was
// skipped: a repair nobody measured has not been shown to be a repair.
func (r *Recovery) verify(ctx context.Context, prober Prober, authorization Authorization) (Verification, error) {
	const op = "recovery.verify"
	// The context is checked before the probe is asked to run, because a
	// prober is entitled to ignore a context it is handed and a repair that
	// has already been abandoned should not be certified by a measurement
	// taken afterwards.
	namespace, target, options := authorization.measurement()
	if err := apperr.FromContext(ctx, op); err != nil {
		return Verification{Namespace: namespace, Target: target, At: r.clock()}, err
	}
	measurement, err := prober.Measure(ctx, namespace, target, options)
	if err != nil {
		if ctxErr := apperr.FromContext(ctx, op); ctxErr != nil {
			return Verification{Namespace: namespace, Target: target, At: r.clock()}, ctxErr
		}
		return Verification{Namespace: namespace, Target: target, At: r.clock()},
			apperr.Wrap(err, kindOf(err), op, "the path after "+authorization.String()+" could not be measured")
	}
	passed, reason := authorization.criteria.Judge(measurement)
	return Verification{
		Namespace:   namespace,
		Target:      target,
		Measurement: measurement,
		Passed:      passed,
		Reason:      reason,
		At:          r.clock(),
	}, nil
}

// Verify measures every authorized action's path without changing anything. It
// is how a caller checks the current state of a laboratory before deciding
// whether a recovery is warranted at all.
func (r *Recovery) Verify(ctx context.Context, prober Prober) ([]Verification, error) {
	const op = "recovery.Verify"
	if prober == nil {
		return nil, apperr.InvalidArgument(op, "a recovery needs a prober to verify with", nil)
	}
	verifications := make([]Verification, 0, len(r.authorizations))
	for _, authorization := range r.authorizations {
		verification, err := r.verify(ctx, prober, authorization)
		verifications = append(verifications, verification)
		if err != nil {
			return verifications, err
		}
	}
	return verifications, nil
}

// Recommendation is what a diagnosis suggests. It is not an authorization: it
// has no way to reach an executor, and the only way to act on it is to write a
// request with Consent set by a caller that means it.
type Recommendation struct {
	// Action is the suggested action.
	Action Action
	// Target is what it would act on.
	Target string
	// Reason is why the diagnosis suggests it.
	Reason string
	// Evidence names the report the suggestion came from.
	Evidence string
}

// Usable reports whether the recommendation names an action a recovery could
// actually perform on a target inside the scope. A recommendation that fails
// this is worth reporting and not worth acting on.
func (r Recommendation) Usable(scope Scope) bool {
	return r.Action.Valid() && scope.AllowsTarget(r.Action, r.Target) == nil
}

func kindOf(err error) apperr.Kind {
	if kind, ok := apperr.KindOf(err); ok {
		return kind
	}
	return apperr.KindInternal
}
