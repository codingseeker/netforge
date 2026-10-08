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

	"github.com/codingseeker/netforge/internal/apperr"
)

const MaxCommands = 64

const MaxControlBytes = 64 << 10

type Operation string

const (
	OpRemoveBackend Operation = "remove_backend"

	OpRestoreBackend Operation = "restore_backend"

	OpDrainConnection Operation = "drain_connection"

	OpReEnableBackend Operation = "re_enable_backend"
)

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

type Command struct {
	Seq uint64 `json:"seq"`

	Operation Operation `json:"operation"`

	Backend string `json:"backend"`
}

type Control struct {
	Experiment string `json:"experiment"`

	Commands []Command `json:"commands"`
}

func marshalControl(control Control) ([]byte, error) {
	return json.Marshal(control)
}

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

type control struct {
	mu      sync.Mutex
	applied uint64
	digest  string

	off bool
}

func (b *Balancer) pollControl() {
	path := b.config.ControlPath
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):

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

	b.control.mu.Lock()
	for _, command := range control.Commands {
		if command.Seq > b.control.applied {
			b.control.applied = command.Seq
		}
	}
	b.control.off = false
	b.control.mu.Unlock()
}

func (b *Balancer) applyCommand(command Command) error {
	const op = "loadbalancer.applyCommand"
	if command.Seq == 0 {
		return apperr.Validation(op, "a command needs a sequence, so it is applied exactly once", nil)
	}
	b.control.mu.Lock()
	replay := command.Seq <= b.control.applied
	b.control.mu.Unlock()
	if replay {

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

		entry.healthy = false
		entry.failures = 0
		entry.successes = 0
	case OpDrainConnection:
		b.beginDrainLocked(entry)
	case OpReEnableBackend:
		entry.drainUntil = time.Time{}
		if entry.removed {

			b.mu.Unlock()
			return apperr.Conflict(op, "backend "+strconv.Quote(command.Backend)+
				" was removed rather than drained, use "+string(OpRestoreBackend)+" to bring it back", nil)
		}
		entry.healthy = true
	default:
		b.mu.Unlock()
		return apperr.Unsupported(op, strconv.Quote(string(command.Operation))+" has no implementation", nil)
	}

	status := b.statusLocked()
	b.mu.Unlock()

	b.writeStatusNow(status)
	return nil
}

func (b *Balancer) backendLocked(address string) *backend {
	for _, entry := range b.backends {
		if entry.address == address {
			return entry
		}
	}
	return nil
}

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

func (b *Balancer) Applied() uint64 {
	b.control.mu.Lock()
	defer b.control.mu.Unlock()
	return b.control.applied
}
