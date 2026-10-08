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

	"github.com/codingseeker/netforge/internal/apperr"
)

const balancerMode = "netforge-balancer"

const (
	StateRunning = "running"

	StateFailed = "failed"

	StateRemoved = "removed"
)

type Status struct {
	State string `json:"state"`

	Error string `json:"error,omitempty"`

	Listen string `json:"listen,omitempty"`

	Policy string `json:"policy,omitempty"`

	Backends []Backend `json:"backends,omitempty"`

	Stats Stats `json:"stats"`

	ControlError string `json:"control_error,omitempty"`

	PID int `json:"pid,omitempty"`

	At time.Time `json:"at"`
}

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

			<-done
			writeStatus(checked, instance.Status())
			return nil
		}
	}
}

func reportInterval(limits Limits) time.Duration {
	if limits.HealthInterval < time.Second {
		return time.Second
	}
	return limits.HealthInterval
}

func (b *Balancer) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.statusLocked()
}

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

func (b *Balancer) noteControlFailure(err error) {
	b.mu.Lock()
	b.controlErr = err.Error()
	status := b.statusLocked()
	b.mu.Unlock()
	b.writeStatusNow(status)
}

func (b *Balancer) writeStatusNow(status Status) { writeStatus(b.config, status) }

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
