package experiment

import (
	"context"
	"strconv"
	"time"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/chaos"
	"github.com/netforge/netforge/internal/investigator"
	"github.com/netforge/netforge/internal/probe"
	"github.com/netforge/netforge/internal/recovery"
)

// Environment is everything an experiment acts on.
//
// It is an interface so the controller is about the lifecycle and the
// environment is about the laboratory. A test drives the whole lifecycle
// without a kernel, and the laboratory adapter is the only code that has to know
// how a namespace, a fault and a probe actually work.
type Environment interface {
	// Setup creates the laboratory.
	Setup(ctx context.Context) error
	// Baseline measures the path before anything is impaired.
	Baseline(ctx context.Context) ([]probe.Measurement, error)
	// ApplyFaults injects the plan's faults and returns the records the engine
	// made, so the report names faults rather than intentions.
	ApplyFaults(ctx context.Context) ([]chaos.Fault, error)
	// Observe measures the path while the faults are in place.
	Observe(ctx context.Context) ([]probe.Measurement, error)
	// Diagnose explains the measurements.
	Diagnose(ctx context.Context, observed []probe.Measurement) (investigator.Report, error)
	// Recover performs the plan's authorized actions. It is given the whole
	// authorization list so the environment can refuse one that is not covered,
	// and it is expected to do nothing at all when the list is empty.
	Recover(ctx context.Context, authorizations []recovery.Authorization) ([]recovery.Outcome, error)
	// Verify measures the path after recovery.
	Verify(ctx context.Context) ([]probe.Measurement, error)
	// Teardown takes the laboratory apart. It is called exactly once, whether
	// the experiment finished or failed, and it must be safe to call on a
	// laboratory that was never created.
	Teardown(ctx context.Context) error
}

// StepReport is what happened in one state.
type StepReport struct {
	// State is the state this was.
	State State
	// Detail is what the step was going to do.
	Detail string
	// Started is when it began.
	Started time.Time
	// Duration is how long it took.
	Duration time.Duration
	// Err is why it failed, if it did.
	Err error
	// Recovery names the actions the recover step performed.
	Recovery []string
}

// Failed reports whether the step failed.
func (r StepReport) Failed() bool { return r.Err != nil }

// Report is what an experiment concluded. It is returned whether the experiment
// finished or failed, because a failed experiment is exactly the case a reader
// needs the report for.
type Report struct {
	// Name and ExperimentID identify the experiment.
	Name         string
	ExperimentID string
	// Seed is the seed the experiment ran with, when it declared one.
	Seed *uint64
	// Steps is the lifecycle as it was walked, in order.
	Steps []StepReport
	// Baseline, Observed and Verified are the measurements of the three points
	// in the lifecycle that measure something.
	Baseline []probe.Measurement
	Observed []probe.Measurement
	Verified []probe.Measurement
	// Faults are the records the fault engine made.
	Faults []chaos.Fault
	// Diagnosis is what the evidence supported. It is a report, not a decision:
	// the recover step below is not derived from it.
	Diagnosis investigator.Report
	// Recommendation is what the diagnosis suggested. It is a suggestion, and
	// it is empty unless the evidence named a repair, because the only way to
	// act on it is for an operator to have authorized the action in advance.
	Recommendation recovery.Recommendation
	// Recoveries are the outcomes of the authorized actions.
	Recoveries []recovery.Outcome
	// Failed is the state the experiment failed in, and is empty when it
	// finished.
	Failed State
	// Err is why it failed.
	Err error
	// CleanedUp reports whether the laboratory was taken apart.
	CleanedUp bool
	// Duration is the whole run.
	Duration time.Duration
}

// Succeeded reports whether the experiment ran to the end without failing.
func (r Report) Succeeded() bool { return r.Err == nil && r.Failed == "" }

// Controller runs a plan.
type Controller struct {
	plan  Plan
	env   Environment
	clock func() time.Time

	mu    chan struct{}
	state State
}

// New builds a controller for a plan.
//
// The state starts at the first state rather than at a state of its own: there
// is nothing before setup to report, and a controller that had a "created"
// state would be a state the lifecycle does not have.
//
// The clock is real: step durations in a report are wall clock times, which is
// what a report is kept for. The time argument is the wall time the experiment
// is taken to have started, so a caller can pin the report's timestamps to a
// reference without freezing the durations at it.
func New(plan Plan, env Environment, now time.Time) (*Controller, error) {
	const op = "experiment.New"
	if env == nil {
		return nil, apperr.InvalidArgument(op, "an experiment needs an environment to act on", nil)
	}
	if len(plan.Steps) == 0 {
		return nil, apperr.Validation(op, "the plan has no steps, so it was not compiled", nil)
	}
	// A plan is a sequence of states, and the controller checks it before it
	// trusts it. Compile builds it correctly; this is what refuses a plan that
	// was assembled by something else, since a plan is a value.
	states := States()
	if len(plan.Steps) != len(states) {
		return nil, apperr.Validation(op, "the plan has "+strconv.Itoa(len(plan.Steps))+
			" steps, the lifecycle has "+strconv.Itoa(len(states)), nil)
	}
	for i, step := range plan.Steps {
		if step.State != states[i] {
			return nil, apperr.Validation(op, "step "+strconv.Itoa(i)+" is "+string(step.State)+
				", want "+string(states[i])+": a plan follows the lifecycle in order", nil)
		}
	}
	anchor := time.Now()
	return &Controller{
		plan:  plan,
		env:   env,
		clock: func() time.Time { return now.Add(time.Since(anchor)) },
		state: plan.Steps[0].State,
		mu:    make(chan struct{}, 1),
	}, nil
}

// Plan returns the plan the controller runs.
func (c *Controller) Plan() Plan { return c.plan }

// State returns the state the controller is in.
func (c *Controller) State() State {
	c.mu <- struct{}{}
	defer func() { <-c.mu }()
	return c.state
}

func (c *Controller) moveTo(state State) {
	c.mu <- struct{}{}
	c.state = state
	<-c.mu
}

// Run walks the lifecycle and returns what happened.
//
// It stops at the first failed state, because a laboratory in an unknown state
// is not one a later step can reason about, and then runs the cleanup step,
// because a failed experiment must not leave namespaces, faults or processes
// behind. Both the report and the error are returned: the error says what went
// wrong and the report says how far it got.
func (c *Controller) Run(ctx context.Context) (Report, error) {
	const op = "experiment.Run"
	report := Report{
		Name:         c.plan.Name,
		ExperimentID: c.plan.ExperimentID,
		Seed:         c.plan.Seed,
	}
	started := c.clock()
	var failure error

	for i, step := range c.plan.Steps {
		// The transition is checked even though the controller produced the
		// plan itself, because a step that ran in the wrong order would make
		// the report lie about the lifecycle it followed.
		if i > 0 {
			previous := c.plan.Steps[i-1].State
			if err := Transition(previous, step.State); err != nil {
				failure = apperr.Wrap(err, kindOf(err), op, "the plan is not a valid lifecycle")
				report.Failed = previous
				report.Err = failure
				break
			}
		}
		c.moveTo(step.State)
		record := StepReport{State: step.State, Detail: step.Detail, Started: c.clock()}

		// The context is checked before the step runs, because an environment
		// is entitled to ignore a context it is handed and an experiment that
		// kept going after it was cancelled would keep changing a laboratory
		// nobody is waiting for.
		var err error
		if ctxErr := apperr.FromContext(ctx, op); ctxErr != nil {
			err = ctxErr
		} else {
			switch step.State {
			case StateSetup:
				err = c.env.Setup(ctx)
			case StateBaseline:
				report.Baseline, err = c.env.Baseline(ctx)
			case StateFault:
				report.Faults, err = c.env.ApplyFaults(ctx)
			case StateObserve:
				report.Observed, err = c.env.Observe(ctx)
			case StateDiagnose:
				if err = c.requireObserved(report); err == nil {
					report.Diagnosis, err = c.env.Diagnose(ctx, report.Observed)
				}
			case StateRecover:
				report.Recoveries, record.Recovery, err = c.recover(ctx)
				if err == nil {
					report.Recommendation = recovery.Propose(report.Diagnosis)
				}
			case StateVerify:
				report.Verified, err = c.verify(ctx)
			case StateReport:
				// The report is this value; the state is where it stops changing.
			case StateCleanup:
				err = c.env.Teardown(ctx)
				report.CleanedUp = err == nil
			}
		}
		record.Duration = c.clock().Sub(record.Started)
		record.Err = err
		report.Steps = append(report.Steps, record)
		if err != nil {
			failure = err
			report.Failed = step.State
			report.Err = err
			break
		}
	}

	// A failure before the cleanup state still has to be cleaned up. This is
	// the branch that keeps a refused or broken experiment from leaving a
	// laboratory behind, and it runs even when the failure was the context
	// being cancelled: a torn-down laboratory is the last thing to go.
	if !report.CleanedUp {
		cleanup := StepReport{State: StateCleanup, Detail: "take the laboratory apart", Started: c.clock()}
		// The teardown gets a context of its own, bounded by the cleanup
		// deadline, so an experiment that was cancelled still takes its
		// namespaces down. Tearing down with a cancelled context is how a
		// laboratory is left behind.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.plan.Steps[len(c.plan.Steps)-1].Deadline)
		err := c.env.Teardown(cleanupCtx)
		cancel()
		cleanup.Duration = c.clock().Sub(cleanup.Started)
		cleanup.Err = err
		report.Steps = append(report.Steps, cleanup)
		report.CleanedUp = err == nil
		if err != nil && failure == nil {
			failure = err
			report.Failed = StateCleanup
			report.Err = err
		}
	}

	c.moveTo(StateCleanup)
	report.Duration = c.clock().Sub(started)
	return report, failure
}

// requireObserved refuses to diagnose nothing. A diagnosis with no
// measurements would be a conclusion with no evidence, and the investigator is
// built to refuse one, so the controller refuses it here rather than handing it
// an empty observation and reporting whatever comes back.
// verify measures the path after the recover step and judges the measurement.
//
// The judging is the state. An environment's Verify is asked for a measurement,
// and a measurement on its own is not a verdict: a path can be measured every
// step of an experiment and still be broken, and an experiment that reported
// success in that case would be worse than one that reported nothing. The
// criteria are the ones the plan was compiled with, so a plan and its run cannot
// disagree about what counts as a working path.
func (c *Controller) verify(ctx context.Context) ([]probe.Measurement, error) {
	const op = "experiment.Verify"
	measurements, err := c.env.Verify(ctx)
	if err != nil {
		return measurements, err
	}
	if len(measurements) == 0 {
		return measurements, apperr.Internal(op, "the verification measured nothing, "+
			"so there is no answer to the question the step asked", nil)
	}
	// Every measurement the environment took has to agree. One healthy reading
	// alongside a broken one is a broken path, and picking the better of the two
	// would make the verdict depend on the order they arrived in.
	for i, measurement := range measurements {
		passed, reason := c.plan.Criteria.Judge(measurement)
		if !passed {
			return measurements, apperr.Validation(op, "the path did not recover: "+reason+
				" (measurement "+strconv.Itoa(i+1)+" of "+strconv.Itoa(len(measurements))+
				", "+measurement.Namespace+" to "+measurement.Target+")", nil)
		}
	}
	return measurements, nil
}

func (c *Controller) requireObserved(report Report) error {
	const op = "experiment.Run"
	if len(report.Observed) == 0 {
		return apperr.Internal(op, "there is no observation to diagnose", nil)
	}
	return nil
}

// recover performs the authorized actions.
//
// It passes the plan's authorizations and requires the environment to have
// done exactly those. An environment that recovered something else, or nothing
// when something was authorized, fails the experiment: a report that says a
// backend was removed when it was not would be worse than a failed run.
func (c *Controller) recover(ctx context.Context) ([]recovery.Outcome, []string, error) {
	const op = "experiment.Run"
	if len(c.plan.Recoveries) == 0 {
		// Nothing was authorized, so nothing is done. The step is still visited
		// and still reported, which is the point: the lifecycle says recovery
		// happened and the report says no action was authorized for it.
		return nil, nil, nil
	}
	outcomes, err := c.env.Recover(ctx, c.plan.Recoveries)
	names := make([]string, 0, len(outcomes))
	for _, outcome := range outcomes {
		names = append(names, outcome.Authorization.String())
	}
	if err != nil {
		return outcomes, names, err
	}
	if len(outcomes) != len(c.plan.Recoveries) {
		return outcomes, names, apperr.Internal(op, "the recover step performed "+
			strconv.Itoa(len(outcomes))+" of the "+strconv.Itoa(len(c.plan.Recoveries))+" authorized actions", nil)
	}
	for i, outcome := range outcomes {
		if outcome.Authorization.Action() != c.plan.Recoveries[i].Action() ||
			outcome.Authorization.Target() != c.plan.Recoveries[i].Target() {
			return outcomes, names, apperr.Internal(op, "the recover step performed "+
				outcome.Authorization.String()+" where "+c.plan.Recoveries[i].String()+" was authorized", nil)
		}
		if err := outcome.Err; err != nil {
			return outcomes, names, apperr.Wrap(err, kindOf(err), op, "the authorized action did not succeed")
		}
		if !outcome.Verification.Passed {
			return outcomes, names, apperr.Validation(op, "the authorized action ran but the path did not recover: "+
				outcome.Verification.Reason, nil)
		}
	}
	return outcomes, names, nil
}
