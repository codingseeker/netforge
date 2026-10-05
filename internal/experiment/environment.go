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

// LabEnvironment runs an experiment against a real laboratory.
//
// The laboratory is created in Setup rather than handed in, because that is
// what makes the plan meaningful: a plan that was compiled from a document can
// be run against a laboratory built from that same document, and nothing exists
// before the first state runs.
type LabEnvironment struct {
	plan  Plan
	spec  network.Spec
	build func(ctx context.Context, spec network.Spec) (*network.Lab, error)

	// lab is the laboratory once it exists.
	lab *network.Lab
	// summary is what the laboratory was, recorded as a value while it existed.
	// It is kept because the only moment a run can report which namespaces it
	// built, which addresses they held and what was listening is the moment before
	// it takes them apart.
	summary *network.Summary
	// agent performs the authorized recovery actions, and is nil when the
	// experiment has none to perform.
	agent recovery.Executor
	// prober measures the path the observation names.
	prober recovery.Prober

	// baseline is the measurement taken before the faults, kept so a diagnosis
	// can be read against the path's own behaviour rather than against an
	// assumption about it.
	baseline []probe.Measurement
	// measured is the number of measurements taken, so a repeated step cannot
	// quietly measure twice.
	measured int
}

// routerReadWindow is how long a teardown will wait for the software router to
// report its counters. It is short because this is a diagnostic taken while the
// laboratory is being taken apart: a run that spent the length of its teardown
// waiting for a number would be a run that reports late, and a reader who waits
// for a report has already stopped trusting the run.
const routerReadWindow = 5 * time.Second

// NewLabEnvironment binds a plan to a laboratory specification and a builder.
//
// The builder is a function so a caller chooses how the laboratory is created
// and a test can supply one that does not need a kernel, while the controller
// above sees the same lifecycle either way.
func NewLabEnvironment(plan Plan, spec network.Spec, build func(context.Context, network.Spec) (*network.Lab, error)) (*LabEnvironment, error) {
	const op = "experiment.NewLabEnvironment"
	if build == nil {
		return nil, apperr.InvalidArgument(op, "an environment needs a builder for its laboratory", nil)
	}
	if spec.Network == "" {
		// The document is the only place a network is named, so a specification
		// that does not carry it is a specification that disagrees with the plan
		// it was built for.
		spec.Network = plan.Document.Scope.Network
	}
	return &LabEnvironment{plan: plan, spec: spec, build: build}, nil
}

// WithRecovery gives the environment the agent that performs recovery actions
// and the prober that verifies them.
func (e *LabEnvironment) WithRecovery(agent recovery.Executor, prober recovery.Prober) *LabEnvironment {
	e.agent = agent
	e.prober = prober
	return e
}

// Lab returns the laboratory, which is nil until the setup step has run.
func (e *LabEnvironment) Lab() *network.Lab { return e.lab }

// Summary returns what the laboratory was, as recorded while it existed. It is
// nil when the laboratory was never created, which is the answer for a run that
// failed in setup or a run that used an environment other than this one.
func (e *LabEnvironment) Summary() *network.Summary { return e.summary }

// Setup creates the laboratory described by the plan's document.
//
// The declared faults are not applied here. The lab would apply them as part of
// its construction, which is the right behaviour for a laboratory and the wrong
// one for an experiment: the experiment has a fault step of its own, between a
// baseline measurement and the observation, and a fault applied at construction
// would make the baseline a measurement of an already impaired path.
func (e *LabEnvironment) Setup(ctx context.Context) error {
	const op = "experiment.Setup"
	if err := apperr.FromContext(ctx, op); err != nil {
		return err
	}
	spec := e.spec
	// The laboratory is created under the operator's identifier when one was
	// chosen, so a document can name the namespaces it grants before the run
	// exists. Without one, the experiment's own identity is the fallback: a
	// caller that derives a spec from a plan gets the namespaces that plan's
	// document has to grant.
	if spec.ID == "" {
		spec.ID = e.plan.ExperimentID
	}
	spec.Faults = nil
	lab, err := e.build(ctx, spec)
	if err != nil {
		return apperr.Wrap(err, kindOf(err), op, "the laboratory could not be created")
	}
	e.lab = lab
	// The summary is taken here, while every namespace and listener still
	// exists. Nothing later in the lifecycle can answer "which namespaces did
	// this run build", because by then there are none.
	summary := lab.Summary()
	e.summary = &summary
	if e.agent == nil {
		// The agent is built here rather than in the constructor because it
		// needs the laboratory, and it is built even when nothing was
		// authorized: a laboratory with no actions to perform is the ordinary
		// case and should not have to be configured differently.
		agent, err := recovery.NewLab(lab, nil, e.plan.Observation.Settle)
		if err != nil {
			return apperr.Wrap(err, kindOf(err), op, "the recovery agent could not be created")
		}
		e.agent = agent
		e.prober = agent
	}
	return nil
}

// Baseline measures the path before any fault is applied.
func (e *LabEnvironment) Baseline(ctx context.Context) ([]probe.Measurement, error) {
	return e.measure(ctx, "baseline")
}

// Observe measures the path with the faults in place.
//
// It waits for the fault to reach the kernel first. A fault that was accepted
// a moment ago may not be installed yet, and measuring immediately would
// measure a path that still works and diagnose a laboratory that is fine.
func (e *LabEnvironment) Observe(ctx context.Context) ([]probe.Measurement, error) {
	if err := e.wait(ctx, e.plan.Observation.AfterFault); err != nil {
		return nil, apperr.Wrap(err, kindOf(err), "experiment.Observe", "the fault was not given time to take effect")
	}
	return e.measure(ctx, "observe")
}

// Verify measures the path after the authorized recovery actions ran.
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

// ApplyFaults injects the plan's faults.
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

// Diagnose explains what the observation measured.
//
// It does not decide what to do about it. The report it returns is evidence,
// and the only thing this package does with that evidence is put it in the
// experiment's report and ask the recovery package what it suggests, which is
// a suggestion the run cannot act on.
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
		// Nothing failed. The investigator explains failures, and asking it to
		// explain a path that answered every probe would produce a report about
		// a failure that did not happen.
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

// Recover performs the authorized actions, in order, and verifies each one.
func (e *LabEnvironment) Recover(ctx context.Context, authorizations []recovery.Authorization) ([]recovery.Outcome, error) {
	const op = "experiment.Recover"
	if len(authorizations) == 0 {
		return nil, nil
	}
	if e.agent == nil || e.prober == nil {
		return nil, apperr.Internal(op, "the experiment has no recovery agent", nil)
	}
	// A recovery is rebuilt from the authorizations rather than from the
	// requests, so the actions performed are the ones that were authorized and
	// the scope they are checked against is this experiment's scope.
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

// requestsOf turns authorizations back into requests, so a recovery can be
// rebuilt from them. The consent is already established: the authorization could
// only have been issued by a consented request.
// requestsOf rebuilds the requests from the authorizations, so that the actions
// performed are the ones that were authorized.
//
// Each request is given the experiment's own observation as the measurement the
// action is judged by. The path being repaired is the path the experiment was
// watching, which is not the same thing as the namespace a fault sits on: a fault
// is removed from a namespace, and what has to be measured afterwards is the path
// to the target that namespace was breaking.
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

// Teardown takes the laboratory apart.
func (e *LabEnvironment) Teardown(ctx context.Context) error {
	const op = "experiment.Teardown"
	if e.lab == nil {
		// A laboratory that was never created is not a laboratory to tear down.
		// The cleanup step runs on a failed setup too, and a setup that failed
		// before the laboratory existed must not be reported as a cleanup
		// failure.
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

// recordRouter asks the software router for its counters one last time.
//
// It runs immediately before the teardown, because that is the last moment the
// router is alive and the last moment a reader can attribute a forwarded packet
// to NetForge rather than to the kernel. A reading that fails is recorded as a
// failure rather than left absent: "the counters are zero" and "the counters
// could not be asked for" are different answers, and only one of them is
// evidence.
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

// reasonOf turns an error into the sentence a report carries.
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

// failureOf names what a measurement that lost probes looked like, so the
// investigator starts from the same evidence the experiment measured.
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

// BaselineOf returns the measurement taken before the faults, which a report
// reads against the observation that followed it.
func (e *LabEnvironment) BaselineOf() []probe.Measurement { return e.baseline }
