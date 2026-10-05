// Package loadbalancer puts a bounded, health-aware balancer in front of a set
// of laboratory services.
//
// A resilience laboratory needs something in the middle. A fault injected on the
// path to one service has to be survivable, and the only way to show that traffic
// still arrives is to put a balancer between the client and several backends,
// take one of them away, and measure what the client sees. This package is that
// balancer: it listens for clients on the reliable transport, chooses a backend
// for each connection under a configured policy, proxies both directions,
// checks every backend on a timer, takes a backend out of service when it stops
// answering, gives the connections it already holds a bounded time to finish,
// and puts it back when it answers again.
//
// The set of backends is fixed when the balancer is built. There is no way to
// add one while it runs, and no way for anything that reaches it to ask for one:
// a balancer whose backend list could be extended by whatever arrived would be
// a way to reach any host that can answer, so registration comes from a
// configuration, only from a configuration, and every address in it is parsed as
// an address rather than resolved as a name.
//
// Everything is bounded. The number of backends, the number of connections, the
// number of connections one backend may hold, how long a health check may take,
// how long a draining connection may live and how long one direction of a
// proxied connection may block are all limits in Limits, they are checked before
// a socket is opened, and nothing a peer sends can raise any of them. Closing a
// balancer stops its listener, its health checks, every connection it proxied and
// every drain it was holding, and leaves nothing running.
//
// A proxied connection ends when either side ends, because the transport has no
// half-close: a peer that sends a fin has ended the connection, not half of it.
// A client that wants the answers to everything it sent must read them before it
// closes, and that is the transport's contract rather than a decision this
// package makes.
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

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/transport"
)

// ErrNoBackend is returned when a connection cannot be placed: every registered
// backend is draining, is already at its connection limit, or could not be
// reached. It is wrapped, so errors.Is finds it while the kind stays one the
// exit codes understand.
var ErrNoBackend = errors.New("no backend can take this connection")

// The states a backend reports. A backend is healthy until something says
// otherwise, because a balancer that serves nobody until its first check has
// finished refuses exactly the traffic that arrives first.
const (
	// StateHealthy means the backend takes new connections.
	StateHealthy = "healthy"
	// StateDraining means it takes no new connections, while the ones it already
	// holds are given the drain timeout to finish.
	StateDraining = "draining"
)

// Config describes one balancer.
type Config struct {
	// Listen is the address clients connect to. It has to be an address rather
	// than a name, for the same reason the backends do.
	Listen string `json:"listen"`
	// Backends are the addresses of the services behind the balancer, in
	// registration order. Two of them may not share an address, and the list may
	// not be empty: a balancer with no backend cannot answer, and answering
	// nothing while reporting itself ready is the one answer a client must not
	// get.
	Backends []string `json:"backends"`
	// Policy decides which backend takes the next connection. An empty policy is
	// round robin.
	Policy Policy `json:"policy"`
	// Scope is an optional prefix every backend has to be inside. A laboratory
	// knows the network it created, and a backend outside it is a host the
	// experiment has no business sending traffic to, so a balancer that is told
	// its scope refuses one rather than dialling it.
	Scope string `json:"scope,omitempty"`
	// StatusPath is where the balancer reports its state, so a laboratory can
	// wait until it is serving and read what it decided afterwards.
	StatusPath string `json:"status_path,omitempty"`
	// ControlPath is where a controller asks this balancer to change which of
	// its own backends are in service. It is read, never written, by the
	// balancer, and it can only name a backend the configuration already
	// registered. A laboratory sets it when it wants to be able to recover a
	// backend; without it this balancer serves the backends it was given and
	// ignores everything else.
	ControlPath string `json:"control_path,omitempty"`
	// Experiment names the experiment this balancer serves. A control document
	// has to name the same experiment, so a command written for one run cannot
	// reach the balancer of another. It is required when ControlPath is set.
	Experiment string `json:"experiment,omitempty"`
	// Limits bound the balancer. Zero values are filled in with working ones.
	Limits Limits `json:"limits"`
}

// Backend is what is known about one registered backend.
type Backend struct {
	// Address is the address the backend was registered with, exactly as it was
	// given.
	Address string `json:"address"`
	// State is StateHealthy, StateDraining or StateRemoved.
	State string `json:"state"`
	// Active is how many connections this backend holds now.
	Active int `json:"active"`
	// Accepted is how many connections it has taken and Skipped is how many
	// times it was passed over because it was full. A balancer that skipped
	// more than it accepted was not spreading the load, and the two numbers are
	// what says so.
	Accepted int `json:"accepted"`
	Skipped  int `json:"skipped"`
	// Checks and FailedChecks are how many health checks were taken and how
	// many of them failed. Consecutive is the run of failures and successes
	// that has not been broken yet, from checks and from live traffic together,
	// because a connection that failed is the same evidence a failed check is.
	Checks       int `json:"checks"`
	FailedChecks int `json:"failed_checks"`
	Consecutive  int `json:"consecutive_failures"`
	Recoveries   int `json:"recoveries"`
	// Forwarded and Returned are the payload bytes carried towards the backend
	// and back from it.
	Forwarded int `json:"forwarded_bytes"`
	Returned  int `json:"returned_bytes"`
	// Drained is how many connections the drain timeout closed.
	Drained int `json:"drained_connections"`
	// Latency is how long the last successful check took, LastChecked when the
	// last check was taken and LastError what the last failure was.
	Latency     time.Duration `json:"latency,omitempty"`
	LastChecked time.Time     `json:"last_checked,omitempty"`
	LastError   string        `json:"last_error,omitempty"`
	// DrainingSince is when the backend left service, and is zero while it is
	// healthy.
	DrainingSince time.Time `json:"draining_since,omitempty"`
}

// Validate checks a configuration before anything is bound.
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
		// A prefix with host bits set is refused rather than masked, and the
		// canonical form is named in the refusal so the document can be corrected.
		// A scope that was quietly reinterpreted would be a scope the operator did
		// not write, and the addresses it holds are what the balancer is allowed
		// to reach.
		if parsed != parsed.Masked() {
			return c, apperr.InvalidArgument(op, fmt.Sprintf(
				"the scope %q has host bits set; the network is %s", c.Scope, parsed.Masked()), nil)
		}
		// A default route holds every address, so a scope that is one constrains
		// nothing while looking as though it does. It is refused, as it is
		// everywhere else in this project that a scope is read.
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

// validateControl checks the control surface. A control document decides which
// backends serve, so the file it is read from is part of what the operator has
// to be able to trust: it has to be an absolute path, it cannot be the status
// file, and the balancer has to know which experiment it is serving before it
// will read anything at all.
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

// ExperimentID returns the experiment this balancer serves, or an empty string
// when it was not told, which is the case in which it reads no control document.
func (c Config) ExperimentID() string { return c.Experiment }

// parseEndpoint turns a configured address into an address and a port. A name is
// refused rather than resolved: the document that configures a balancer is
// already trusted to name every host it may reach, and a name would let what a
// name resolves to be decided after the document was validated, which is the one
// thing validation is for.
//
// anyPort is set for the address the balancer binds rather than the addresses it
// sends to. A zero port there means any port the kernel has free, which is how a
// balancer gets a port to report through Address, and it is refused for a backend
// because no service listens on port zero and a document that names one is a
// document that will never work.
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
	// A zone names an interface on the host reading the document, not a place on
	// the network: the same address with two zones is two different destinations,
	// and the scope a document gives is a prefix, which cannot say which of them it
	// means. An address in a document is therefore an address, and the interface is
	// chosen by whoever runs the balancer.
	if endpoint.Addr().Zone() != "" {
		return netip.AddrPort{}, apperr.InvalidArgument(op, fmt.Sprintf(
			"%s %q names the interface %q, which is a property of the host reading it rather than of the backend",
			what, address, endpoint.Addr().Zone()), nil)
	}
	// A broadcast address is the one address on a network that no host answers on,
	// so a document naming it describes a backend that can never be reached. It is
	// refused for the same reason the unspecified address is: neither is a host.
	if endpoint.Addr().Is4() && endpoint.Addr() == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return netip.AddrPort{}, apperr.InvalidArgument(op, fmt.Sprintf(
			"%s %q is a broadcast address, which no host answers on", what, address), nil)
	}
	return endpoint, nil
}

// backend is the balancer's own record of one registered service. Every field is
// read and written under the balancer's lock. The counters live here rather than
// in the sessions because a backend outlives the connections it held, and a
// laboratory needs to know what a backend did across the connections that came
// and went.
type backend struct {
	// address is the address the backend was registered with, and endpoint is
	// its parsed form. The parsed form is kept so nothing on a path a client can
	// make busy has to parse an address again.
	address  string
	endpoint netip.AddrPort
	// healthy is the decision the policies read. A backend starts healthy and
	// stops being healthy once failures cross the threshold.
	healthy bool
	// removed is set by a control document that took this backend out of
	// service. It is kept apart from healthy because the two are not the same
	// claim: a backend that is unhealthy has not answered, while one that is
	// removed was taken out on purpose and must stay out even when a health
	// check or a live connection says it would work. A backend that could be
	// talked back into service by a passing check could not be removed at all.
	removed bool
	// failures and successes are the runs of consecutive outcomes the thresholds
	// are compared against. Counting every failure since the last success would
	// let a backend that fails once an hour never be taken out of service, and
	// counting the other way would take one out for a single lost datagram.
	failures  int
	successes int
	// active is how many connection slots this backend holds, which is what the
	// least-connections policy and the per-backend limit both read. A slot is
	// taken when a client is placed and given back when the session ends.
	active int
	// drainUntil is when the connections this backend held when it started
	// draining will be closed. A backend that recovers forgets it, because it is
	// serving again and the connections it holds are the ones it can answer.
	drainUntil time.Time
	// sessions are the connections open to this backend, so a drain can find them
	// and the drain timeout can close them.
	sessions map[*session]struct{}
	// stats is the reported record. It is never read to make a decision.
	stats Backend
}

// Balancer serves clients and forwards them to registered backends.
type Balancer struct {
	config Config
	limits Limits

	listener *transport.Listener
	// life is the lifetime of the balancer's own work: the health checks, the
	// connections to backends and the drain deadlines all stop when it is
	// cancelled, which is what Close does.
	life   context.Context
	stop   context.CancelFunc
	closed bool
	// accepting records that the accept loop has started, and serving is closed
	// when it has returned. A Close from anywhere waits for the loop to stop
	// before it waits for the goroutines the loop starts, because a session
	// started after that wait would be one nothing is keeping track of.
	accepting bool
	serving   chan struct{}
	serveOnce sync.Once

	// backends is the registration order, which is the order the rotation walks
	// and the order a hash ring is built from. It never changes, so a policy's
	// answer for a given key is the same every time and after a restart.
	backends []*backend
	// cursor is where the next round-robin choice starts, and ring and ringFor
	// are the last ring built and the backends it was built from.
	cursor  int
	ring    []point
	ringFor []string
	// sessions are the proxied connections, keyed by the client connection. A
	// nil value is a slot reserved for a client that is still being placed,
	// which is how the connection limit counts a client from the moment it is
	// accepted rather than from the moment a backend answers.
	sessions map[*transport.Conn]*session
	// served, refused and refusedNoBackend are the balancer's own totals.
	served           int
	refused          int
	refusedNoBackend int

	// control is what the controller asked for and controlErr is the last
	// control document that could not be applied. They are read by the status
	// file so a laboratory can see that its command was refused.
	control    control
	controlErr string

	mu sync.Mutex
	// wg counts the health checks and the proxied connections, so Close can say
	// when nothing this balancer started is running any more.
	wg sync.WaitGroup
}

// session is one proxied connection: the client, the backend it was placed with
// and the connection between the balancer and that backend.
type session struct {
	client   *transport.Conn
	upstream *transport.Conn
	backend  *backend
	started  time.Time
	// stopped records that the balancer itself tore this session down. Without
	// it a session the balancer closed because a client left would be counted as
	// a backend that failed, and every client that disconnected would drain a
	// healthy backend.
	stopped atomic.Bool
	// once guards the teardown, which is reached from either direction and from
	// a drain.
	once sync.Once
}

// New builds a balancer, registers its backends and binds the socket clients
// connect to. Everything that can be refused is refused here, before a socket is
// open, so a configuration this rejects never reached the network.
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
	// The balancer's own context is the lifetime of the health checks and of
	// every connection it opens to a backend, so cancelling it stops all of them
	// at once and nothing is left reading a socket that has no owner.
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

// Address reports where the balancer is listening.
func (b *Balancer) Address() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.addressLocked()
}

// addressLocked reports the listening address with the lock already held.
func (b *Balancer) addressLocked() string {
	if b.listener == nil {
		return ""
	}
	return b.listener.LocalAddr().String()
}

// Policy reports which policy the balancer was built with.
func (b *Balancer) Policy() Policy { return b.config.Policy }

// Limits reports the limits the balancer is running under.
func (b *Balancer) Limits() Limits { return b.limits }

// Backends returns what the balancer knows about each backend, in registration
// order. It is a snapshot of copies, so a reader cannot change what the balancer
// does by looking at them.
func (b *Balancer) Backends() []Backend {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.backendsLocked()
}

// backendsLocked copies every backend's record with the lock already held.
func (b *Balancer) backendsLocked() []Backend {
	out := make([]Backend, 0, len(b.backends))
	for _, entry := range b.backends {
		out = append(out, b.snapshot(entry))
	}
	return out
}

// snapshot copies a backend's record and fills in what is decided elsewhere: the
// state, the live connection count and the run of failures.
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

// Serve accepts clients until the context is cancelled or the balancer is
// closed. It returns nil in both cases: a balancer that stopped on purpose has
// not failed, and a laboratory tearing its namespaces down must not be told that
// the balancer it asked to stop reported an error.
func (b *Balancer) Serve(ctx context.Context) error {
	// The listener watches the balancer's own context, not this one, so a
	// cancelled context becomes a stop rather than being noticed when a client
	// happens to send something.
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

// accept is the loop that reads datagrams and places the clients behind them.
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
				// The listener is over its connection limit: the peer was told,
				// the datagram was dropped, and the balancer keeps serving the
				// connections it already holds, because one client asking for
				// too much is not a reason to stop serving the others.
				b.mu.Lock()
				b.refused++
				b.mu.Unlock()
				continue
			}
			return err
		}
		if conn == nil {
			// A datagram that was not a frame this transport understands. The
			// transport dropped it and there is nothing to answer.
			continue
		}
		b.admit(conn)
	}
}

// admit places an accepted client on a backend and starts the two directions
// that carry its traffic.
func (b *Balancer) admit(client *transport.Conn) {
	key := client.RemoteAddr().String()
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		_ = client.Close()
		return
	}
	if _, running := b.sessions[client]; running {
		// The listener returns the same connection for every datagram a peer
		// sends, and this one is already being proxied. A second pair of
		// directions would answer every payload twice, which is a different
		// protocol from the one the client is speaking.
		b.mu.Unlock()
		return
	}
	if len(b.sessions) >= b.limits.MaxConnections {
		b.refused++
		b.mu.Unlock()
		_ = client.Close()
		return
	}
	// The slot is reserved before a backend is dialled, so a burst of clients
	// racing for the last one is bounded by the number the limit names.
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
	// The connection slot was taken when the backend was chosen, so it is already
	// counted; what is left is to attach the session to the backend and to count
	// the connection as taken rather than as chosen.
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

// place chooses a backend for a key and opens the connection to it. The slot on
// that backend is taken before the socket is opened and given back if it cannot
// be, so two clients cannot both be placed on a backend that has room for one.
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
		// A backend that cannot be reached is evidence, so it is recorded the
		// same way a failed check is. The client is not offered to another
		// backend: nothing has been said about the others, and quietly trying
		// each of them in turn would turn one client's failure into a scan.
		b.recordFailure(chosen, err, false)
		return nil, nil, apperr.Wrap(ErrNoBackend, apperr.KindIO, op,
			"the chosen backend could not be reached")
	}
	return chosen, upstream, nil
}

// run proxies one connection. Both directions run at once, because a client that
// sends a request and waits for the answer and a client that pipelines a
// transfer are the same client, and the balancer must not turn the second into a
// sequence of request-and-wait exchanges.
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

// forward copies payloads from one connection to the other until one of them
// stops. Every exchange is bounded by the exchange timeout, so a peer that
// accepts a payload and never answers costs a timeout rather than a goroutine for
// ever, and every payload is counted so a laboratory can say how much a backend
// actually carried.
func (b *Balancer) forward(active *session, from, to *transport.Conn, returning bool) {
	for {
		exchange, cancel := context.WithTimeout(b.life, b.limits.ExchangeTimeout)
		payload, err := from.Read(exchange)
		if err != nil {
			cancel()
			// A read that failed on the connection to a backend is evidence that
			// the backend is not serving. A read that failed on the connection to
			// a client is not, and a failure this balancer caused by closing the
			// connection itself is not either.
			if returning {
				b.noteUpstreamFailure(active, err)
			}
			// The session is marked as torn down before the other connection is
			// closed. The other direction is reading or writing on it, and what
			// it sees when this close reaches it is a consequence of this teardown
			// rather than anything about a backend, which is a difference worth
			// one store: without it every client that disconnected took a healthy
			// backend out of service.
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
			// A write that failed towards a backend is the same evidence. A
			// write that failed towards a client says something about the client.
			if !returning {
				b.noteUpstreamFailure(active, err)
			}
			active.stopped.Store(true)
			_ = to.Close()
			return
		}
	}
}

// noteUpstreamFailure records a failure against the session's backend, unless the
// balancer closed the connection itself.
func (b *Balancer) noteUpstreamFailure(active *session, err error) {
	if active.stopped.Load() {
		return
	}
	b.recordFailure(active.backend, err, false)
}

// count records the payload carried in one direction.
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

// halt takes one session down: both connections are closed, the backend's
// accounting is corrected and the client is forgotten. It is idempotent, because
// either direction or a drain may reach it first.
func (b *Balancer) halt(active *session) {
	active.once.Do(func() {
		// The flag is set before anything is closed, so a direction that wakes
		// with an error knows the error is this teardown and not a backend that
		// failed.
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

// Close stops the balancer: the listener, the health checks, every proxied
// connection and every drain it was holding. It is idempotent, and when it
// returns nothing this balancer started is still running.
func (b *Balancer) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		// The work still has to be waited for, because a Close from the loop
		// that is stopping must not return before that loop has.
		b.wg.Wait()
		return nil
	}
	b.closed = true
	accepting := b.accepting
	b.mu.Unlock()

	// The context is cancelled before the listener is closed, so the connections
	// to backends, which the transport holds as children of it, stop before
	// anything waits for the goroutines using them.
	b.stop()
	var failure error
	if b.listener != nil {
		failure = b.listener.Close()
	}
	// The accept loop is waited for before the sessions, because it is the only
	// thing that starts them and a session started after this would be one
	// nothing knows about.
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

// Stats is what the balancer measured since it was built.
type Stats struct {
	// Served is how many connections were proxied. Refused is how many clients
	// were turned away, by the connection limit or because no backend could take
	// them, and RefusedNoBackend is the second of those on its own, because a
	// client refused for want of a backend and a client refused for want of a
	// slot are different problems.
	Served           int `json:"served"`
	Refused          int `json:"refused"`
	RefusedNoBackend int `json:"refused_no_backend"`
	// Active is how many connections are being proxied now.
	Active int `json:"active"`
	// Skipped is how many times a backend was passed over because it was at its
	// own connection limit. A balancer that skips is a balancer that could not
	// spread what it was given.
	Skipped int `json:"skipped"`
	// Healthy, Draining and Removed count the backends in each state, and
	// Checks, FailedChecks and Recoveries are the health checks taken, the ones
	// that failed and the backends put back into service. A removed backend is
	// counted apart from a draining one because only a control document can put
	// it back.
	Healthy      int `json:"healthy"`
	Draining     int `json:"draining"`
	Removed      int `json:"removed"`
	Checks       int `json:"checks"`
	FailedChecks int `json:"failed_checks"`
	Recoveries   int `json:"recoveries"`
	// Drained is how many connections the drain timeout closed.
	Drained int `json:"drained_connections"`
}

// Stats returns what the balancer has measured.
func (b *Balancer) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.statsLocked()
}

// statsLocked builds the report with the lock already held.
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
