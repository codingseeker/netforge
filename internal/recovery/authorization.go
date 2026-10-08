package recovery

import (
	"strconv"
	"time"

	"github.com/codingseeker/netforge/internal/apperr"
	"github.com/codingseeker/netforge/internal/probe"
)

type Request struct {
	Action Action

	Target string

	From string

	Check *Check

	Reason string

	Consent bool
}

type Check struct {
	Namespace string

	Target string

	Options probe.Options
}

type Authorization struct {
	action   Action
	target   string
	from     string
	reason   string
	options  probe.Options
	check    *Check
	scope    Scope
	granted  time.Time
	criteria Criteria
}

func (a Authorization) Check() *Check { return a.check }

func (a Authorization) measurement() (namespace, target string, options probe.Options) {
	if a.check != nil {
		return a.check.Namespace, a.check.Target, a.check.Options
	}
	return a.from, a.target, a.options
}

func (a Authorization) Action() Action { return a.action }

func (a Authorization) Target() string { return a.target }

func (a Authorization) From() string { return a.from }

func (a Authorization) Reason() string { return a.reason }

func (a Authorization) GrantedAt() time.Time { return a.granted }

func (a Authorization) Options() probe.Options { return a.options }

func (a Authorization) Criteria() Criteria { return a.criteria }

func (a Authorization) Scope() Scope { return a.scope }

func (a Authorization) String() string {
	return string(a.action) + " " + a.target + " (from " + a.from + ")"
}

func Authorize(scope Scope, request Request, criteria Criteria, limits Limits, now time.Time) (Authorization, error) {
	const op = "recovery.Authorize"

	if !request.Consent {
		return Authorization{}, apperr.Permission(op,
			"a recovery needs explicit authorization, and none was given for "+request.describe(actionOrNone(request.Action)), nil)
	}
	if err := request.Action.check(op, "action"); err != nil {
		return Authorization{}, err
	}
	action := request.Action
	if err := scope.AllowsTarget(action, request.Target); err != nil {
		return Authorization{}, err
	}
	from, err := resolveFrom(scope, action, request)
	if err != nil {
		return Authorization{}, err
	}
	reason, err := checkReason(op, request.Reason, limits)
	if err != nil {
		return Authorization{}, err
	}
	options, err := verificationOptions(limits)
	if err != nil {
		return Authorization{}, err
	}
	check, err := authorizeCheck(op, scope, request.Check, options, limits)
	if err != nil {
		return Authorization{}, err
	}
	checked, err := criteria.check(op, limits)
	if err != nil {
		return Authorization{}, err
	}
	return Authorization{
		action:   action,
		target:   request.Target,
		from:     from,
		reason:   reason,
		options:  options,
		check:    check,
		scope:    scope,
		granted:  now.UTC(),
		criteria: checked,
	}, nil
}

func authorizeCheck(op string, scope Scope, check *Check, defaults probe.Options, limits Limits) (*Check, error) {
	if check == nil {
		return nil, nil
	}
	if check.Namespace == "" {
		return nil, apperr.Validation(op, "a check needs the namespace the measurement is taken from", nil)
	}
	if _, granted := scope.lookup(check.Namespace); !granted {
		return nil, apperr.Permission(op, "check.namespace "+strconv.Quote(check.Namespace)+
			" is not in the scope of experiment "+scope.Experiment, nil)
	}
	if err := scope.AllowsObservation(check.Target); err != nil {
		return nil, err
	}
	options := check.Options
	if options.Count <= 0 {
		options.Count = defaults.Count
	}
	if options.Interval <= 0 {
		options.Interval = defaults.Interval
	}
	if options.Size <= 0 {
		options.Size = defaults.Size
	}
	if options.Timeout <= 0 {
		options.Timeout = defaults.Timeout
	}
	if options.Deadline <= 0 {
		options.Deadline = defaults.Deadline
	}

	if options.Deadline > limits.MaxDeadline {
		return nil, apperr.LimitExceeded(op, "check.options.deadline is "+options.Deadline.String()+
			", the longest a verification may take is "+limits.MaxDeadline.String(), nil)
	}
	return &Check{Namespace: check.Namespace, Target: check.Target, Options: options}, nil
}

func (o Request) describe(action Action) string {
	if o.Target == "" {
		return string(action)
	}
	return string(action) + " on " + o.Target
}

func actionOrNone(action Action) Action {
	if action == "" {
		return "no action"
	}
	return action
}

func resolveFrom(scope Scope, action Action, request Request) (string, error) {
	const op = "recovery.Authorize"
	from := request.From
	if from == "" && !action.NeedsAddress() {
		from = request.Target
	}
	if from == "" {
		return "", apperr.Validation(op, "from is required for "+string(action)+
			", because the target is an address and does not say where the verification is measured from", nil)
	}
	if _, granted := scope.lookup(from); !granted {
		return "", apperr.Permission(op, "verification namespace "+strconv.Quote(from)+
			" is not in the scope of experiment "+scope.Experiment, nil)
	}
	return from, nil
}

func checkReason(op, reason string, limits Limits) (string, error) {
	if reason == "" {
		return "", apperr.Validation(op, "reason is required, so a later reader can see why the laboratory was changed", nil)
	}
	if len(reason) > limits.MaxReasonLength {
		return "", apperr.LimitExceeded(op, "reason is "+strconv.Itoa(len(reason))+
			" characters, the maximum is "+strconv.Itoa(limits.MaxReasonLength), nil)
	}
	return reason, nil
}

func verificationOptions(limits Limits) (probe.Options, error) {
	const op = "recovery.Authorize"
	options := probe.Options{
		Count:    5,
		Interval: 20 * time.Millisecond,
		Size:     64,
		Timeout:  limits.MaxDeadline / 5,
		Deadline: limits.MaxDeadline,
	}
	if options.Timeout <= 0 {
		return probe.Options{}, apperr.InvalidArgument(op, "limits.max_deadline is too short to verify a path", nil)
	}
	return options, nil
}
