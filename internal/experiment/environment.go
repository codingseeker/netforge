package experiment

import (
	"context"
	"strconv"
	"time"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/chaos"
	"github.com/netforge/netforge/internal/investigator"
	"github.com/netforge/netforge/internal/network"
	"github.com/netforge/netforge/internal/probe"
	"github.com/netforge/netforge/internal/recovery"
)

type LabEnvironment struct {
	plan  Plan
	spec  network.Spec
	build func(ctx context.Context, spec network.Spec) (*network.Lab, error)

	lab *network.Lab

	summary *network.Summary

	agent recovery.Executor

	prober recovery.Prober

	baseline []probe.Measurement

	measured int
}

const routerReadWindow = 5 * time.Second

func NewLabEnvironment(plan Plan, spec network.Spec, build func(context.Context, network.Spec) (*network.Lab, error)) (*LabEnvironment, error) {
	const op = "experiment.NewLabEnvironment"
	if build == nil {
		return nil, apperr.InvalidArgument(op, "an environment needs a builder for its laboratory", nil)
	}
	if spec.Network == "" {

		spec.Network = plan.Document.Scope.Network
	}
	return &LabEnvironment{plan: plan, spec: spec, build: build}, nil
}

func (e *LabEnvironment) WithRecovery(agent recovery.Executor, prober recovery.Prober) *LabEnvironment {
	e.agent = agent
	e.prober = prober
	return e
}

func (e *LabEnvironment) Lab() *network.Lab { return e.lab }

func (e *LabEnvironment) Summary() *network.Summary { return e.summary }

func (e *LabEnvironment) Setup(ctx context.Context) error {
	const op = "experiment.Setup"
	if err := apperr.FromContext(ctx, op); err != nil {
		return err
	}
	spec := e.spec

	if spec.ID == "" {
		spec.ID = e.plan.ExperimentID
	}
	spec.Faults = nil
	lab, err := e.build(ctx, spec)
	if err != nil {
		return apperr.Wrap(err, kindOf(err), op, "the laboratory could not be created")
	}
	e.lab = lab

	summary := lab.Summary()
	e.summary = &summary
	if e.agent == nil {

		agent, err := recovery.NewLab(lab, nil, e.plan.Observation.Settle)
		if err != nil {
			return apperr.Wrap(err, kindOf(err), op, "the recovery agent could not be created")
		}
		e.agent = agent
		e.prober = agent
	}
	return nil
}

func (e *LabEnvironment) Baseline(ctx context.Context) ([]probe.Measurement, error) {
	return e.measure(ctx, "baseline")
}

func (e *LabEnvironment) Observe(ctx context.Context) ([]probe.Measurement, error) {
	if err := e.wait(ctx, e.plan.Observation.AfterFault); err != nil {
		return nil, apperr.Wrap(err, kindOf(err), "experiment.Observe", "the fault was not given time to take effect")
	}
	return e.measure(ctx, "observe")
}

func (e *LabEnvironment) Verify(ctx context.Context) ([]probe.Measurement, error) {
	if err := e.wait(ctx, e.plan.Observation.Settle); err != nil {
		return nil, apperr.Wrap(err, kindOf(err), "experiment.Verify", "the recovery was not given time to take effect")
	}
	return e.measure(ctx, "verify")
}

func (e *LabEnvironment) measure(ctx context.Context, phase string) ([]probe.Measurement, error) {
	const op = "experiment.measure"
	if e.lab == nil {
		return nil, apperr.Internal(op, "the laboratory has not been created", nil)
	}
	observation := e.plan.Observation
	measurement, err := e.lab.Measure(ctx, observation.Namespace, observation.Target, observation.Options)
	if err != nil {
		return nil, apperr.Wrap(err, kindOf(err), op, "the "+phase+" measurement failed")
	}
	e.measured++
	if phase == "baseline" {
		e.baseline = []probe.Measurement{measurement}
	}
	return []probe.Measurement{measurement}, nil
}

func (e *LabEnvironment) ApplyFaults(ctx context.Context) ([]chaos.Fault, error) {
	const op = "experiment.ApplyFaults"
	if e.lab == nil {
		return nil, apperr.Internal(op, "the laboratory has not been created", nil)
	}
	engine := e.lab.Chaos()
	if engine == nil {
		return nil, apperr.Internal(op, "the laboratory has no fault engine", nil)
	}
	if len(e.plan.Faults) == 0 {
		return nil, nil
	}
	faults, err := engine.ApplyAll(ctx, e.plan.ExperimentID, e.plan.Faults)
	if err != nil {
		return nil, apperr.Wrap(err, kindOf(err), op, "the declared faults could not be injected")
	}
	return faults, nil
}

func (e *LabEnvironment) Diagnose(ctx context.Context, observed []probe.Measurement) (investigator.Report, error) {
	const op = "experiment.Diagnose"
	if e.lab == nil {
		return investigator.Report{}, apperr.Internal(op, "the laboratory has not been created", nil)
	}
	if len(observed) == 0 {
		return investigator.Report{}, apperr.Internal(op, "there is nothing to diagnose", nil)
	}
	failure := observed[0]
	if failure.Loss == 0 && failure.Received == failure.Sent && failure.Sent > 0 {

		return investigator.Report{}, apperr.Validation(op,
			"the path answered all "+strconv.Itoa(failure.Sent)+" probes, so there is no failure to diagnose", nil)
	}
	investigatorEngine, err := e.lab.Investigator(investigator.DefaultLimits())
	if err != nil {
		return investigator.Report{}, apperr.Wrap(err, kindOf(err), op, "the investigator could not be created")
	}
	observation := investigator.Observation{
		Experiment: e.plan.ExperimentID,
		Namespace:  e.plan.Observation.Namespace,
		Target:     e.plan.Observation.Target,
		Port:       e.plan.Observation.Port,
		Failed:     e.plan.Observation.Protocol,
		Failure:    failureOf(failure),
		Error:      "the path lost " + strconv.FormatFloat(failure.Loss, 'g', -1, 64) + "% of its probes",
		At:         time.Now(),
	}
	report, err := investigatorEngine.Investigate(ctx, observation)
	if err != nil {
		return investigator.Report{}, apperr.Wrap(err, kindOf(err), op, "the failure could not be investigated")
	}
	return report, nil
}

func (e *LabEnvironment) Recover(ctx context.Context, authorizations []recovery.Authorization) ([]recovery.Outcome, error) {
	const op = "experiment.Recover"
	if len(authorizations) == 0 {
		return nil, nil
	}
	if e.agent == nil || e.prober == nil {
		return nil, apperr.Internal(op, "the experiment has no recovery agent", nil)
	}

	rebuilt, err := recovery.New(e.plan.Scope, requestsOf(authorizations, e.plan.Observation),
		e.plan.Criteria, recovery.Limits{
			MaxActions:      recovery.DefaultLimits().MaxActions,
			MaxReasonLength: recovery.DefaultLimits().MaxReasonLength,
			MaxDeadline:     e.plan.Observation.Options.Deadline,
		}, time.Now())
	if err != nil {
		return nil, apperr.Wrap(err, kindOf(err), op, "the authorized actions could not be rebuilt")
	}
	return rebuilt.Execute(ctx, e.agent, e.prober)
}

func requestsOf(authorizations []recovery.Authorization, observation Observation) []recovery.Request {
	out := make([]recovery.Request, 0, len(authorizations))
	for _, authorization := range authorizations {
		out = append(out, recovery.Request{
			Action:  authorization.Action(),
			Target:  authorization.Target(),
			From:    authorization.From(),
			Reason:  authorization.Reason(),
			Consent: true,
			Check: &recovery.Check{
				Namespace: observation.Namespace,
				Target:    observation.Target,
				Options:   observation.Options,
			},
		})
	}
	return out
}

func (e *LabEnvironment) Teardown(ctx context.Context) error {
	const op = "experiment.Teardown"
	if e.lab == nil {

		return nil
	}
	e.recordRouter()
	err := e.lab.Destroy(ctx)
	e.lab = nil
	e.agent = nil
	e.prober = nil
	if err != nil {
		return apperr.Wrap(err, kindOf(err), op, "the laboratory could not be taken apart")
	}
	return nil
}

func (e *LabEnvironment) recordRouter() {
	if e.lab == nil || e.summary == nil || e.summary.Routing != network.RoutingSoftware {
		return
	}
	if !e.lab.RouterAlive() {
		e.summary.RecordRouterFailure("the software router was no longer running")
		return
	}
	stats, err := e.lab.RouterStatsWithin(routerReadWindow)
	if err != nil {
		e.summary.RecordRouterFailure(reasonOf(err))
		return
	}
	e.summary.RecordRouter(stats)
}

func reasonOf(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (e *LabEnvironment) wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return apperr.FromContext(ctx, "experiment.wait")
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return apperr.FromContext(ctx, "experiment.wait")
	case <-timer.C:
		return nil
	}
}

func failureOf(measurement probe.Measurement) probe.Failure {
	switch {
	case measurement.Received == 0:
		return probe.FailureTimeout
	case measurement.Loss > 0:
		return probe.FailureTimeout
	default:
		return probe.FailureNone
	}
}

func (e *LabEnvironment) BaselineOf() []probe.Measurement { return e.baseline }
