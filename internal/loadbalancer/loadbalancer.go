package loadbalancer

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codingseeker/netforge/internal/apperr"
	"github.com/codingseeker/netforge/internal/transport"
)

var ErrNoBackend = errors.New("no backend can take this connection")

const (
	StateHealthy = "healthy"

	StateDraining = "draining"
)

type Config struct {
	Listen string `json:"listen"`

	Backends []string `json:"backends"`

	Policy Policy `json:"policy"`

	Scope string `json:"scope,omitempty"`

	StatusPath string `json:"status_path,omitempty"`

	ControlPath string `json:"control_path,omitempty"`

	Experiment string `json:"experiment,omitempty"`

	Limits Limits `json:"limits"`
}

type Backend struct {
	Address string `json:"address"`

	State string `json:"state"`

	Active int `json:"active"`

	Accepted int `json:"accepted"`
	Skipped  int `json:"skipped"`

	Checks       int `json:"checks"`
	FailedChecks int `json:"failed_checks"`
	Consecutive  int `json:"consecutive_failures"`
	Recoveries   int `json:"recoveries"`

	Forwarded int `json:"forwarded_bytes"`
	Returned  int `json:"returned_bytes"`

	Drained int `json:"drained_connections"`

	Latency     time.Duration `json:"latency,omitempty"`
	LastChecked time.Time     `json:"last_checked,omitempty"`
	LastError   string        `json:"last_error,omitempty"`

	DrainingSince time.Time `json:"draining_since,omitempty"`
}

func (c Config) Validate() (Config, error) {
	const op = "loadbalancer.Config.Validate"
	if c.Listen == "" {
		return c, apperr.InvalidArgument(op, "a balancer needs an address to listen on", nil)
	}
	if _, err := parseEndpoint(op, "the listen address", c.Listen, true); err != nil {
		return c, err
	}
	if len(c.Backends) == 0 {
		return c, apperr.InvalidArgument(op, "a balancer needs at least one backend", nil)
	}
	limits, err := c.Limits.Validate()
	if err != nil {
		return c, err
	}
	c.Limits = limits
	if len(c.Backends) > limits.MaxBackends {
		return c, apperr.LimitExceeded(op, fmt.Sprintf(
			"%d backends were configured and the limit is %d", len(c.Backends), limits.MaxBackends), nil)
	}
	if c.Policy == "" {
		c.Policy = PolicyRoundRobin
	}
	if !c.Policy.Valid() {
		return c, apperr.InvalidArgument(op, fmt.Sprintf(
			"%q is not a policy this balancer implements", c.Policy), nil)
	}
	var scope netip.Prefix
	if c.Scope != "" {
		parsed, err := netip.ParsePrefix(c.Scope)
		if err != nil {
			return c, apperr.InvalidArgument(op, fmt.Sprintf(
				"the scope %q is not a prefix", c.Scope), err)
		}

		if parsed != parsed.Masked() {
			return c, apperr.InvalidArgument(op, fmt.Sprintf(
				"the scope %q has host bits set; the network is %s", c.Scope, parsed.Masked()), nil)
		}

		if parsed.Bits() == 0 {
			return c, apperr.InvalidArgument(op, fmt.Sprintf(
				"the scope %q is a default route, which names every address and so names none", c.Scope), nil)
		}
		scope = parsed
	}
	seen := make(map[string]bool, len(c.Backends))
	for index, address := range c.Backends {
		endpoint, err := parseEndpoint(op, fmt.Sprintf("backend %d", index), address, false)
		if err != nil {
			return c, err
		}
		if scope.IsValid() && !scope.Contains(endpoint.Addr()) {
			return c, apperr.InvalidArgument(op, fmt.Sprintf(
				"backend %d is %s, which is outside the scope %s", index, address, scope), nil)
		}
		if seen[address] {
			return c, apperr.InvalidArgument(op, fmt.Sprintf(
				"backend %d repeats %s; a backend registered twice is two shares of one server",
				index, address), nil)
		}
		seen[address] = true
	}
	if err := c.validateControl(); err != nil {
		return c, err
	}
	return c, nil
}

func (c Config) validateControl() error {
	const op = "loadbalancer.Config.Validate"
	if c.ControlPath == "" {
		if c.Experiment != "" {
			return apperr.InvalidArgument(op,
				"an experiment was named but no control_path, so the name would guard nothing", nil)
		}
		return nil
	}
	if !filepath.IsAbs(c.ControlPath) {
		return apperr.InvalidArgument(op, fmt.Sprintf(
			"the control path %q is relative; a control file resolved against a working directory is one nobody can find",
			c.ControlPath), nil)
	}
	if c.StatusPath != "" && filepath.Clean(c.ControlPath) == filepath.Clean(c.StatusPath) {
		return apperr.InvalidArgument(op,
			"the control path and the status path are the same file", nil)
	}
	if strings.TrimSpace(c.Experiment) == "" {
		return apperr.InvalidArgument(op,
			"a control_path needs an experiment, so a document written for another run is refused", nil)
	}
	if len(c.Experiment) > 64 {
		return apperr.InvalidArgument(op, fmt.Sprintf(
			"the experiment name is %d characters, the maximum is 64", len(c.Experiment)), nil)
	}
	return nil
}

func (c Config) ExperimentID() string { return c.Experiment }

func parseEndpoint(op, what, address string, anyPort bool) (netip.AddrPort, error) {
	endpoint, err := netip.ParseAddrPort(address)
	if err != nil {
		return netip.AddrPort{}, apperr.InvalidArgument(op, fmt.Sprintf(
			"%s %q is not an address and a port", what, address), err)
	}
	if endpoint.Port() == 0 && !anyPort {
		return netip.AddrPort{}, apperr.InvalidArgument(op, fmt.Sprintf(
			"%s %q has no port", what, address), nil)
	}
	if endpoint.Addr().IsUnspecified() {
		return netip.AddrPort{}, apperr.InvalidArgument(op, fmt.Sprintf(
			"%s %q is the unspecified address, which is not a host to send to", what, address), nil)
	}
	if endpoint.Addr().IsMulticast() {
		return netip.AddrPort{}, apperr.InvalidArgument(op, fmt.Sprintf(
			"%s %q is a multicast address", what, address), nil)
	}

	if endpoint.Addr().Zone() != "" {
		return netip.AddrPort{}, apperr.InvalidArgument(op, fmt.Sprintf(
			"%s %q names the interface %q, which is a property of the host reading it rather than of the backend",
			what, address, endpoint.Addr().Zone()), nil)
	}

	if endpoint.Addr().Is4() && endpoint.Addr() == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return netip.AddrPort{}, apperr.InvalidArgument(op, fmt.Sprintf(
			"%s %q is a broadcast address, which no host answers on", what, address), nil)
	}
	return endpoint, nil
}

type backend struct {
	address  string
	endpoint netip.AddrPort

	healthy bool

	removed bool

	failures  int
	successes int

	active int

	drainUntil time.Time

	sessions map[*session]struct{}

	stats Backend
}

type Balancer struct {
	config Config
	limits Limits

	listener *transport.Listener

	life   context.Context
	stop   context.CancelFunc
	closed bool

	accepting bool
	serving   chan struct{}
	serveOnce sync.Once

	backends []*backend

	cursor  int
	ring    []point
	ringFor []string

	sessions map[*transport.Conn]*session

	served           int
	refused          int
	refusedNoBackend int

	control    control
	controlErr string

	mu sync.Mutex

	wg sync.WaitGroup
}

type session struct {
	client   *transport.Conn
	upstream *transport.Conn
	backend  *backend
	started  time.Time

	stopped atomic.Bool

	once sync.Once
}

func New(config Config) (*Balancer, error) {
	checked, err := config.Validate()
	if err != nil {
		return nil, err
	}
	balancer := &Balancer{
		config:   checked,
		limits:   checked.Limits,
		sessions: make(map[*transport.Conn]*session),
	}
	for _, address := range checked.Backends {
		endpoint, _ := netip.ParseAddrPort(address)
		entry := &backend{
			address:  address,
			endpoint: endpoint,
			healthy:  true,
			sessions: make(map[*session]struct{}),
			stats:    Backend{Address: address, State: StateHealthy},
		}
		balancer.backends = append(balancer.backends, entry)
	}

	balancer.life, balancer.stop = context.WithCancel(context.Background())
	balancer.serving = make(chan struct{})
	listener, err := transport.Listen(balancer.life, checked.Listen, checked.Limits.Transport)
	if err != nil {
		balancer.stop()
		return nil, err
	}
	balancer.listener = listener
	return balancer, nil
}

func (b *Balancer) Address() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.addressLocked()
}

func (b *Balancer) addressLocked() string {
	if b.listener == nil {
		return ""
	}
	return b.listener.LocalAddr().String()
}

func (b *Balancer) Policy() Policy { return b.config.Policy }

func (b *Balancer) Limits() Limits { return b.limits }

func (b *Balancer) Backends() []Backend {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.backendsLocked()
}

func (b *Balancer) backendsLocked() []Backend {
	out := make([]Backend, 0, len(b.backends))
	for _, entry := range b.backends {
		out = append(out, b.snapshot(entry))
	}
	return out
}

func (b *Balancer) snapshot(entry *backend) Backend {
	record := entry.stats
	record.Active = entry.active
	record.Consecutive = entry.failures
	switch {
	case entry.removed:
		record.State = StateRemoved
		record.DrainingSince = time.Time{}
	case entry.healthy:
		record.State = StateHealthy
		record.DrainingSince = time.Time{}
	default:
		record.State = StateDraining
	}
	return record
}

func (b *Balancer) Serve(ctx context.Context) error {

	watched := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = b.Close()
		case <-watched:
		}
	}()
	err := b.accept()
	close(watched)
	_ = b.Close()
	if errors.Is(err, transport.ErrClosed) || apperr.IsKind(err, apperr.KindCanceled) {
		return nil
	}
	return err
}

func (b *Balancer) accept() error {
	defer b.serveOnce.Do(func() { close(b.serving) })

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return transport.ErrClosed
	}
	b.accepting = true
	b.wg.Add(1)
	b.mu.Unlock()
	go func() {
		defer b.wg.Done()
		b.watchHealth(b.life)
	}()

	for {
		conn, err := b.listener.ServePeer(b.life)
		if err != nil {
			if apperr.IsKind(err, apperr.KindLimitExceeded) {

				b.mu.Lock()
				b.refused++
				b.mu.Unlock()
				continue
			}
			return err
		}
		if conn == nil {

			continue
		}
		b.admit(conn)
	}
}

func (b *Balancer) admit(client *transport.Conn) {
	key := client.RemoteAddr().String()
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		_ = client.Close()
		return
	}
	if _, running := b.sessions[client]; running {

		b.mu.Unlock()
		return
	}
	if len(b.sessions) >= b.limits.MaxConnections {
		b.refused++
		b.mu.Unlock()
		_ = client.Close()
		return
	}

	b.sessions[client] = nil
	b.mu.Unlock()

	target, upstream, err := b.place(key)
	if err != nil {
		b.mu.Lock()
		delete(b.sessions, client)
		b.refusedNoBackend++
		b.mu.Unlock()
		_ = client.Close()
		return
	}
	active := &session{client: client, upstream: upstream, backend: target, started: time.Now()}

	b.mu.Lock()
	if b.closed {
		delete(b.sessions, client)
		b.mu.Unlock()
		b.release(target)
		_ = upstream.Close()
		_ = client.Close()
		return
	}

	b.sessions[client] = active
	target.sessions[active] = struct{}{}
	target.stats.Accepted++
	b.served++
	b.wg.Add(1)
	b.mu.Unlock()

	go func() {
		defer b.wg.Done()
		b.run(active)
	}()
}

func (b *Balancer) place(key string) (*backend, *transport.Conn, error) {
	const op = "loadbalancer.place"
	chosen := b.reserve(key)
	if chosen == nil {
		return nil, nil, apperr.Wrap(ErrNoBackend, apperr.KindLimitExceeded, op,
			"every backend is draining or is at its connection limit")
	}
	upstream, err := transport.Dial(b.life, "", chosen.address, b.limits.Transport)
	if err != nil {
		b.release(chosen)

		b.recordFailure(chosen, err, false)
		return nil, nil, apperr.Wrap(ErrNoBackend, apperr.KindIO, op,
			"the chosen backend could not be reached")
	}
	return chosen, upstream, nil
}

func (b *Balancer) run(active *session) {
	directions := make(chan struct{}, 2)
	go func() {
		defer func() { directions <- struct{}{} }()
		b.forward(active, active.client, active.upstream, false)
	}()
	go func() {
		defer func() { directions <- struct{}{} }()
		b.forward(active, active.upstream, active.client, true)
	}()
	<-directions
	<-directions
	b.halt(active)
}

func (b *Balancer) forward(active *session, from, to *transport.Conn, returning bool) {
	for {
		exchange, cancel := context.WithTimeout(b.life, b.limits.ExchangeTimeout)
		payload, err := from.Read(exchange)
		if err != nil {
			cancel()

			if returning {
				b.noteUpstreamFailure(active, err)
			}

			active.stopped.Store(true)
			_ = to.Close()
			return
		}
		written, err := to.Write(exchange, payload)
		cancel()
		if err == nil {
			b.count(active.backend, written, returning)
		}
		if err != nil {

			if !returning {
				b.noteUpstreamFailure(active, err)
			}
			active.stopped.Store(true)
			_ = to.Close()
			return
		}
	}
}

func (b *Balancer) noteUpstreamFailure(active *session, err error) {
	if active.stopped.Load() {
		return
	}
	b.recordFailure(active.backend, err, false)
}

func (b *Balancer) count(target *backend, size int, returning bool) {
	if target == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if returning {
		target.stats.Returned += size
		return
	}
	target.stats.Forwarded += size
}

func (b *Balancer) halt(active *session) {
	active.once.Do(func() {

		active.stopped.Store(true)
		_ = active.upstream.Close()
		_ = active.client.Close()
		b.mu.Lock()
		if current, running := b.sessions[active.client]; running && current == active {
			delete(b.sessions, active.client)
		}
		if active.backend != nil {
			if _, held := active.backend.sessions[active]; held {
				delete(active.backend.sessions, active)
				active.backend.active--
			}
		}
		b.mu.Unlock()
	})
}

func (b *Balancer) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()

		b.wg.Wait()
		return nil
	}
	b.closed = true
	accepting := b.accepting
	b.mu.Unlock()

	b.stop()
	var failure error
	if b.listener != nil {
		failure = b.listener.Close()
	}

	if accepting {
		<-b.serving
	}

	b.mu.Lock()
	active := make([]*session, 0, len(b.sessions))
	for _, held := range b.sessions {
		if held != nil {
			active = append(active, held)
		}
	}
	b.mu.Unlock()
	for _, held := range active {
		b.halt(held)
	}
	b.wg.Wait()
	return failure
}

type Stats struct {
	Served           int `json:"served"`
	Refused          int `json:"refused"`
	RefusedNoBackend int `json:"refused_no_backend"`

	Active int `json:"active"`

	Skipped int `json:"skipped"`

	Healthy      int `json:"healthy"`
	Draining     int `json:"draining"`
	Removed      int `json:"removed"`
	Checks       int `json:"checks"`
	FailedChecks int `json:"failed_checks"`
	Recoveries   int `json:"recoveries"`

	Drained int `json:"drained_connections"`
}

func (b *Balancer) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.statsLocked()
}

func (b *Balancer) statsLocked() Stats {
	report := Stats{
		Served:           b.served,
		Refused:          b.refused,
		RefusedNoBackend: b.refusedNoBackend,
		Active:           len(b.sessions),
	}
	for _, entry := range b.backends {
		switch {
		case entry.removed:
			report.Removed++
		case entry.healthy:
			report.Healthy++
		default:
			report.Draining++
		}
		report.Skipped += entry.stats.Skipped
		report.Checks += entry.stats.Checks
		report.FailedChecks += entry.stats.FailedChecks
		report.Recoveries += entry.stats.Recoveries
		report.Drained += entry.stats.Drained
	}
	return report
}
