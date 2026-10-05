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

const probeMode = "netforge-probe"

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

type Echor interface {
	Measure(ctx context.Context, namespace, target string, options Options) (Measurement, error)
}

func NewEngine(limits Limits, scope Scope, run Runner, echo Echor, self string) (*Engine, error) {
	const op = "probe.NewEngine"
	if run == nil {
		return nil, apperr.Internal(op, "a probe engine needs a command runner", nil)
	}
	if self == "" {

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

func (e *Engine) Limits() Limits { return e.limits }

func (e *Engine) Scope() Scope { return e.scope }

func (e *Engine) Store() *Store { return e.store }

func (e *Engine) Results() []Result { return e.store.Results() }

func (e *Engine) Run(ctx context.Context, request Request) (Result, error) {
	const op = "probe.Engine.Run"

	checked, err := request.check(op, e.limits, e.scope)
	if err != nil {

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

type noReplyError struct{ sent int }

func (e *noReplyError) Error() string {
	return fmt.Sprintf("no reply to any of the %d echo requests", e.sent)
}

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

func bounded(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	return text[:limit] + "..."
}
