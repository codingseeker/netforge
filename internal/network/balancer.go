package network

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/codingseeker/netforge/internal/apperr"
	"github.com/codingseeker/netforge/internal/loadbalancer"
	"github.com/codingseeker/netforge/internal/namespace"
)

const (
	balancerMode  = "netforge-balancer"
	balancerReady = 10 * time.Second
	balancerStop  = 2 * time.Second
	balancerPoll  = 20 * time.Millisecond
	balancerState = "running"
)

type balancerProcess struct {
	pid   int
	path  string
	space *namespace.Space
}

func (l *Lab) startBalancer() error {
	const op = "network.startBalancer"
	if l.spec.Balancer == nil {
		return nil
	}
	if err := l.probeScope().Check(op, l.spec.Balancer.Namespace, l.plan.Network.Addr().String()); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	l.mu.Lock()
	space, known := l.spaces[l.spec.Balancer.Namespace]
	l.mu.Unlock()
	if !known {
		return fmt.Errorf("%s: the namespace %s is not part of this laboratory",
			op, l.spec.Balancer.Namespace)
	}
	settings, err := json.Marshal(l.balancerConfig())
	if err != nil {
		return fmt.Errorf("%s: encode the balancer configuration: %w", op, err)
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	process, err := space.Handle.Start(self, balancerMode, string(settings))
	if err != nil {
		return fmt.Errorf("%s: start the balancer: %w", op, err)
	}
	record := &balancerProcess{
		pid:   process,
		path:  filepath.Join(l.work, "balancer.json"),
		space: space.Handle,
	}
	if err := l.stack.Defer("balancer", func(context.Context) error {
		return stopBalancer(process)
	}); err != nil {
		_ = stopBalancer(process)
		return fmt.Errorf("%s: %w", op, err)
	}
	l.mu.Lock()
	l.balancer = record
	l.mu.Unlock()
	return l.awaitBalancer()
}

func (l *Lab) balancerConfig() loadbalancer.Config {
	spec := l.spec.Balancer
	config := loadbalancer.Config{
		Listen:      spec.Listen,
		Backends:    spec.Backends,
		Policy:      spec.Policy,
		Scope:       l.plan.Network.String(),
		StatusPath:  filepath.Join(l.work, "balancer.json"),
		ControlPath: filepath.Join(l.work, "balancer-control.json"),
		Experiment:  l.experiment,
		Limits:      spec.Limits,
	}
	return config
}

func (l *Lab) awaitBalancer() error {
	const op = "network.awaitBalancer"
	deadline := time.Now().Add(balancerReady)
	for time.Now().Before(deadline) {
		status, err := loadbalancer.ReadStatus(l.balancer.path)
		if err == nil {
			switch status.State {
			case loadbalancer.StateRunning:
				return nil
			case loadbalancer.StateFailed:
				return fmt.Errorf("%s: the balancer stopped: %s", op, status.Error)
			}
		}
		if !l.balancer.space.Alive() || syscall.Kill(l.balancer.pid, 0) != nil {
			return fmt.Errorf("%s: the balancer is gone", op)
		}
		time.Sleep(balancerPoll)
	}
	return fmt.Errorf("%s: the balancer did not become ready within %s", op, balancerReady)
}

func (l *Lab) BalancerStatus() (loadbalancer.Status, error) {
	const op = "network.BalancerStatus"
	l.mu.Lock()
	process := l.balancer
	l.mu.Unlock()
	if process == nil {
		return loadbalancer.Status{}, apperr.NotFound(op, "the laboratory has no load balancer", nil)
	}
	return loadbalancer.ReadStatus(process.path)
}

func (l *Lab) BalancerControl() (path, experiment string, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.balancer == nil || l.spec.Balancer == nil {
		return "", "", false
	}
	return filepath.Join(l.work, "balancer-control.json"), l.experiment, true
}

func (l *Lab) BalancerNamespace() (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.spec.Balancer == nil {
		return "", false
	}
	return l.spec.Balancer.Namespace, true
}

func (l *Lab) BalancerListen() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.spec.Balancer == nil {
		return ""
	}
	return l.spec.Balancer.Listen
}

func stopBalancer(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return fmt.Errorf("stop the balancer: %w", err)
	}
	deadline := time.Now().Add(balancerStop)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return nil
		}
		time.Sleep(balancerPoll)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	return nil
}

func ReliableServiceFor(namespace string, port uint16) (ServiceSpec, error) {
	const op = "network.ReliableServiceFor"
	if strings.TrimSpace(namespace) == "" {
		return ServiceSpec{}, apperr.InvalidArgument(op, "a service needs a namespace to serve from", nil)
	}
	if port == 0 {
		return ServiceSpec{}, apperr.InvalidArgument(op, "a service needs a port to bind", nil)
	}
	return ServiceSpec{Namespace: namespace, RUDPPort: port}, nil
}
