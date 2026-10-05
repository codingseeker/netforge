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

type Environment interface {
	Setup(ctx context.Context) error

	Baseline(ctx context.Context) ([]probe.Measurement, error)

	ApplyFaults(ctx context.Context) ([]chaos.Fault, error)

	Observe(ctx context.Context) ([]probe.Measurement, error)

	Diagnose(ctx context.Context, observed []probe.Measurement) (investigator.Report, error)

	Recover(ctx context.Context, authorizations []recovery.Authorization) ([]recovery.Outcome, error)

	Verify(ctx context.Context) ([]probe.Measurement, error)

	Teardown(ctx context.Context) error
}

type StepReport struct {
	State State

	Detail string

	Started time.Time

	Duration time.Duration

	Err error

	Recovery []string
}

func (r StepReport) Failed() bool { return r.Err != nil }

type Report struct {
	Name         string
	ExperimentID string

	Seed *uint64

	Steps []StepReport

	Baseline []probe.Measurement
	Observed []probe.Measurement
	Verified []probe.Measurement

	Faults []chaos.Fault

	Diagnosis investigator.Report

	Recommendation recovery.Recommendation

	Recoveries []recovery.Outcome

	Failed State

	Err error

	CleanedUp bool

	Duration time.Duration
}

func (r Report) Succeeded() bool { return r.Err == nil && r.Failed == "" }

type Controller struct {
	plan  Plan
	env   Environment
	clock func() time.Time

	mu    chan struct{}
	state State
}

func New(plan Plan, env Environment, now time.Time) (*Controller, error) {
	const op = "experiment.New"
	if env == nil {
		return nil, apperr.InvalidArgument(op, "an experiment needs an environment to act on", nil)
	}
	if len(plan.Steps) == 0 {
		return nil, apperr.Validation(op, "the plan has no steps, so it was not compiled", nil)
	}

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

func (c *Controller) Plan() Plan { return c.plan }

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

	if !report.CleanedUp {
		cleanup := StepReport{State: StateCleanup, Detail: "take the laboratory apart", Started: c.clock()}

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

func (c *Controller) recover(ctx context.Context) ([]recovery.Outcome, []string, error) {
	const op = "experiment.Run"
	if len(c.plan.Recoveries) == 0 {

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
