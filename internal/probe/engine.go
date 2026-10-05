package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/netforge/netforge/internal/apperr"
)

// probeMode is the argument the probe agent is started with. The agent runs
// inside a laboratory namespace, which is the only place a probe may open a
// socket, and prints one JSON result.
const probeMode = "netforge-probe"

// Engine runs protocol probes inside laboratory namespaces. It is the only
// part of NetForge that originates application traffic, so it is where the
// scope, timeout, concurrency, response size and storage bounds are enforced.
type Engine struct {
	limits Limits
	scope  Scope
	run    Runner
	echo   Echor
	self   string
	store  *Store
	slots  chan struct{}

	mu      sync.Mutex
	agentOK bool
}

// Echor measures reachability with the kernel's own echo tool. An echo
// request is a kernel operation rather than a socket a laboratory agent could
// open, so ICMP probes go through this instead of through the agent.
type Echor interface {
	Measure(ctx context.Context, namespace, target string, options Options) (Measurement, error)
}

// NewEngine builds a probe engine. The returned engine is safe for concurrent
// use and admits at most MaxConcurrent probes at a time. Echo is how ICMP is
// measured, so an engine without one refuses an echo probe rather than
// reporting a measurement it did not make.
func NewEngine(limits Limits, scope Scope, run Runner, echo Echor, self string) (*Engine, error) {
	const op = "probe.NewEngine"
	if run == nil {
		return nil, apperr.Internal(op, "a probe engine needs a command runner", nil)
	}
	if self == "" {
		// A namespace runs with a small PATH, so the agent has to be named by
		// its own path rather than by a mode name nothing can resolve.
		path, err := os.Executable()
		if err != nil {
			return nil, apperr.Internal(op, "find this executable to run the probe agent: "+err.Error(), err)
		}
		self = path
	}
	if !scope.Network.IsValid() || len(scope.Namespaces) == 0 {
		return nil, apperr.InvalidArgument(op, "a probe engine needs a laboratory scope", nil)
	}
	checked, err := limits.check()
	if err != nil {
		return nil, err
	}
	store, err := NewStore(checked.MaxStoredResults)
	if err != nil {
		return nil, err
	}
	return &Engine{
		limits: checked,
		scope:  scope,
		run:    run,
		echo:   echo,
		self:   self,
		store:  store,
		slots:  make(chan struct{}, checked.MaxConcurrent),
	}, nil
}

// Limits returns the limits the engine enforces.
func (e *Engine) Limits() Limits { return e.limits }

// Scope returns the scope the engine refuses to leave.
func (e *Engine) Scope() Scope { return e.scope }

// Store returns the bounded result store.
func (e *Engine) Store() *Store { return e.store }

// Results returns the stored results, oldest first.
func (e *Engine) Results() []Result { return e.store.Results() }

// Run probes one target and records the result. A probe that fails still
// returns a result: the failure is the measurement, and an investigator needs
// to see it rather than infer it from a missing entry.
func (e *Engine) Run(ctx context.Context, request Request) (Result, error) {
	const op = "probe.Engine.Run"

	checked, err := request.check(op, e.limits, e.scope)
	if err != nil {
		// A refusal is itself a recorded result, because "the probe was
		// refused" and "the probe found nothing" are different answers.
		refused := e.refusal(checked, err)
		e.store.Add(refused)
		return refused, err
	}
	if err := e.admit(op); err != nil {
		refused := e.refusal(checked, err)
		e.store.Add(refused)
		return refused, err
	}
	defer e.release()

	if err := apperr.FromContext(ctx, op); err != nil {
		refused := e.refusal(checked, err)
		e.store.Add(refused)
		return refused, err
	}

	runCtx, cancel := context.WithTimeout(ctx, checked.Timeout)
	defer cancel()

	started := time.Now()
	var result Result
	var runErr error
	if checked.Protocol == ProtocolICMP {
		result, runErr = e.echoProbe(runCtx, checked)
	} else {
		result, runErr = e.dispatch(runCtx, checked, started)
	}
	result.Duration = time.Since(started)
	if result.At.IsZero() {
		result.At = time.Now()
	}
	result.Experiment = checked.Experiment
	result.Namespace = checked.Namespace
	result.Target = checked.Target
	result.Protocol = checked.Protocol

	if runErr != nil && !result.Succeeded() {
		// The agent reports its own failures in the result. An error here
		// means the agent could not run at all, which is a laboratory problem
		// rather than a measurement.
		result.OK = false
		if result.Failure == FailureNone {
			result.Failure = Classify(runErr)
		}
		if result.Error == "" {
			result.Error = runErr.Error()
		}
	}
	e.store.Add(result)
	return result, runErr
}

// echoProbe measures an ICMP probe with the kernel's echo tool and turns the
// measurement into a result.
func (e *Engine) echoProbe(ctx context.Context, request Request) (Result, error) {
	const op = "probe.Engine.echoProbe"
	result := Result{
		Experiment: request.Experiment,
		Namespace:  request.Namespace,
		Target:     request.Target,
		Protocol:   ProtocolICMP,
		At:         time.Now(),
	}
	if e.echo == nil {
		return result, apperr.Internal(op, "this laboratory has no way to measure an echo probe", nil)
	}
	count, size := 1, request.Payload
	if count <= 0 {
		count = 1
	}
	if size <= 0 {
		size = 32
	}
	measurement, err := e.echo.Measure(ctx, request.Namespace, request.Target, Options{
		Count:    count,
		Size:     size,
		Timeout:  request.Timeout,
		Deadline: request.Timeout,
	})
	if err != nil {
		return result, err
	}
	result.RTT = measurement.AverageRTT
	result.Sent = measurement.Sent
	result.Received = measurement.Received
	result.Loss = measurement.Loss
	result.ResponseSize = measurement.Received
	result.Detail = fmt.Sprintf("%d of %d replies, %.1f%% loss",
		measurement.Received, measurement.Sent, measurement.Loss)
	if measurement.Received == 0 {
		silent := &noReplyError{sent: measurement.Sent}
		result.Failure = Classify(silent)
		result.Error = silent.Error()
		return result, nil
	}
	result.OK = true
	return result, nil
}

// noReplyError reports that an echo measurement completed but nothing came
// back, which is a different answer from "the measurement could not run".
type noReplyError struct{ sent int }

func (e *noReplyError) Error() string {
	return fmt.Sprintf("no reply to any of the %d echo requests", e.sent)
}

// dispatch runs the probe agent inside the target namespace and reads its
// answer back.
func (e *Engine) dispatch(ctx context.Context, request Request, started time.Time) (Result, error) {
	const op = "probe.Engine.dispatch"
	payload := AgentPayload{
		Request:         request,
		MaxResponseSize: e.limits.MaxResponseSize,
		MaxPayload:      e.limits.MaxSize,
	}
	settings, err := json.Marshal(payload)
	if err != nil {
		return Result{}, apperr.Internal(op, "encode the probe request: "+err.Error(), err)
	}
	stdout, stderr, code, err := e.run.Run(request.Namespace, e.self, probeMode, string(settings))
	if err != nil {
		return Result{}, apperr.Wrap(err, apperr.KindInternal, op, fmt.Sprintf(
			"the probe agent could not be run in %s", request.Namespace))
	}
	if elapsed := time.Since(started); elapsed > request.Timeout+2*time.Second {
		return Result{}, apperr.LimitExceeded(op, fmt.Sprintf(
			"the probe agent took %s, past the %s budget for the request", elapsed, request.Timeout), nil)
	}
	if code != 0 && strings.TrimSpace(stdout) == "" {
		return Result{}, apperr.IO(op, fmt.Sprintf(
			"the probe agent in %s failed with exit status %d: %s",
			request.Namespace, code, strings.TrimSpace(stderr)),
			fmt.Errorf("%s", strings.TrimSpace(stderr)))
	}
	var result Result
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		return Result{}, apperr.Validation(op, fmt.Sprintf(
			"the probe agent in %s printed an answer that could not be read: %s",
			request.Namespace, bounded(err.Error(), 200)), err)
	}
	if result.Protocol != request.Protocol {
		return Result{}, apperr.Validation(op, fmt.Sprintf(
			"the probe agent answered for %s when %s was asked", result.Protocol, request.Protocol), nil)
	}
	return result, nil
}

// refusal builds the result that records a probe which never left the process.
func (e *Engine) refusal(request Request, err error) Result {
	return Result{
		Experiment: request.Experiment,
		Namespace:  request.Namespace,
		Target:     request.Target,
		Protocol:   request.Protocol,
		OK:         false,
		Error:      bounded(err.Error(), 400),
		Failure:    Classify(err),
		At:         time.Now(),
	}
}

// admit takes a concurrency slot without waiting, so a probe that cannot run
// is told so rather than queued behind others without bound.
func (e *Engine) admit(op string) error {
	select {
	case e.slots <- struct{}{}:
		return nil
	default:
		return apperr.LimitExceeded(op, fmt.Sprintf(
			"%d probes are already running, the limit is %d", len(e.slots), e.limits.MaxConcurrent), nil)
	}
}

func (e *Engine) release() { <-e.slots }

// bounded shortens a string so a peer cannot make an error message, and
// therefore the process's memory, grow with the answer it sent.
func bounded(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	return text[:limit] + "..."
}
