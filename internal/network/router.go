package network

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/namespace"
	"github.com/netforge/netforge/internal/reconcile"
	"github.com/netforge/netforge/internal/router"
)

const (
	routerMode  = "netforge-router"
	routerReady = 3 * time.Second
	// routerReport is the wait for one round trip of the router's own counters.
	// It is far longer than starting the router needs, because the router is
	// busy forwarding while it is asked, and a diagnostic read that gives up
	// under load reports a laboratory that is fine as one that is broken.
	routerReport  = 20 * time.Second
	routerStop    = 2 * time.Second
	routerPoll    = 20 * time.Millisecond
	routerStats   = "router.json"
	routerStateOn = "running"
)

type routerProcess struct {
	pid   int
	path  string
	space *namespace.Space
	last  string
}

func (l *Lab) startRouter() error {
	const op = "network.Create"
	space, ok := l.spaces[l.plan.RouterNS]
	if !ok {
		return fmt.Errorf("%s: the router namespace is missing", op)
	}
	if err := l.net.DisableForwarding(op, space.Handle); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if l.dir == "" {
		// The router's statistics directory carries an ownership marker for the
		// same reason the laboratory's does: a run killed with SIGKILL leaves it
		// behind, and a reconciler may only remove what it can prove it created.
		dir, _, err := reconcile.Create(reconcile.StateRoot(), reconcile.KindRouter, l.spec.ID, time.Now())
		if err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
		l.dir = dir
	}
	settings, err := json.Marshal(l.routerConfig())
	if err != nil {
		return fmt.Errorf("%s: encode the router configuration: %w", op, err)
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	process, err := space.Handle.Start(self, routerMode, string(settings))
	if err != nil {
		return fmt.Errorf("%s: start the router: %w", op, err)
	}
	l.router = &routerProcess{
		pid:   process,
		path:  filepath.Join(l.dir, routerStats),
		space: space.Handle,
	}
	return l.awaitRouter()
}

func (l *Lab) routerConfig() router.Config {
	devices := make([]router.Device, 0, len(l.plan.Links))
	routes := make([]router.Route, 0, len(l.plan.Links))
	for _, link := range l.plan.Links {
		devices = append(devices, router.Device{
			Name:      link.RouterIf,
			Addresses: []netip.Prefix{link.Node.Router},
			MTU:       l.plan.MTU,
		})
		routes = append(routes, router.Route{
			Prefix: link.Network,
			Device: link.RouterIf,
		})
	}
	return router.Config{
		Scope:        l.plan.Network,
		Devices:      devices,
		Routes:       routes,
		MaxRoutes:    l.spec.Router.MaxRoutes,
		MaxNeighbors: l.spec.Router.MaxNeighbors,
		QueueSize:    l.spec.Router.QueueSize,
		StatsPath:    filepath.Join(l.dir, routerStats),
	}
}

func (l *Lab) awaitRouter() error {
	const op = "network.Create"
	deadline := time.Now().Add(routerReady)
	for time.Now().Before(deadline) {
		stats, err := router.ReadStats(l.router.path)
		if err == nil {
			if stats.State == routerStateOn {
				return nil
			}
			if stats.State == "failed" {
				return fmt.Errorf("%s: the router stopped: %s", op, stats.Error)
			}
		}
		time.Sleep(routerPoll)
	}
	return fmt.Errorf("%s: the router did not become ready within %s", op, routerReady)
}

// RouterStats asks the software router for its counters and waits for the
// answer.
func (l *Lab) RouterStats() (router.Stats, error) {
	return l.RouterStatsWithin(routerReport)
}

// RouterStatsWithin asks the software router for its counters and gives up after
// the given window.
//
// The window is a parameter because two callers want different answers: a test
// asking about a healthy router will wait, and a run reporting its counters
// while it takes the laboratory apart will not sit in a diagnostic read longer
// than the teardown it is part of.
func (l *Lab) RouterStatsWithin(window time.Duration) (router.Stats, error) {
	const op = "network.Lab.RouterStats"
	if window <= 0 {
		return router.Stats{}, apperr.InvalidArgument(op,
			"a router reading needs a positive window to give up after", nil)
	}
	l.mu.Lock()
	process := l.router
	l.mu.Unlock()
	if process == nil {
		return router.Stats{}, fmt.Errorf("%s: the laboratory has no software router", op)
	}
	var before uint64
	if current, err := router.ReadStats(process.path); err == nil {
		before = current.Revision
	}
	_ = syscall.Kill(process.pid, syscall.SIGUSR1)
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		stats, err := router.ReadStats(process.path)
		switch {
		case err == nil && stats.State == routerStateOn && stats.Revision > before:
			return stats, nil
		case err == nil && stats.State == "failed":
			return router.Stats{}, fmt.Errorf("%s: the router stopped: %s", op, stats.Error)
		}
		if syscall.Kill(process.pid, 0) != nil {
			return router.Stats{}, fmt.Errorf("%s: the router is gone, it last reported %q", op, process.last)
		}
		time.Sleep(routerPoll)
	}
	return router.Stats{}, fmt.Errorf("%s: the router did not report within %s", op, window)
}

func (l *Lab) RouterAlive() bool {
	l.mu.Lock()
	process := l.router
	l.mu.Unlock()
	if process == nil {
		return false
	}
	return process.space.Alive() && syscall.Kill(process.pid, 0) == nil
}

func (l *Lab) stopRouter() {
	l.mu.Lock()
	process := l.router
	l.router = nil
	l.mu.Unlock()
	if process == nil {
		return
	}
	_ = syscall.Kill(process.pid, syscall.SIGTERM)
	deadline := time.Now().Add(routerStop)
	for time.Now().Before(deadline) {
		if syscall.Kill(process.pid, 0) != nil {
			return
		}
		time.Sleep(routerPoll)
	}
	_ = syscall.Kill(process.pid, syscall.SIGKILL)
}

func (l *Lab) removeRouterDir() {
	if l.dir == "" {
		return
	}
	_ = os.RemoveAll(l.dir)
	l.dir = ""
}
