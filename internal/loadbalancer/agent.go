package loadbalancer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/netforge/netforge/internal/apperr"
)

// balancerMode is the argument the balancer agent is started with, in the same
// way a laboratory starts a service agent.
const balancerMode = "netforge-balancer"

// The states an agent reports in its status file.
const (
	// StateRunning means the balancer is bound and serving.
	StateRunning = "running"
	// StateFailed means it could not start, with the reason.
	StateFailed = "failed"

	// StateRemoved is a backend a control document took out of service. It is
	// reported apart from StateDraining because the two mean different things to
	// a reader: a draining backend is failing or finishing its work, while a
	// removed one was taken out on purpose and stays out until it is restored.
	StateRemoved = "removed"
)

// Status is what a running balancer reports about itself. A laboratory reads it
// to know when the balancer is serving and to learn afterwards which backend each
// client was placed with, which is the only record of a decision the balancer
// made in passing.
type Status struct {
	// State is StateRunning or StateFailed.
	State string `json:"state"`
	// Error is why it failed.
	Error string `json:"error,omitempty"`
	// Listen is the address it is bound to, which is the answer when the
	// configuration asked for port zero.
	Listen string `json:"listen,omitempty"`
	// Policy is the policy the backends were chosen under.
	Policy string `json:"policy,omitempty"`
	// Backends is what was known about each backend when this was written.
	Backends []Backend `json:"backends,omitempty"`
	// Stats are the balancer's totals at the moment this was written.
	Stats Stats `json:"stats"`
	// ControlError is the last control document that could not be applied, which
	// is how a laboratory learns its command was refused.
	ControlError string `json:"control_error,omitempty"`
	// PID is the process serving, so a status names what is answering rather
	// than only that something is.
	PID int `json:"pid,omitempty"`
	// At is when the status was written.
	At time.Time `json:"at"`
}

// Serve runs a balancer until the context is cancelled, reporting its state to
// the status file as it goes. The status is written again whenever the balancer's
// view of its backends changes, so a laboratory that reads it after a fault sees
// which backend left service and why.
func Serve(ctx context.Context, config Config) error {
	const op = "loadbalancer.Serve"
	checked, err := config.Validate()
	if err != nil {
		writeStatus(config, Status{State: StateFailed, Error: err.Error()})
		return err
	}
	instance, err := New(checked)
	if err != nil {
		writeStatus(checked, Status{State: StateFailed, Error: err.Error()})
		return fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = instance.Close() }()
	writeStatus(checked, instance.Status())

	// The status is rewritten on a tick rather than on every change, because a
	// status file written per decision is a file written per connection, and the
	// laboratory that reads it wants the state of the balancer rather than its
	// history.
	ticker := time.NewTicker(reportInterval(checked.Limits))
	defer ticker.Stop()
	done := make(chan error, 1)
	go func() { done <- instance.Serve(ctx) }()
	for {
		select {
		case err := <-done:
			writeStatus(checked, instance.Status())
			return err
		case <-ticker.C:
			writeStatus(checked, instance.Status())
		case <-ctx.Done():
			// Serve returns once the balancer has closed, so the status written
			// here is the one a reader sees after the balancer has stopped
			// rather than the one it had while it was running.
			<-done
			writeStatus(checked, instance.Status())
			return nil
		}
	}
}

// reportInterval is how often the status is rewritten. It is the health interval
// when that is slow enough to be worth reading, and no faster than that, so the
// status file never costs more than the checks it reports.
func reportInterval(limits Limits) time.Duration {
	if limits.HealthInterval < time.Second {
		return time.Second
	}
	return limits.HealthInterval
}

// Status renders what the balancer knows about itself.
//
// It is exported for the two readers that need it: the agent writing the status
// file, and whoever is waiting to be told whether a control document was
// accepted. A controller that has just removed a backend can read the answer
// here rather than assuming that writing a document is the same as applying it.
func (b *Balancer) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.statusLocked()
}

// statusLocked builds the status with the lock already held, for the paths that
// change a backend and report the effect in one step.
func (b *Balancer) statusLocked() Status {
	return Status{
		State:        StateRunning,
		Listen:       b.addressLocked(),
		Policy:       string(b.config.Policy),
		Backends:     b.backendsLocked(),
		Stats:        b.statsLocked(),
		ControlError: b.controlErr,
		PID:          os.Getpid(),
		At:           time.Now(),
	}
}

// noteControlFailure records a control document that could not be applied and
// reports the balancer, so a laboratory reading the status file sees that its
// command was refused rather than seeing silence.
func (b *Balancer) noteControlFailure(err error) {
	b.mu.Lock()
	b.controlErr = err.Error()
	status := b.statusLocked()
	b.mu.Unlock()
	b.writeStatusNow(status)
}

// writeStatusNow writes a status the caller has already built.
func (b *Balancer) writeStatusNow(status Status) { writeStatus(b.config, status) }

// ReadStatus reads a balancer status file.
func ReadStatus(path string) (Status, error) {
	const op = "loadbalancer.ReadStatus"
	data, err := os.ReadFile(path)
	if err != nil {
		return Status{}, apperr.IO(op, "read "+path, err)
	}
	var status Status
	if err := json.Unmarshal(data, &status); err != nil {
		return Status{}, apperr.IO(op, "decode "+path, err)
	}
	return status, nil
}

// writeStatus reports the balancer state atomically, so a reader never sees a
// half-written file.
func writeStatus(config Config, status Status) {
	if config.StatusPath == "" {
		return
	}
	if status.At.IsZero() {
		status.At = time.Now()
	}
	data, err := json.Marshal(status)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(config.StatusPath), 0o700); err != nil {
		return
	}
	temporary := config.StatusPath + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(temporary, config.StatusPath)
}

// Dispatch runs the balancer agent. It is started inside a laboratory namespace
// by the same mechanism that starts a service there, and it is only reachable
// from that namespace: the configuration is passed as an argument by the
// laboratory that built it, and nothing that can reach the listening socket can
// add a backend to it.
func Dispatch(args []string) (bool, int) {
	if len(args) == 0 || args[0] != balancerMode {
		return false, 0
	}
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, balancerMode+": expected one configuration argument")
		return true, 2
	}
	var config Config
	if err := json.Unmarshal([]byte(args[1]), &config); err != nil {
		fmt.Fprintln(os.Stderr, balancerMode+": "+err.Error())
		return true, 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := Serve(ctx, config); err != nil {
		fmt.Fprintln(os.Stderr, balancerMode+": "+err.Error())
		return true, 1
	}
	return true, 0
}
