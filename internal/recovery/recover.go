package recovery

import (
	"context"
	"strconv"
	"time"

	"github.com/codingseeker/netforge/internal/apperr"
)

type Executor interface {
	RemoveFault(ctx context.Context, namespace string) error
	RestoreRoute(ctx context.Context, namespace, target string) error
	RestartService(ctx context.Context, namespace string) error
	RemoveBackend(ctx context.Context, namespace, target string) error
	RestoreBackend(ctx context.Context, namespace, target string) error
	DrainConnection(ctx context.Context, namespace, target string) error
	ReEnableBackend(ctx context.Context, namespace, target string) error
}

type Outcome struct {
	Authorization Authorization

	RecoveredAt time.Time

	Verification Verification

	Err error
}

func (o Outcome) Passed() bool { return o.Err == nil && o.Verification.Passed }

type Recovery struct {
	authorizations []Authorization
	scope          Scope
	limits         Limits
	clock          func() time.Time
}

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

func (r *Recovery) Scope() Scope { return r.scope }

func (r *Recovery) Limits() Limits { return r.limits }

func (r *Recovery) Authorizations() []Authorization {
	return append([]Authorization(nil), r.authorizations...)
}

func (r *Recovery) Approved(action Action, target string) bool {
	for _, authorization := range r.authorizations {
		if authorization.action == action && authorization.target == target {
			return true
		}
	}
	return false
}

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

func (r *Recovery) verify(ctx context.Context, prober Prober, authorization Authorization) (Verification, error) {
	const op = "recovery.verify"

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

type Recommendation struct {
	Action Action

	Target string

	Reason string

	Evidence string
}

func (r Recommendation) Usable(scope Scope) bool {
	return r.Action.Valid() && scope.AllowsTarget(r.Action, r.Target) == nil
}

func kindOf(err error) apperr.Kind {
	if kind, ok := apperr.KindOf(err); ok {
		return kind
	}
	return apperr.KindInternal
}
