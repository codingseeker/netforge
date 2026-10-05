package recovery

import (
	"strconv"
	"time"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/probe"
)

// Request is what an operator asks a laboratory to do about a failure.
//
// Consent is a field rather than an implication because a request that can be
// inferred is a request that can be forged by whatever produced the failure
// it answers. A caller that wants a repair has to say so.
type Request struct {
	// Action is the repair to perform. It has to be one of Actions.
	Action Action
	// Target is the namespace or address the action acts on. Which of the two
	// it is follows from the action.
	Target string
	// From is the namespace the verification probe runs in. It is required
	// for an action that acts on an address, because an address alone cannot
	// say where the measurement comes from.
	From string
	// Check is the measurement that decides whether the repair worked. It is
	// what the experiment was watching, and it is not the same thing as the
	// action's target: a fault is removed from a namespace, but it is the path
	// to the target that is measured to find out whether removing it mattered.
	// When it is absent the action's own namespace and target are measured, which
	// is right for an action whose target is an address.
	Check *Check
	// Reason is the operator's justification. It is carried into the report
	// so a later reader can see why the laboratory was changed.
	Reason string
	// Consent is the explicit authorization. Without it the request is
	// refused, whatever the diagnosis says.
	Consent bool
}

// Authorization is a request that passed every check: the action exists, the
// target is inside the scope, and an operator asked for it by name.
//
// The fields are unexported on purpose. A value of this type cannot be written
// by a caller, so holding one means the checks ran. That is what keeps a
// diagnosis from being mistaken for a permission: the investigator produces
// evidence, and the only producer of an Authorization is Authorize.
// Check is the measurement a repair is judged by: where the probe runs, what it
// probes, and how.
type Check struct {
	// Namespace is the namespace the probe runs in.
	Namespace string
	// Target is the address the probe is sent to.
	Target string
	// Options are the probe's settings, within the recovery's own limits.
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

// Check returns the measurement this authorization is judged by, or nil when the
// action's own namespace and target are measured.
func (a Authorization) Check() *Check { return a.check }

// measurement reports what a verification of this authorization would measure,
// which is the check when one was given and the action's own target otherwise.
func (a Authorization) measurement() (namespace, target string, options probe.Options) {
	if a.check != nil {
		return a.check.Namespace, a.check.Target, a.check.Options
	}
	return a.from, a.target, a.options
}

// Action returns the authorized action.
func (a Authorization) Action() Action { return a.action }

// Target returns what the action acts on.
func (a Authorization) Target() string { return a.target }

// From returns the namespace the verification probe runs in.
func (a Authorization) From() string { return a.from }

// Reason returns the operator's justification.
func (a Authorization) Reason() string { return a.reason }

// GrantedAt is when the authorization was issued.
func (a Authorization) GrantedAt() time.Time { return a.granted }

// Options returns the probe options verification runs with.
func (a Authorization) Options() probe.Options { return a.options }

// Criteria returns what counts as a recovered path.
func (a Authorization) Criteria() Criteria { return a.criteria }

// Scope returns the scope the authorization was granted in.
func (a Authorization) Scope() Scope { return a.scope }

// String renders the authorization for a log line.
func (a Authorization) String() string {
	return string(a.action) + " " + a.target + " (from " + a.from + ")"
}

// Authorize validates one request into an authorization.
//
// The order is the order the requirements are written in, and it is also the
// order a refusal should read in: there is no authorization, then there is no
// scope, then there is no valid action. A caller that got all three wrong is
// told about the first thing that is missing rather than the last.
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

// authorizeCheck validates the measurement an action is judged by. Both halves
// are scope-checked for the same reason the action's target is: a measurement is
// a request against the laboratory, and one taken from outside the scope or of
// an address outside it would be evidence about something else.
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
	// A check carries the recovery's own probe budget rather than a budget of its
	// own. The limits that exist here are the action count, the length of a
	// justification and the deadline of one verification, and a check is judged
	// against the last of those: an operator who widened a deadline by hand would
	// be able to turn a fast verdict into a slow one.
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

// resolveFrom decides the namespace a verification probe runs in. It is the
// target itself for an action on a namespace, and an explicit choice for an
// action on an address, which cannot imply one.
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

// verificationOptions returns the probe options verification uses. They are
// small and fixed rather than configurable, because a repair that failed must
// still be verifiable within the operator's deadline.
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
