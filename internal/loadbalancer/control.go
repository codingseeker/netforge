package loadbalancer

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/netforge/netforge/internal/apperr"
)

// The control document is how a controller changes which of a balancer's own
// backends are in service. It is deliberately much smaller than a
// registration interface: a command may change the service state of an address
// the configuration already lists, and may not introduce an address, move a
// backend onto a different one, or change anything else. A recovery step needs
// to take a backend out and put it back, and it does not need the power to
// decide who the backends are.

// MaxCommands is the largest number of commands one control document may hold.
const MaxCommands = 64

// MaxControlBytes is the largest control document that is read.
const MaxControlBytes = 64 << 10

// Operation names one change a controller may ask a running balancer to make.
type Operation string

const (
	// OpRemoveBackend takes a backend out of service. It keeps its
	// configuration entry and can be restored by address.
	OpRemoveBackend Operation = "remove_backend"
	// OpRestoreBackend puts a removed backend back to being checked. It is not
	// put straight back into service: a restored backend has not been proven
	// healthy, so the health checks have to prove it first.
	OpRestoreBackend Operation = "restore_backend"
	// OpDrainConnection stops placing clients on a backend and lets the
	// connections it holds finish, after which the backend serves again.
	OpDrainConnection Operation = "drain_connection"
	// OpReEnableBackend cancels a drain, so the backend takes clients again
	// before the drain deadline.
	OpReEnableBackend Operation = "re_enable_backend"
)

// Operations lists every operation a control document may name.
func Operations() []Operation {
	return []Operation{OpRemoveBackend, OpRestoreBackend, OpDrainConnection, OpReEnableBackend}
}

func (o Operation) String() string { return string(o) }

func (o Operation) valid() bool {
	for _, known := range Operations() {
		if o == known {
			return true
		}
	}
	return false
}

// Command is one requested change.
type Command struct {
	// Seq is the position of this command in the experiment's sequence. A
	// balancer applies a command once and remembers the highest sequence it has
	// applied, so a document that is written again with the same commands is
	// not applied twice and a retry after a crash is not a second change.
	Seq uint64 `json:"seq"`
	// Operation is what to change.
	Operation Operation `json:"operation"`
	// Backend is the address the change is about. It has to be one of the
	// addresses the configuration registered.
	Backend string `json:"backend"`
}

// Control is the document a controller writes for a running balancer.
type Control struct {
	// Experiment names the experiment the commands belong to, so a command
	// written for one experiment is refused by the balancer of another.
	Experiment string `json:"experiment"`
	// Commands are the requested changes, in sequence order.
	Commands []Command `json:"commands"`
}

// marshalControl encodes a control document the way WriteControl does, so a
// caller can check what will be written without writing it.
func marshalControl(control Control) ([]byte, error) {
	return json.Marshal(control)
}

// ParseControl decodes a control document strictly.
//
// It is strict for the same reason configuration is: a control document is
// data, a field that is misspelled is a mistake, and a field that is ignored is
// a change an operator believed was made and was not.
func ParseControl(data []byte) (Control, error) {
	const op = "loadbalancer.ParseControl"
	if len(data) == 0 {
		return Control{}, apperr.Validation(op, "the control document is empty", nil)
	}
	if len(data) > MaxControlBytes {
		return Control{}, apperr.LimitExceeded(op,
			"the control document is "+strconv.Itoa(len(data))+" bytes, the maximum is "+strconv.Itoa(MaxControlBytes), nil)
	}
	var control Control
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&control); err != nil {
		return Control{}, apperr.Wrap(err, apperr.KindInvalidConfig, op, "the control document could not be decoded")
	}
	// A document with trailing content is refused rather than read up to the
	// first object, because the part after the object is either a second
	// document or a mistake and neither should be silently dropped.
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return Control{}, apperr.Validation(op, "the control document has content after its object", nil)
	}
	if err := control.validate(op); err != nil {
		return Control{}, err
	}
	return control, nil
}

func (c Control) validate(op string) error {
	if strings.TrimSpace(c.Experiment) == "" {
		return apperr.Validation(op, "experiment is required, so a balancer can refuse a command meant for another run", nil)
	}
	if len(c.Commands) == 0 {
		return apperr.Validation(op, "commands must not be empty", nil)
	}
	if len(c.Commands) > MaxCommands {
		return apperr.LimitExceeded(op, "the control document carries "+strconv.Itoa(len(c.Commands))+
			" commands, the maximum is "+strconv.Itoa(MaxCommands), nil)
	}
	for i, command := range c.Commands {
		field := "commands[" + strconv.Itoa(i) + "]"
		if !command.Operation.valid() {
			return apperr.Unsupported(op, field+".operation "+strconv.Quote(string(command.Operation))+
				" is not an operation, want one of "+operationList(), nil)
		}
		if command.Backend == "" {
			return apperr.Validation(op, field+".backend is required", nil)
		}
	}
	return nil
}

func operationList() string {
	names := make([]string, 0, len(Operations()))
	for _, operation := range Operations() {
		names = append(names, string(operation))
	}
	return strings.Join(names, ", ")
}

// WriteControl writes a control document where a balancer will read it.
//
// The write is atomic and the file is private to its owner, because a document
// that is half written would be a document a balancer cannot parse, and one
// another user can write is a document that decides which backends serve.
func WriteControl(path string, control Control) error {
	const op = "loadbalancer.WriteControl"
	if path == "" {
		return apperr.InvalidArgument(op, "a control path is required", nil)
	}
	if err := control.validate(op); err != nil {
		return err
	}
	data, err := json.Marshal(control)
	if err != nil {
		return apperr.Wrap(err, apperr.KindInternal, op, "the control document could not be encoded")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return apperr.Wrap(err, apperr.KindIO, op, "the control directory could not be created")
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return apperr.Wrap(err, apperr.KindIO, op, "the control document could not be written")
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return apperr.Wrap(err, apperr.KindIO, op, "the control document could not be put in place")
	}
	return nil
}

// control is a balancer's own view of the control file.
type control struct {
	// mu guards applied and digest, which are read on the health tick and
	// written by whoever applies a command.
	mu      sync.Mutex
	applied uint64
	digest  string
	// off is true once a document has been refused, so a broken document is
	// reported once rather than on every tick.
	off bool
}

// pollControl reads the control file and applies what is new in it.
//
// It is called from the health tick, so the interval a control change takes to
// take effect is the health interval, which is already the granularity at which
// this balancer notices that a backend has changed state on its own. A file
// that cannot be read or parsed is reported to the caller and then left alone:
// a controller that crashed mid-write must not stop the balancer from serving.
func (b *Balancer) pollControl() {
	path := b.config.ControlPath
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// Nothing has been asked of this balancer yet.
		return
	case err != nil:
		b.reportControlError(apperr.Wrap(err, apperr.KindIO, "loadbalancer.pollControl",
			"the control document could not be read"))
		return
	}
	control, err := ParseControl(data)
	if err != nil {
		b.reportControlError(err)
		return
	}
	if control.Experiment != b.config.ExperimentID() {
		b.reportControlError(apperr.Permission("loadbalancer.pollControl",
			"the control document is for experiment "+control.Experiment+
				", this balancer serves experiment "+b.config.ExperimentID(), nil))
		return
	}
	for _, command := range control.Commands {
		if err := b.applyCommand(command); err != nil {
			b.reportControlError(err)
			return
		}
	}
	// The highest sequence applied is remembered, so a document that is written
	// again with the same commands is not a second change.
	b.control.mu.Lock()
	for _, command := range control.Commands {
		if command.Seq > b.control.applied {
			b.control.applied = command.Seq
		}
	}
	b.control.off = false
	b.control.mu.Unlock()
}

// applyCommand performs one command, refusing anything this balancer was not
// configured with.
func (b *Balancer) applyCommand(command Command) error {
	const op = "loadbalancer.applyCommand"
	if command.Seq == 0 {
		return apperr.Validation(op, "a command needs a sequence, so it is applied exactly once", nil)
	}
	b.control.mu.Lock()
	replay := command.Seq <= b.control.applied
	b.control.mu.Unlock()
	if replay {
		// A command that has already been applied is a no-op rather than an
		// error: re-reading the same document is how a controller retries, and
		// a drain applied twice would drain again.
		return nil
	}

	b.mu.Lock()
	entry := b.backendLocked(command.Backend)
	if entry == nil {
		b.mu.Unlock()
		return apperr.Permission(op, "backend "+strconv.Quote(command.Backend)+
			" is not registered with this balancer, and a control document may not introduce one", nil)
	}
	switch command.Operation {
	case OpRemoveBackend:
		entry.removed = true
		entry.healthy = false
		b.beginDrainLocked(entry)
	case OpRestoreBackend:
		entry.removed = false
		entry.drainUntil = time.Time{}
		// The backend is not declared healthy here. It is checked like any
		// other, and it serves again when the checks say it does.
		entry.healthy = false
		entry.failures = 0
		entry.successes = 0
	case OpDrainConnection:
		b.beginDrainLocked(entry)
	case OpReEnableBackend:
		entry.drainUntil = time.Time{}
		if entry.removed {
			// Re-enabling something that was taken out of service would put it
			// back without a health check, so it is refused rather than guessed at.
			b.mu.Unlock()
			return apperr.Conflict(op, "backend "+strconv.Quote(command.Backend)+
				" was removed rather than drained, use "+string(OpRestoreBackend)+" to bring it back", nil)
		}
		entry.healthy = true
	default:
		b.mu.Unlock()
		return apperr.Unsupported(op, strconv.Quote(string(command.Operation))+" has no implementation", nil)
	}
	// The consistent-hash ring is rebuilt from the candidate set the next time a
	// key is placed, and a removed backend is not in that set, so there is
	// nothing to invalidate here. Rebuilding it eagerly would cost a sort on a
	// tick that may have changed nothing.
	status := b.statusLocked()
	b.mu.Unlock()

	// The change is reported before it is acknowledged by the next tick, so a
	// controller that reads the status file sees the effect of its own command.
	b.writeStatusNow(status)
	return nil
}

// backendLocked finds a registered backend by the address it was registered
// with. It only ever matches an entry from the configuration, which is what
// keeps a control document from adding a backend.
func (b *Balancer) backendLocked(address string) *backend {
	for _, entry := range b.backends {
		if entry.address == address {
			return entry
		}
	}
	return nil
}

// reportControlError records a control document that could not be applied. It
// is reported once per document so a persistent problem does not fill the
// status file, and it never stops the balancer: a controller that is broken is
// not a reason to stop serving the connections that are already up.
func (b *Balancer) reportControlError(err error) {
	b.control.mu.Lock()
	first := !b.control.off
	b.control.off = true
	b.control.mu.Unlock()
	if !first {
		return
	}
	b.noteControlFailure(err)
}

// Applied reports the highest control sequence this balancer has applied.
func (b *Balancer) Applied() uint64 {
	b.control.mu.Lock()
	defer b.control.mu.Unlock()
	return b.control.applied
}
