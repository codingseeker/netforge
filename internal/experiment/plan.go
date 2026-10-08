package experiment

import (
	"errors"
	"strconv"
	"time"

	"github.com/codingseeker/netforge/internal/apperr"
	"github.com/codingseeker/netforge/internal/chaos"
	"github.com/codingseeker/netforge/internal/config"
	"github.com/codingseeker/netforge/internal/probe"
	"github.com/codingseeker/netforge/internal/recovery"
)

type Limits struct {
	MaxFaults int

	MaxRecoveries int

	MaxStep time.Duration

	MaxTotal time.Duration

	MaxProbePayload int

	MaxProbes int
}

func DefaultLimits() Limits {
	return Limits{
		MaxFaults:       16,
		MaxRecoveries:   8,
		MaxStep:         2 * time.Minute,
		MaxTotal:        30 * time.Minute,
		MaxProbePayload: 1400,
		MaxProbes:       100,
	}
}

func (l Limits) check() (Limits, error) {
	const op = "experiment.Limits"
	switch {
	case l.MaxFaults <= 0:
		return l, apperr.InvalidArgument(op, "limits.max_faults must be positive", nil)
	case l.MaxRecoveries <= 0:
		return l, apperr.InvalidArgument(op, "limits.max_recoveries must be positive", nil)
	case l.MaxStep <= 0:
		return l, apperr.InvalidArgument(op, "limits.max_step must be positive", nil)
	case l.MaxTotal <= 0:
		return l, apperr.InvalidArgument(op, "limits.max_total must be positive", nil)
	case l.MaxStep > l.MaxTotal:
		return l, apperr.InvalidArgument(op, "limits.max_step "+l.MaxStep.String()+
			" is longer than limits.max_total "+l.MaxTotal.String(), nil)
	case l.MaxProbePayload <= 0:
		return l, apperr.InvalidArgument(op, "limits.max_probe_payload must be positive", nil)
	case l.MaxProbes <= 0:
		return l, apperr.InvalidArgument(op, "limits.max_probes must be positive", nil)
	}
	return l, nil
}

type Observation struct {
	Namespace string

	Target string

	Port uint16

	Protocol probe.Protocol

	Options probe.Options

	AfterFault time.Duration

	Settle time.Duration
}

type Plan struct {
	Name string

	ExperimentID string

	Seed *uint64

	Document config.Config

	Scope recovery.Scope

	Faults []chaos.Spec

	Observation Observation

	Recoveries []recovery.Authorization

	Criteria recovery.Criteria

	Steps []Step

	Total time.Duration

	Limits Limits
}

func (p Plan) HasFaults() bool { return len(p.Faults) > 0 }

func (p Plan) Authorized(action recovery.Action, target string) bool {
	for _, authorization := range p.Recoveries {
		if authorization.Action() == action && authorization.Target() == target {
			return true
		}
	}
	return false
}

func (p Plan) StepFor(state State) (Step, bool) {
	for _, step := range p.Steps {
		if step.State == state {
			return step, true
		}
	}
	return Step{}, false
}

func Compile(document config.Config, observation Observation, requests []recovery.Request, criteria recovery.Criteria, limits Limits, now time.Time) (Plan, error) {
	const op = "experiment.Compile"
	checked, err := limits.check()
	if err != nil {
		return Plan{}, err
	}

	var problems []error
	if err := document.Validate(); err != nil {
		problems = append(problems, apperr.Wrap(err, kindOf(err), op, "the experiment document is not valid"))
	}

	plan := Plan{
		Name:         document.Name,
		ExperimentID: document.ExperimentID(),
		Seed:         document.Seed,
		Document:     document,
		Observation:  observation,
		Criteria:     criteria,
		Limits:       checked,
	}
	scope, err := recovery.ScopeOf(document)
	if err != nil {
		problems = append(problems, apperr.Wrap(err, kindOf(err), op, "the experiment has no usable scope"))
	} else {
		plan.Scope = scope
	}

	if len(document.Faults) > checked.MaxFaults {
		problems = append(problems, apperr.LimitExceeded(op, "the document declares "+strconv.Itoa(len(document.Faults))+
			" faults, the maximum is "+strconv.Itoa(checked.MaxFaults), nil))
	}
	for i := range document.Faults {
		spec, err := document.Faults[i].Translate(document.Limits)
		if err != nil {
			problems = append(problems, apperr.Wrap(err, kindOf(err), op,
				"faults["+strconv.Itoa(i)+"] cannot be applied"))
			continue
		}
		plan.Faults = append(plan.Faults, spec)
	}

	if plan.Scope.Network.IsValid() {
		problems = append(problems, checkObservation(op, plan.Scope, observation, checked)...)
	} else {
		problems = append(problems, apperr.Validation(op,
			"the observation was not checked, because the document does not grant a scope to check it against", nil))
	}

	if len(requests) > checked.MaxRecoveries {
		problems = append(problems, apperr.LimitExceeded(op, "the run carries "+strconv.Itoa(len(requests))+
			" recovery actions, the maximum is "+strconv.Itoa(checked.MaxRecoveries), nil))
	} else if len(requests) > 0 && plan.Scope.Network.IsValid() {
		authorized, err := recovery.New(plan.Scope, requests, criteria, recovery.DefaultLimits(), now)
		if err != nil {
			problems = append(problems, apperr.Wrap(err, kindOf(err), op, "the recovery actions are not all authorized"))
		} else {
			plan.Recoveries = authorized.Authorizations()
		}
	}

	if plan.Total, err = budget(document, observation, checked); err != nil {
		problems = append(problems, err)
	}
	if len(problems) > 0 {

		return Plan{}, errors.Join(problems...)
	}

	plan.Steps = steps(plan, observation, checked)
	return plan, nil
}

func checkObservation(op string, scope recovery.Scope, observation Observation, limits Limits) []error {
	var problems []error
	if observation.Namespace == "" {
		problems = append(problems, apperr.Validation(op,
			"observation.namespace is required, so the experiment knows where it measures from", nil))
	} else if !scope.AllowsNamespace(observation.Namespace) {
		problems = append(problems, apperr.Permission(op, "observation.namespace "+strconv.Quote(observation.Namespace)+
			" is not granted by scope.namespaces", nil))
	}
	if observation.Target == "" {
		problems = append(problems, apperr.Validation(op, "observation.target is required", nil))
	} else if err := scope.AllowsObservation(observation.Target); err != nil {
		problems = append(problems, err)
	}
	if !observation.Protocol.Valid() {
		problems = append(problems, apperr.Unsupported(op, "observation.protocol "+
			strconv.Quote(string(observation.Protocol))+" is not a protocol a laboratory measures", nil))
	}
	switch {
	case observation.Options.Count <= 0:
		problems = append(problems, apperr.Validation(op, "observation.options.count must be positive", nil))
	case observation.Options.Count > limits.MaxProbes:
		problems = append(problems, apperr.LimitExceeded(op, "observation.options.count is "+
			strconv.Itoa(observation.Options.Count)+", the maximum is "+strconv.Itoa(limits.MaxProbes), nil))
	}
	switch {
	case observation.Options.Size <= 0:
		problems = append(problems, apperr.Validation(op, "observation.options.size must be positive", nil))
	case observation.Options.Size > limits.MaxProbePayload:
		problems = append(problems, apperr.LimitExceeded(op, "observation.options.size is "+
			strconv.Itoa(observation.Options.Size)+" bytes, the maximum is "+strconv.Itoa(limits.MaxProbePayload), nil))
	}
	switch {
	case observation.Options.Timeout <= 0:
		problems = append(problems, apperr.Validation(op, "observation.options.timeout must be positive", nil))
	case observation.Options.Deadline <= 0:
		problems = append(problems, apperr.Validation(op, "observation.options.deadline must be positive", nil))
	case observation.Options.Deadline > limits.MaxStep:
		problems = append(problems, apperr.LimitExceeded(op, "observation.options.deadline is "+
			observation.Options.Deadline.String()+", the longest one step may take is "+limits.MaxStep.String(), nil))
	}
	if observation.AfterFault < 0 {
		problems = append(problems, apperr.Validation(op, "observation.after_fault must not be negative", nil))
	}
	if observation.Settle < 0 {
		problems = append(problems, apperr.Validation(op, "observation.settle must not be negative", nil))
	}
	if observation.Protocol.Valid() && observation.Protocol != probe.ProtocolICMP && observation.Port == 0 {
		problems = append(problems, apperr.Validation(op,
			"observation.port is required for "+string(observation.Protocol), nil))
	}
	return problems
}

func budget(document config.Config, observation Observation, limits Limits) (time.Duration, error) {
	const op = "experiment.budget"
	measure := observation.Options.Deadline

	if measure > limits.MaxStep {
		return 0, apperr.LimitExceeded(op, "one measurement is budgeted "+measure.String()+
			", the longest one step may take is "+limits.MaxStep.String(), nil)
	}
	total := 3*measure + observation.AfterFault + 3*observation.Settle
	if total > limits.MaxTotal {
		return 0, apperr.LimitExceeded(op, "the experiment needs "+total.String()+
			" and the longest this controller will run one is "+limits.MaxTotal.String(), nil)
	}
	allowed := document.Limits.MaxDuration.Duration()
	if total > allowed {
		return 0, apperr.LimitExceeded(op, "the experiment needs "+total.String()+
			" but limits.max_duration is "+allowed.String(), nil)
	}
	return total, nil
}

func steps(plan Plan, observation Observation, limits Limits) []Step {
	measure := observation.Options.Deadline
	setup := limits.MaxStep
	if measure > setup {
		setup = measure
	}
	out := []Step{
		{State: StateSetup, Detail: "create the laboratory from the document", Deadline: limits.MaxStep},
		{State: StateBaseline, Detail: "measure the path before anything is impaired", Deadline: measure},
		{State: StateFault, Detail: "inject the declared faults", Deadline: measure},
		{State: StateObserve, Detail: "measure the path while the faults are in place", Deadline: measure},
		{State: StateDiagnose, Detail: "explain the measurement with evidence", Deadline: measure},
		{State: StateRecover, Detail: "perform the authorized recovery actions", Deadline: measure},
		{State: StateVerify, Detail: "measure the path after recovery", Deadline: measure},
		{State: StateReport, Detail: "write down what happened", Deadline: limits.MaxStep},
		{State: StateCleanup, Detail: "take the laboratory apart", Deadline: limits.MaxStep},
	}
	if len(plan.Faults) == 0 {

		out[2].Detail = "inject the declared faults: the document declares none"
	}
	if len(plan.Recoveries) == 0 {
		out[5].Detail = "perform the authorized recovery actions: none were authorized, so nothing is recovered"
	} else {
		names := make([]string, 0, len(plan.Recoveries))
		for _, authorization := range plan.Recoveries {
			names = append(names, authorization.String())
		}
		out[5].Recovery = joinNames(names)
	}
	return out
}

func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	joined := names[0]
	for _, name := range names[1 : len(names)-1] {
		joined += ", " + name
	}
	return joined + " and " + names[len(names)-1]
}

func kindOf(err error) apperr.Kind {
	if kind, ok := apperr.KindOf(err); ok {
		return kind
	}
	return apperr.KindInternal
}
