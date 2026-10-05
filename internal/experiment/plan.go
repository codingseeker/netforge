package experiment

import (
	"errors"
	"strconv"
	"time"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/chaos"
	"github.com/netforge/netforge/internal/config"
	"github.com/netforge/netforge/internal/probe"
	"github.com/netforge/netforge/internal/recovery"
)

// Limits bound what a plan may contain. They are the controller's own limits,
// separate from the document's, because a document says what an experiment
// wants and these say what this controller is willing to compile.
type Limits struct {
	// MaxFaults is the largest number of faults one experiment may declare.
	MaxFaults int
	// MaxRecoveries is the largest number of authorized recovery actions one
	// experiment may carry.
	MaxRecoveries int
	// MaxStep is the longest wall clock time one step may be given.
	MaxStep time.Duration
	// MaxTotal is the longest wall clock time the whole experiment may be given,
	// whatever the document's own limit says.
	MaxTotal time.Duration
	// MaxProbePayload is the largest payload any measurement may send.
	MaxProbePayload int
	// MaxProbes is the largest number of packets any measurement may send.
	MaxProbes int
}

// DefaultLimits returns limits suited to an interactive laboratory.
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

// Observation is what an experiment watches and how it is measured.
type Observation struct {
	// Namespace is the laboratory namespace the probes run in. It has to be a
	// namespace the document grants.
	Namespace string
	// Target is the address that is observed, and it has to be inside the
	// network the document grants.
	Target string
	// Port is the port measured for the protocols that use one.
	Port uint16
	// Protocol is the layer the observation is about. It is what a diagnosis
	// starts from, so it names the layer the experiment expects to break.
	Protocol probe.Protocol
	// Options are the probe options every measurement runs with.
	Options probe.Options
	// AfterFault is how long the experiment waits after a fault is injected
	// before it observes. A fault that has not reached the kernel yet would be
	// measured as a path that works.
	AfterFault time.Duration
	// Settle is how long a change is given to take effect before it is
	// measured.
	Settle time.Duration
}

// Plan is a validated experiment, ready to run.
//
// Every field has been checked. Nothing in a plan can name a namespace outside
// the scope, a fault the engine would refuse, a target outside the network, a
// measurement that costs more than the limits allow, or a recovery that was not
// authorized. Compile is the only way to get one, and it has no side effects:
// a plan that exists has changed nothing, so a refused plan leaves nothing
// behind either.
type Plan struct {
	// Name is the experiment's name, as the document wrote it.
	Name string
	// ExperimentID is the identity every fault, measurement and status of this
	// experiment is recorded under.
	ExperimentID string
	// Seed is the experiment's seed, when it declared one. It is carried
	// through to the report so a run can be repeated.
	Seed *uint64
	// Document is the document the plan was compiled from.
	Document config.Config
	// Scope is what the experiment may touch.
	Scope recovery.Scope
	// Faults are the declared faults in the engine's own form, already checked
	// against the chaos engine's limits.
	Faults []chaos.Spec
	// Observation is what the experiment watches.
	Observation Observation
	// Recoveries are the authorized recovery actions, in request order. It is
	// empty when nothing was authorized, which is not an error: an experiment
	// that only measures is a complete experiment.
	Recoveries []recovery.Authorization
	// Criteria is what a verified path has to meet.
	Criteria recovery.Criteria
	// Steps is the lifecycle, in order, with what each step does.
	Steps []Step
	// Total is the time the plan budgeted for every step together.
	Total time.Duration
	// Limits are the limits the plan was compiled under.
	Limits Limits
}

// HasFaults reports whether the experiment injects anything.
func (p Plan) HasFaults() bool { return len(p.Faults) > 0 }

// AuthorizedReports whether a recovery action was authorized for this plan.
func (p Plan) Authorized(action recovery.Action, target string) bool {
	for _, authorization := range p.Recoveries {
		if authorization.Action() == action && authorization.Target() == target {
			return true
		}
	}
	return false
}

// StepFor returns the step of a state, and whether the plan has one.
func (p Plan) StepFor(state State) (Step, bool) {
	for _, step := range p.Steps {
		if step.State == state {
			return step, true
		}
	}
	return Step{}, false
}

// Compile turns a document and a run request into a plan.
//
// It validates the whole experiment and returns it, or returns every problem it
// found and no plan at all. That is the requirement this function exists for: a
// step that turns out to be invalid while the laboratory is already running
// cannot be refused for free, so everything is checked while nothing has been
// created yet. It performs no side effects of any kind.
func Compile(document config.Config, observation Observation, requests []recovery.Request, criteria recovery.Criteria, limits Limits, now time.Time) (Plan, error) {
	const op = "experiment.Compile"
	checked, err := limits.check()
	if err != nil {
		return Plan{}, err
	}
	// Every problem is collected rather than returned at the first one. A
	// document with three mistakes says all three, so its author is not made to
	// discover them one run at a time. The checks that depend on the scope are
	// skipped when the scope itself is unusable, because there is nothing to
	// check an observation against in that case.
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

	// The faults are translated here rather than at injection time, so a fault
	// the engine would refuse is a refusal the caller gets before the
	// laboratory exists rather than a fault that half installed.
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

	// The observation is checked against the same scope, because an experiment
	// that may only touch its own namespaces has no business measuring another
	// one's path, and an address outside the network is not a laboratory path.
	if plan.Scope.Network.IsValid() {
		problems = append(problems, checkObservation(op, plan.Scope, observation, checked)...)
	} else {
		problems = append(problems, apperr.Validation(op,
			"the observation was not checked, because the document does not grant a scope to check it against", nil))
	}

	// The recovery actions are authorized now, as part of compiling, so the
	// recover step of a plan is a step that was authorized before the
	// experiment started rather than one that asks for permission while it runs.
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
		// Every problem is reported, so a document with three mistakes says
		// three things rather than making the author fix them one run at a time.
		return Plan{}, errors.Join(problems...)
	}

	plan.Steps = steps(plan, observation, checked)
	return plan, nil
}

// checkObservation validates what the experiment watches. Every problem is
// reported rather than the first, so an observation that names the wrong
// namespace and the wrong target is corrected in one pass.
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

// budget works out how long the whole experiment may take and refuses a plan
// that cannot fit inside the document's own duration limit.
//
// The sum is checked against the document's limit and against the controller's,
// because a document that allows twenty-four hours is still refused by a
// controller that will not wait that long. A fault that outlives the experiment
// is not part of this check: the chaos engine has already bounded every fault
// against its own maximum, and the engine takes faults back out when the
// laboratory is destroyed.
func budget(document config.Config, observation Observation, limits Limits) (time.Duration, error) {
	const op = "experiment.budget"
	measure := observation.Options.Deadline
	// Three measurements are taken: a baseline, an observation and a
	// verification. Each is allowed its own deadline, and none of them may
	// overrun the step limit.
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

// steps builds the lifecycle for a plan. It is the whole experiment, in order,
// with each step's ceiling worked out from the plan's own budget.
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
		// A document with no faults still walks the lifecycle. Saying so is
		// more useful than skipping the state, because a report that lists the
		// fault step and says it injected nothing is a report a reader can
		// check.
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
