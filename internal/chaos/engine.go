package chaos

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/netforge/netforge/internal/apperr"
)

type Runner interface {
	Run(namespace string, argv ...string) (stdout, stderr string, code int, err error)
}

type Topology interface {
	Devices(namespace string) ([]string, error)

	Peer(namespace, device string) (Endpoint, bool)
}

type Engine struct {
	mu     sync.Mutex
	scope  Scope
	limits Limits
	runner Runner
	topo   Topology
	now    func() time.Time

	faults map[string]*Fault

	order []string

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	closed   bool
}

func New(ctx context.Context, scope Scope, limits Limits, run Runner, topo Topology) (*Engine, error) {
	const op = "chaos.New"
	if run == nil {
		return nil, apperr.Internal(op, "the chaos engine needs a command runner", nil)
	}
	if topo == nil {
		return nil, apperr.Internal(op, "the chaos engine needs a topology", nil)
	}
	if err := scope.check(); err != nil {
		return nil, err
	}
	checked, err := limits.check()
	if err != nil {
		return nil, err
	}
	e := &Engine{
		scope:  scope,
		limits: checked,
		runner: run,
		topo:   topo,
		now:    time.Now,
		faults: make(map[string]*Fault),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go e.watch(ctx)
	return e, nil
}

func (e *Engine) Limits() Limits { return e.limits }

func (e *Engine) Scope() Scope { return e.scope }

func (e *Engine) Apply(ctx context.Context, experiment string, spec Spec) (Fault, error) {
	const op = "chaos.Engine.Apply"
	if err := checkExperiment(op, experiment); err != nil {
		return Fault{}, err
	}
	checked, err := spec.check(e.limits)
	if err != nil {
		return Fault{}, err
	}
	if !e.scope.allows(checked.Target) {
		return Fault{}, apperr.Permission(op, fmt.Sprintf(
			"the namespace %q is outside the chaos scope, chaos never runs outside the laboratory",
			checked.Target), nil)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return Fault{}, apperr.Internal(op, "the chaos engine is closed", nil)
	}
	if err := ctx.Err(); err != nil {
		return Fault{}, apperr.Wrap(err, apperr.KindCanceled, op, "the fault was cancelled before it was applied")
	}

	endpoints, err := e.resolve(op, checked)
	if err != nil {
		return Fault{}, err
	}
	if checked.ID == "" {
		checked.ID = deriveID(checked, endpoints)
	}
	if existing, ok := e.faults[checked.ID]; ok {
		return Fault{}, apperr.Conflict(op, fmt.Sprintf(
			"the fault id %q is already in use by a %s fault", checked.ID, existing.Status), nil)
	}
	if _, owner := e.deviceOwner(endpoints.source.Namespace, endpoints.source.Interface); owner != "" {
		return Fault{}, apperr.Conflict(op, fmt.Sprintf(
			"the device %s already carries the fault %q, one device carries at most one fault",
			endpoints.source, owner), nil)
	}
	if err := e.reserve(op); err != nil {
		return Fault{}, err
	}

	fault := Fault{
		ID:          checked.ID,
		Experiment:  experiment,
		Kind:        checked.Kind,
		Status:      StatusPending,
		Target:      Endpoint{Namespace: checked.Target, Interface: endpoints.target.Interface},
		Source:      endpoints.source,
		Destination: endpoints.destination,
		Direction:   checked.Direction,
		Parameters:  checked.Parameters,
		Duration:    checked.Duration,
	}
	if err := e.install(ctx, &fault); err != nil {
		fault.Status = StatusFailed
		fault.Verified = false
		fault.Reason = err.Error()

		_ = e.forget(&fault)
		return Fault{}, err
	}
	e.remember(&fault)
	return fault, nil
}

func (e *Engine) ApplyAll(ctx context.Context, experiment string, specs []Spec) ([]Fault, error) {
	const op = "chaos.Engine.ApplyAll"
	applied := make([]Fault, 0, len(specs))
	for _, spec := range specs {
		fault, err := e.Apply(ctx, experiment, spec)
		if err == nil {
			applied = append(applied, fault)
			continue
		}
		failures := e.rollback(applied)
		message := fmt.Sprintf("the fault set was rejected after %d of %d faults, which were removed again",
			len(applied), len(specs))
		cause := error(err)
		if len(failures) > 0 {
			message = fmt.Sprintf("%s, and %d of them could not be removed", message, len(failures))
			cause = errors.Join(append([]error{err}, failures...)...)
		}
		return nil, apperr.Wrap(cause, kindOf(err), op, message)
	}
	return applied, nil
}

func (e *Engine) rollback(faults []Fault) []error {
	var failures []error
	for i := len(faults) - 1; i >= 0; i-- {
		if _, err := e.Remove(faults[i].ID); err != nil {
			failures = append(failures, err)
		}
	}
	return failures
}

func (e *Engine) Remove(id string) (Fault, error) {
	const op = "chaos.Engine.Remove"
	if id == "" {
		return Fault{}, apperr.InvalidArgument(op, "a fault id must not be empty", nil)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return Fault{}, apperr.Internal(op, "the chaos engine is closed", nil)
	}
	fault, ok := e.faults[id]
	if !ok {
		return Fault{}, apperr.NotFound(op, fmt.Sprintf("there is no fault %q", id), nil)
	}
	if fault.Status.Finished() {
		return *fault, nil
	}
	if err := e.detach(op, fault); err != nil {
		fault.Status = StatusFailed
		fault.Verified = false
		fault.Reason = err.Error()
		return *fault, err
	}
	fault.Status = StatusRemoved
	fault.Observed = Observed{}
	return *fault, nil
}

func (e *Engine) RemoveAll() error {
	const op = "chaos.Engine.RemoveAll"
	e.mu.Lock()
	defer e.mu.Unlock()
	var failures []error
	for _, id := range e.order {
		fault, ok := e.faults[id]
		if !ok || fault.Status.Finished() {
			continue
		}
		if err := e.detach(op, fault); err != nil {
			fault.Status = StatusFailed
			fault.Reason = err.Error()
			failures = append(failures, err)
			continue
		}
		fault.Status = StatusRemoved
		fault.Observed = Observed{}
	}
	return errors.Join(failures...)
}

func (e *Engine) Expire(now time.Time) int {
	const op = "chaos.Engine.Expire"
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return 0
	}
	removed := 0
	for _, id := range e.order {
		fault, ok := e.faults[id]
		if !ok || fault.Status.Finished() {
			continue
		}
		if fault.Expires.After(now) {
			continue
		}
		if err := e.detach(op, fault); err != nil {
			fault.Status = StatusFailed
			fault.Reason = err.Error()
			continue
		}
		fault.Status = StatusExpired
		fault.Observed = Observed{}
		removed++
	}
	return removed
}

func (e *Engine) Get(id string) (Fault, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	fault, ok := e.faults[id]
	if !ok {
		return Fault{}, false
	}
	return *fault, true
}

func (e *Engine) List() []Fault {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Fault, 0, len(e.order))
	for _, id := range e.order {
		if fault, ok := e.faults[id]; ok {
			out = append(out, *fault)
		}
	}
	return out
}

func (e *Engine) Active() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	active := 0
	for _, fault := range e.faults {
		if fault.Status == StatusActive {
			active++
		}
	}
	return active
}

func (e *Engine) Close() error {

	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()

	e.stopOnce.Do(func() { close(e.stop) })
	<-e.done
	return e.RemoveAll()
}

func (e *Engine) install(ctx context.Context, fault *Fault) error {
	const op = "chaos.install"
	device := fault.Source.Interface
	built, err := buildPlan(device, fault.Parameters)
	if err != nil {
		return err
	}

	if err := e.clear(op, fault.Source.Namespace, device); err != nil {
		return err
	}
	if _, _, _, err := e.runIn(fault.Source.Namespace, built.root); err != nil {
		e.uninstall(fault)
		return err
	}
	commands := [][]string{built.clear, built.root}

	states, err := e.states(fault.Source.Namespace, device)
	if err != nil {
		e.uninstall(fault)
		return err
	}
	seen, err := observe(states, device, fault.Parameters)
	if err != nil {
		e.uninstall(fault)
		return err
	}
	if err := verify(seen, fault.Parameters); err != nil {
		e.uninstall(fault)
		return err
	}

	fault.Start = e.now()
	fault.Expires = fault.Start.Add(fault.Duration.Std())
	fault.Observed = seen
	fault.Verified = true
	fault.Status = StatusActive
	fault.Commands = commands
	return nil
}

func (e *Engine) uninstall(fault *Fault) {
	_, _, _, _ = e.runIn(fault.Source.Namespace, delArgs(fault.Source.Interface))
}

func (e *Engine) detach(op string, fault *Fault) error {
	return e.clear(op, fault.Source.Namespace, fault.Source.Interface)
}

func (e *Engine) clear(op, namespace, device string) error {
	_, _, _, _ = e.runIn(namespace, delArgs(device))
	states, err := e.states(namespace, device)
	if err != nil {
		return err
	}
	if installed(states) {
		return apperr.IO(op, fmt.Sprintf(
			"the device %s still carries a fault discipline", device), nil)
	}
	return nil
}

func (e *Engine) states(namespace, device string) ([]qdiscState, error) {
	const op = "chaos.Engine.states"
	stdout, stderr, code, err := e.run(namespace, showArgs(device)...)
	if err != nil {
		return nil, apperr.Wrap(err, kindOf(err), op, fmt.Sprintf(
			"the state of %s could not be read", device))
	}
	if code != 0 {
		return nil, apperr.IO(op, fmt.Sprintf("the state of %s could not be read", device),
			commandFailure(stderr, code))
	}
	return parseState(stdout)
}

func (e *Engine) runIn(namespace string, argv []string) (string, string, int, error) {
	stdout, stderr, code, err := e.run(namespace, argv...)
	if err != nil {
		return stdout, stderr, code, err
	}
	if code != 0 {
		return stdout, stderr, code, apperr.IO("chaos.run", fmt.Sprintf(
			"%s failed in %s", strings.Join(argv, " "), namespace), commandFailure(stderr, code))
	}
	return stdout, stderr, code, nil
}

func (e *Engine) run(namespace string, argv ...string) (string, string, int, error) {
	const op = "chaos.Engine.run"
	if len(argv) == 0 || argv[0] != tcProgram {
		return "", "", 0, apperr.Permission(op, "the chaos engine may only run traffic control", nil)
	}
	if err := e.scope.checkNamespaceArg(namespace); err != nil {
		return "", "", 0, err
	}
	return e.runner.Run(namespace, argv...)
}

func (s Scope) checkNamespaceArg(namespace string) error {
	if s.allows(namespace) {
		return nil
	}
	return apperr.Permission("chaos.Scope.checkNamespaceArg", fmt.Sprintf(
		"the namespace %q is outside the chaos scope", namespace), nil)
}

func (e *Engine) reserve(op string) error {
	active := 0
	for _, fault := range e.faults {
		if fault.Status == StatusActive {
			active++
		}
	}
	if active >= e.limits.MaxFaults {
		return apperr.LimitExceeded(op, fmt.Sprintf(
			"the laboratory already carries %d faults, the maximum is %d", active, e.limits.MaxFaults), nil)
	}
	return nil
}

func (e *Engine) remember(fault *Fault) {
	e.faults[fault.ID] = fault
	e.order = append(e.order, fault.ID)
	e.trim()
}

func (e *Engine) forget(fault *Fault) error {
	if _, ok := e.faults[fault.ID]; !ok {
		return nil
	}
	delete(e.faults, fault.ID)
	for i, id := range e.order {
		if id == fault.ID {
			e.order = append(e.order[:i], e.order[i+1:]...)
			break
		}
	}
	return nil
}

func (e *Engine) trim() {
	limit := 2 * e.limits.MaxFaults
	for len(e.order) > limit {
		removed := false
		for i, id := range e.order {
			fault, ok := e.faults[id]
			if !ok || !fault.Status.Finished() {
				continue
			}
			delete(e.faults, id)
			e.order = append(e.order[:i], e.order[i+1:]...)
			removed = true
			break
		}
		if !removed {
			return
		}
	}
}

func (e *Engine) deviceOwner(namespace, device string) (string, string) {
	for _, id := range e.order {
		fault, ok := e.faults[id]
		if !ok || fault.Status != StatusActive {
			continue
		}
		if fault.Source.Namespace == namespace && fault.Source.Interface == device {
			return device, fault.ID
		}
	}
	return device, ""
}

func (e *Engine) resolve(op string, spec Spec) (resolved, error) {
	devices, err := e.topo.Devices(spec.Target)
	if err != nil {
		return resolved{}, apperr.Wrap(err, kindOf(err), op, fmt.Sprintf(
			"the interfaces of %s could not be read", spec.Target))
	}
	device := spec.Interface
	if device == "" {
		switch len(devices) {
		case 0:
			return resolved{}, apperr.NotFound(op, fmt.Sprintf(
				"the namespace %s has no data interface to impair", spec.Target), nil)
		case 1:
			device = devices[0]
		default:
			names := append([]string(nil), devices...)
			sort.Strings(names)
			return resolved{}, apperr.InvalidArgument(op, fmt.Sprintf(
				"the namespace %s has the interfaces %s, name the one to impair",
				spec.Target, strings.Join(names, ", ")), nil)
		}
	}
	if !contains(devices, device) {
		return resolved{}, apperr.NotFound(op, fmt.Sprintf(
			"the namespace %s has no interface %q, it has %s",
			spec.Target, device, describe(devices)), nil)
	}
	target := Endpoint{Namespace: spec.Target, Interface: device}
	peer, ok := e.topo.Peer(spec.Target, device)
	if !ok || !peer.valid() {
		return resolved{}, apperr.NotFound(op, fmt.Sprintf(
			"the far end of %s is not part of the laboratory, an impaired link needs both ends", target), nil)
	}

	source, destination := target, peer
	if spec.Direction == DirectionIn {
		source, destination = peer, target
	}
	return resolved{target: target, source: source, destination: destination}, nil
}

type resolved struct {
	target      Endpoint
	source      Endpoint
	destination Endpoint
}

func describe(devices []string) string {
	if len(devices) == 0 {
		return "no data interface"
	}
	return strings.Join(devices, ", ")
}

func deriveID(spec Spec, at resolved) string {
	return fmt.Sprintf("%s@%s:%s", spec.Kind, at.source, spec.Direction)
}

func kindOf(err error) apperr.Kind {
	if kind, ok := apperr.KindOf(err); ok {
		return kind
	}
	return apperr.KindInternal
}

func commandFailure(stderr string, code int) error {
	message := strings.TrimSpace(stderr)
	if message == "" {
		return fmt.Errorf("exit status %d", code)
	}
	return fmt.Errorf("exit status %d: %s", code, message)
}

func checkExperiment(op, experiment string) error {
	if strings.TrimSpace(experiment) == "" {
		return apperr.InvalidArgument(op, "a fault must belong to an experiment", nil)
	}
	if len(experiment) > maxExperimentLength {
		return apperr.InvalidArgument(op, fmt.Sprintf(
			"the experiment id is %d characters, the maximum is %d", len(experiment), maxExperimentLength), nil)
	}
	return nil
}

const maxExperimentLength = 128

func (e *Engine) watch(ctx context.Context) {
	defer close(e.done)
	ticker := time.NewTicker(expiryTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			e.RemoveAll()
			return
		case <-e.stop:
			return
		case <-ticker.C:
			e.Expire(e.now())
		}
	}
}

const expiryTick = time.Second
