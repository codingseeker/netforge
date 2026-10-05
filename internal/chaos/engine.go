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

// Runner executes a command inside a named laboratory namespace and reports
// its exit status. The namespace layer owns process creation; the engine only
// ever supplies a validated argument vector, never a shell.
type Runner interface {
	Run(namespace string, argv ...string) (stdout, stderr string, code int, err error)
}

// Topology describes the laboratory an engine is allowed to touch. It is
// consulted before any command runs, so a device that does not exist is
// refused rather than discovered by trial and error.
type Topology interface {
	// Devices lists the data interfaces of a namespace, without loopback.
	Devices(namespace string) ([]string, error)
	// Peer returns the far end of the link a device belongs to.
	Peer(namespace, device string) (Endpoint, bool)
}

// Engine injects faults into one laboratory and takes them back out again.
//
// Every fault is bounded by the limits, restricted to the scope, verified
// against the kernel after it is applied, and removed when it expires, when
// it is removed by name, or when the engine is closed. The engine holds its
// lock while it talks to the laboratory so that a fault is never applied and
// removed at the same time.
type Engine struct {
	mu     sync.Mutex
	scope  Scope
	limits Limits
	runner Runner
	topo   Topology
	now    func() time.Time

	faults map[string]*Fault
	// order keeps the insertion order so the oldest finished record is the one
	// that is forgotten when the history is full.
	order []string

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	closed   bool
}

// New builds an engine for one laboratory. The returned engine starts its
// expiry watcher, so the caller must Close it when the laboratory goes away.
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

// Limits returns the limits the engine enforces.
func (e *Engine) Limits() Limits { return e.limits }

// Scope returns the namespaces the engine may touch.
func (e *Engine) Scope() Scope { return e.scope }

// Apply injects one fault and returns the record the kernel agreed to.
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
		// The installation already undoes whatever it managed to do, so the
		// failed record is not kept: a fault that is not in the kernel has no
		// place in the ledger.
		_ = e.forget(&fault)
		return Fault{}, err
	}
	e.remember(&fault)
	return fault, nil
}

// ApplyAll injects a set of faults. If any of them cannot be applied, the ones
// that were already installed are removed again, so a rejected set never
// leaves a laboratory half impaired.
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

// rollback removes faults in reverse order and returns the failures.
func (e *Engine) rollback(faults []Fault) []error {
	var failures []error
	for i := len(faults) - 1; i >= 0; i-- {
		if _, err := e.Remove(faults[i].ID); err != nil {
			failures = append(failures, err)
		}
	}
	return failures
}

// Remove takes a fault back out of the kernel. Removing a fault that is
// already gone succeeds, so cleanup paths can run more than once.
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

// RemoveAll takes every active fault back out and returns the failures.
//
// Removal stays available on a closed engine. Closing an engine is how a
// laboratory hands its faults back, so refusing to remove them afterwards would
// leave the last faults in the kernel with nothing left to take them out.
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

// Expire removes every fault whose deadline has passed and returns the number
// of faults that were removed. The expiry watcher calls it; a caller that
// drives time itself can call it directly.
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

// Get returns a fault record by id.
func (e *Engine) Get(id string) (Fault, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	fault, ok := e.faults[id]
	if !ok {
		return Fault{}, false
	}
	return *fault, true
}

// List returns every known fault record, oldest first.
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

// Active returns the number of faults currently expected in the kernel.
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

// Close stops the expiry watcher and takes every fault back out of the
// laboratory. It is safe to call more than once.
func (e *Engine) Close() error {
	// The engine stops taking new work before it starts cleaning up, so a
	// fault cannot slip in between the last removal and the return.
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()

	e.stopOnce.Do(func() { close(e.stop) })
	<-e.done
	return e.RemoveAll()
}

// install puts a fault into the kernel and verifies it. If any step fails the
// fault is taken back out again, so a failed installation leaves no trace.
func (e *Engine) install(ctx context.Context, fault *Fault) error {
	const op = "chaos.install"
	device := fault.Source.Interface
	built, err := buildPlan(device, fault.Parameters)
	if err != nil {
		return err
	}

	// The device starts from an empty discipline: replacing a netem qdisc
	// merges options, which would leave parameters from an earlier fault in
	// place and make the record describe something the kernel never applied.
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

// uninstall removes whatever the fault may have installed, ignoring failures:
// it is the rollback of a step that already failed.
func (e *Engine) uninstall(fault *Fault) {
	_, _, _, _ = e.runIn(fault.Source.Namespace, delArgs(fault.Source.Interface))
}

// detach removes a fault from the kernel and confirms it is gone.
func (e *Engine) detach(op string, fault *Fault) error {
	return e.clear(op, fault.Source.Namespace, fault.Source.Interface)
}

// clear takes a device back to the kernel's own empty discipline.
//
// The exit status of the delete is not the evidence here. The kernel reports a
// device that never carried a discipline as an error, and a device that never
// carried one is exactly the state a fault has to start from and end in. What
// decides whether the device is clear is the state the kernel reports after the
// delete, so that is what is checked.
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

// states reads and parses the kernel state of a device.
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

// runIn runs a command and turns a nonzero exit into an error, keeping the
// kernel's own message as the cause.
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

// checkNamespaceArg refuses to run in anything but a scoped namespace. It is
// the last line of defence: even a bug in a caller cannot aim the engine at
// the host network.
func (s Scope) checkNamespaceArg(namespace string) error {
	if s.allows(namespace) {
		return nil
	}
	return apperr.Permission("chaos.Scope.checkNamespaceArg", fmt.Sprintf(
		"the namespace %q is outside the chaos scope", namespace), nil)
}

// reserve makes room for one more active fault and trims the history.
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

// remember stores a fault record and keeps the history within its bound.
func (e *Engine) remember(fault *Fault) {
	e.faults[fault.ID] = fault
	e.order = append(e.order, fault.ID)
	e.trim()
}

// forget drops a record that was never installed.
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

// trim forgets the oldest finished records once the history is twice the
// fault limit, so a long laboratory cannot grow the ledger without bound. A
// laboratory whose whole history is active keeps its records.
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

// deviceOwner returns the id of the active fault on a device, if any.
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

// resolve decides which device carries the fault and what the two ends of the
// impaired link are. The device is only chosen from the topology, never by
// running a command and hoping for the best.
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
	// The queue discipline always sits on the egress side of the link: an
	// inbound fault is applied where the packets leave the peer.
	source, destination := target, peer
	if spec.Direction == DirectionIn {
		source, destination = peer, target
	}
	return resolved{target: target, source: source, destination: destination}, nil
}

// resolved is the placement of a fault on a link.
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

// deriveID builds a stable identifier for a fault that was not named.
func deriveID(spec Spec, at resolved) string {
	return fmt.Sprintf("%s@%s:%s", spec.Kind, at.source, spec.Direction)
}

// kindOf keeps the kind of an error that already carries one, so wrapping
// does not lose the reason a step failed.
func kindOf(err error) apperr.Kind {
	if kind, ok := apperr.KindOf(err); ok {
		return kind
	}
	return apperr.KindInternal
}

// commandFailure turns a nonzero exit into a cause that keeps the message the
// program printed.
func commandFailure(stderr string, code int) error {
	message := strings.TrimSpace(stderr)
	if message == "" {
		return fmt.Errorf("exit status %d", code)
	}
	return fmt.Errorf("exit status %d: %s", code, message)
}

// checkExperiment validates the experiment a fault belongs to.
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

// watch removes expired faults until the engine is closed or the laboratory
// context ends. A laboratory that goes away without closing the engine still
// gets its faults taken back out.
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

// expiryTick is how often the engine looks for faults that ran out of time.
const expiryTick = time.Second
