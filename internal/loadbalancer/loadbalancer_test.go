package loadbalancer

import (
	"context"
	"net"
	"net/netip"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/transport"
)

// testLimits are the limits these tests run with: intervals and timeouts short
// enough that a timeout is something a test waits for rather than something it
// hopes does not happen, and windows small enough that a bound is a handful of
// segments.
func testLimits() Limits {
	limits := DefaultLimits()
	limits.HealthInterval = 100 * time.Millisecond
	limits.HealthTimeout = 60 * time.Millisecond
	limits.FailureThreshold = 2
	limits.RecoveryThreshold = 2
	limits.DrainTimeout = time.Second
	limits.ExchangeTimeout = 5 * time.Second
	limits.Transport.MaxPayload = 256
	limits.Transport.MaxWindow = 8
	limits.Transport.MaxReorder = 8
	limits.Transport.RetransmitTimeout = 60 * time.Millisecond
	limits.Transport.MaxRetransmits = 6
	limits.Transport.IdleTimeout = 5 * time.Second
	limits.Transport.MaxRTT = 400 * time.Millisecond
	return limits
}

// testConfig is a balancer on an unused local port with the given backends and
// the given policy.
func testConfig(policy Policy, backends ...string) Config {
	return Config{
		Listen:   "127.0.0.1:0",
		Backends: backends,
		Policy:   policy,
		Limits:   testLimits(),
	}
}

// startBackend runs a real backend: a listener on the reliable transport that
// answers every payload with its own name in front of it.
//
// The name is what distribution is measured with, because a client cannot tell
// two backends apart otherwise, and being able to stop one is what failure and
// recovery are measured with. Nothing here is a stub: the balancer talks to this
// over a real socket, so what the balancer does to a backend is what it does to a
// service.
type backendServer struct {
	name    string
	address string
	// silent is a backend that accepts payloads and answers nothing, which is
	// the case a health check has to be able to tell from a closed port.
	silent bool

	listener *transport.Listener
	cancel   context.CancelFunc
	done     chan struct{}
	// conns are the connections being echoed, so stopping the backend closes
	// them rather than leaving the balancer talking to a socket nobody reads.
	conns map[string]*transport.Conn
	mu    sync.Mutex
}

// startBackend runs a backend that answers, stopped with the test.
func startBackend(t *testing.T, name string) *backendServer {
	t.Helper()
	server := newBackend(t, name, false)
	t.Cleanup(server.stop)
	return server
}

// startSilentBackend runs a backend that never answers. A port with nothing
// behind it is refused by the kernel, which is not the failure a datagram
// transport sees: a host that is switched off still accepts a datagram and never
// replies, so this is the case health checks exist for.
func startSilentBackend(t *testing.T, name string) *backendServer {
	t.Helper()
	server := newBackend(t, name, true)
	t.Cleanup(server.stop)
	return server
}

// newBackend binds a backend and runs its read loop. The address is kept so a
// stopped backend can be started again on the same one, which is what recovery
// is measured with.
func newBackend(t *testing.T, name string, silent bool) *backendServer {
	t.Helper()
	limits := testLimits().Transport
	ctx, cancel := context.WithCancel(context.Background())
	listener, err := transport.Listen(ctx, "127.0.0.1:0", limits)
	if err != nil {
		cancel()
		t.Fatalf("a backend could not listen: %v", err)
	}
	server := &backendServer{
		name:     name,
		address:  listener.LocalAddr().String(),
		silent:   silent,
		listener: listener,
		cancel:   cancel,
		done:     make(chan struct{}),
		conns:    make(map[string]*transport.Conn),
	}
	go server.serve()
	return server
}

// serve reads datagrams and echoes what they carry, until the backend is stopped.
func (s *backendServer) serve() {
	defer close(s.done)
	for {
		conn, err := s.listener.ServePeer(context.Background())
		if err != nil {
			// A peer over the connection limit is this backend's problem to keep
			// serving past, but a closed or cancelled socket means the backend is
			// stopped and the loop is finished.
			if apperr.IsKind(err, apperr.KindLimitExceeded) {
				continue
			}
			return
		}
		if conn == nil {
			continue
		}
		s.admit(conn)
	}
}

// admit starts one echo per peer. The listener returns the same connection for
// every datagram a peer sends, so without this a peer that sent more than one
// payload would be answered by a reader per payload.
func (s *backendServer) admit(conn *transport.Conn) {
	key := conn.RemoteAddr().String()
	s.mu.Lock()
	if _, running := s.conns[key]; running {
		s.mu.Unlock()
		return
	}
	s.conns[key] = conn
	s.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for {
			payload, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if s.silent {
				continue
			}
			answer := append([]byte(s.name+"|"), payload...)
			if _, err := conn.Write(ctx, answer); err != nil {
				return
			}
		}
	}()
}

// stop takes the backend away: its socket is closed and every connection it held
// is closed with it, which is what stopping a service in a laboratory does.
func (s *backendServer) stop() {
	s.cancel()
	_ = s.listener.Close()
	<-s.done
}

// restart binds the same address again and serves from it, so a backend that was
// taken away comes back where it was rather than somewhere new.
func (s *backendServer) restart(t *testing.T) {
	t.Helper()
	limits := testLimits().Transport
	ctx, cancel := context.WithCancel(context.Background())
	listener, err := transport.Listen(ctx, s.address, limits)
	if err != nil {
		cancel()
		t.Fatalf("the backend could not listen on %s again: %v", s.address, err)
	}
	s.mu.Lock()
	s.listener = listener
	s.cancel = cancel
	s.conns = make(map[string]*transport.Conn)
	s.mu.Unlock()
	s.done = make(chan struct{})
	go s.serve()
	t.Cleanup(s.stop)
}

// startBalancer builds a balancer and serves it until the test ends.
func startBalancer(t *testing.T, config Config) *Balancer {
	t.Helper()
	balancer, err := New(config)
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- balancer.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := balancer.Close(); err != nil {
			t.Errorf("Close() = %v, want nil", err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve() = %v, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve() did not return within five seconds of the balancer being closed")
		}
	})
	return balancer
}

// dial opens a client connection through the balancer and stops it with the test.
func dial(t *testing.T, balancer *Balancer) *transport.Conn {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := transport.Dial(ctx, "127.0.0.1:0", balancer.Address(), testLimits().Transport)
	if err != nil {
		cancel()
		t.Fatalf("a client could not reach the balancer on %s: %v", balancer.Address(), err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		cancel()
	})
	return conn
}

// roundTrip sends one payload through the balancer and returns the answer,
// including the name of the backend that produced it.
func roundTrip(t *testing.T, conn *transport.Conn, payload string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := conn.Write(ctx, []byte(payload)); err != nil {
		t.Fatalf("Write(%q) = %v, want nil", payload, err)
	}
	answer, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("Read() = %v, want an answer to %q", err, payload)
	}
	return string(answer)
}

// servedBy sends one payload on a fresh connection and reports which backend
// answered, which is how a test says which backend a client was placed with.
func servedBy(t *testing.T, balancer *Balancer, payload string) string {
	t.Helper()
	conn := dial(t, balancer)
	answer := roundTrip(t, conn, payload)
	name, _, found := strings.Cut(answer, "|")
	if !found {
		t.Fatalf("the answer %q does not name a backend", answer)
	}
	return name
}

// eventually waits for a condition a test has to observe rather than assume, and
// reports it as a failure rather than hanging when the balancer never gets there.
func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the balancer never reported %s within ten seconds", what)
}

// stateOf reports a backend's state, or the empty string when it is not
// registered, which is what a test comparing before and after wants.
func stateOf(balancer *Balancer, address string) string {
	for _, backend := range balancer.Backends() {
		if backend.Address == address {
			return backend.State
		}
	}
	return ""
}

// mustAddr resolves an address a test just had the kernel choose.
func mustAddr(t *testing.T, address string) *net.UDPAddr {
	t.Helper()
	resolved, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		t.Fatalf("ResolveUDPAddr(%q) = %v, want nil", address, err)
	}
	return resolved
}

// holdConnection reports connections a backend is already holding, as if a
// session the balancer placed earlier were still running. Which backend a client
// is given is the balancer's own decision, so a test that needs a particular
// backend to be full sets that state up rather than trying to arrange it from
// outside.
func holdConnection(balancer *Balancer, index, count int) {
	balancer.mu.Lock()
	defer balancer.mu.Unlock()
	balancer.backends[index].active += count
}

// A payload has to cross two real connections and come back intact, which is the
// one thing a test of the balancer's decisions cannot say.
func TestAPayloadCrossesTheBalancerAndComesBack(t *testing.T) {
	t.Parallel()
	backends := []*backendServer{startBackend(t, "a"), startBackend(t, "b")}
	balancer := startBalancer(t, testConfig(PolicyRoundRobin, backends[0].address, backends[1].address))

	conn := dial(t, balancer)
	answer := roundTrip(t, conn, "and back again")
	if !strings.HasPrefix(answer, "a|") && !strings.HasPrefix(answer, "b|") {
		t.Fatalf("Read() = %q, want an answer from a registered backend", answer)
	}
	if !strings.HasSuffix(answer, "|and back again") {
		t.Errorf("Read() = %q, want the payload to come back unchanged after the backend's name", answer)
	}
}

// A transfer larger than one window has to survive the proxy in both directions,
// with everything that comes back arriving in the order it was sent. Both halves
// run at once because the windows on either side are bounded: a client that wrote
// a whole transfer before reading any of it would fill its own receive window and
// wait for itself, so this is a client that keeps draining while it sends.
func TestATransferLargerThanOneWindowSurvivesTheProxy(t *testing.T) {
	t.Parallel()
	backend := startBackend(t, "a")
	balancer := startBalancer(t, testConfig(PolicyRoundRobin, backend.address))

	conn := dial(t, balancer)
	limits := testLimits().Transport
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const messages = 64
	// A payload of almost the largest a segment can carry, so the transfer is
	// larger than one window carries several times over.
	payload := strings.Repeat("p", limits.MaxPayload-8)

	answers := make(chan string, messages)
	readErrors := make(chan error, 1)
	go func() {
		for range messages {
			answer, err := conn.Read(ctx)
			if err != nil {
				readErrors <- err
				return
			}
			answers <- string(answer)
		}
	}()

	for index := range messages {
		if _, err := conn.Write(ctx, []byte(payload)); err != nil {
			t.Fatalf("Write of message %d = %v, want nil", index, err)
		}
	}
	for index := range messages {
		select {
		case answer := <-answers:
			if want := "a|" + payload; answer != want {
				t.Fatalf("message %d came back as %q, want the payload unchanged behind the backend's name", index, answer)
			}
		case err := <-readErrors:
			t.Fatalf("Read of message %d = %v, want the whole transfer to come back", index, err)
		case <-ctx.Done():
			t.Fatalf("the transfer stalled after %d of %d messages", index, messages)
		}
	}
}

// The backends are registered in the order they were given, and that order is
// what the round robin walks, so a laboratory that reads the report knows which
// backend the first connection went to.
func TestBackendsAreRegisteredInTheOrderTheyWereGiven(t *testing.T) {
	t.Parallel()
	addresses := []string{"127.0.0.1:9101", "127.0.0.1:9102", "127.0.0.1:9103"}
	balancer, err := New(testConfig(PolicyRoundRobin, addresses...))
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	t.Cleanup(func() { _ = balancer.Close() })

	registered := balancer.Backends()
	if len(registered) != len(addresses) {
		t.Fatalf("registered %d backends, want %d", len(registered), len(addresses))
	}
	for index, backend := range registered {
		if backend.Address != addresses[index] {
			t.Errorf("backend %d is %s, want %s", index, backend.Address, addresses[index])
		}
		// A backend is serving until something says otherwise, or a balancer
		// refuses exactly the traffic that arrives before its first check.
		if backend.State != StateHealthy {
			t.Errorf("backend %d is %s before anything has failed, want %s", index, backend.State, StateHealthy)
		}
		if backend.Active != 0 {
			t.Errorf("backend %d holds %d connections before any client arrived, want 0", index, backend.Active)
		}
	}
}

// A balancer with no backend cannot answer, and one that registered itself as
// ready while answering nothing is the answer a laboratory must not be given.
func TestARegistrationWithoutABackendIsRefused(t *testing.T) {
	t.Parallel()
	if _, err := New(testConfig(PolicyRoundRobin)); err == nil {
		t.Error("New() with no backends = nil, want a refusal")
	}
}

// A backend registered twice is two shares of one server, so the balancer would
// report a distribution it did not have.
func TestABackendRegisteredTwiceIsRefused(t *testing.T) {
	t.Parallel()
	config := testConfig(PolicyRoundRobin, "127.0.0.1:9101", "127.0.0.1:9101")
	if _, err := New(config); err == nil {
		t.Error("New() with a repeated backend = nil, want a refusal")
	}
}

// The number of backends is bounded, and a configuration that asks for more than
// the limit is refused before a socket is open rather than being served until it
// runs out of memory.
func TestABackendCountAboveTheLimitIsRefused(t *testing.T) {
	t.Parallel()
	addresses := make([]string, 0, 8)
	for index := range 8 {
		addresses = append(addresses, "127.0.0.1:"+strconv.Itoa(9200+index))
	}
	config := testConfig(PolicyRoundRobin, addresses...)
	config.Limits.MaxBackends = 4
	if _, err := New(config); err == nil {
		t.Fatal("New() with 8 backends and a limit of 4 = nil, want a refusal")
	} else if !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Errorf("New() = %v, want a refusal for exceeding a limit", err)
	}
	// The same configuration under the limit is accepted, so the refusal is the
	// limit working and not the addresses being unusable.
	config.Limits.MaxBackends = 8
	if _, err := New(config); err != nil {
		t.Errorf("New() with 8 backends and a limit of 8 = %v, want nil", err)
	}
}

// A backend that is listening is reachable from inside the scope the balancer
// was given and a host outside it is not. The scope is checked against the
// address rather than against what it resolves to, so a document cannot name a
// laboratory network and send the traffic somewhere else.
func TestABackendOutsideTheScopeIsRefused(t *testing.T) {
	t.Parallel()
	config := testConfig(PolicyRoundRobin, "10.57.0.2:7000")
	config.Scope = "10.42.0.0/24"
	if _, err := New(config); err == nil {
		t.Fatal("New() with a backend outside the scope = nil, want a refusal")
	}
	config.Backends = []string{"10.42.0.6:7000"}
	if _, err := New(config); err != nil {
		t.Errorf("New() with a backend inside the scope = %v, want nil", err)
	}
}

// A client that has been placed may not be moved to another backend, and a
// balancer that reports a connection against the wrong backend is worse than one
// that reports nothing.
func TestAConnectionIsCountedAgainstTheBackendThatServedIt(t *testing.T) {
	t.Parallel()
	backends := []*backendServer{startBackend(t, "a"), startBackend(t, "b")}
	balancer := startBalancer(t, testConfig(PolicyRoundRobin, backends[0].address, backends[1].address))

	conn := dial(t, balancer)
	answer := roundTrip(t, conn, "where did this go")
	if name, _, _ := strings.Cut(answer, "|"); name == "" {
		t.Fatalf("the answer %q named no backend", answer)
	}

	report := balancer.Backends()
	if got := stateOf(balancer, backends[0].address); got != StateHealthy {
		t.Errorf("backend a is %s after serving one client, want %s", got, StateHealthy)
	}
	if stats := balancer.Stats(); stats.Served != 1 {
		t.Errorf("served = %d, want 1", stats.Served)
	}
	// Whichever backend answered is the one that now holds the connection, and
	// the other holds none.
	served, idle := 0, 0
	for _, backend := range report {
		switch backend.Address {
		case backends[0].address, backends[1].address:
			if backend.Active == 1 {
				served++
			}
			if backend.Active == 0 {
				idle++
			}
		}
	}
	if served != 1 || idle != 1 {
		t.Errorf("backends reported %d holding the connection and %d holding none, want 1 and 1: %+v",
			served, idle, report)
	}
}

// The connection limit is enforced by the balancer and not only by the transport
// underneath it, so a client over the limit is turned away rather than being
// given a socket this balancer has no room to serve.
func TestTheConnectionLimitIsEnforced(t *testing.T) {
	t.Parallel()
	backend := startBackend(t, "a")
	config := testConfig(PolicyRoundRobin, backend.address)
	// One connection in total and one on the backend: a balancer allowed fewer
	// connections in total than on one backend would describe a fleet that cannot
	// exist, and a document that lowers the total lowers this with it.
	config.Limits.MaxConnections = 1
	config.Limits.MaxBackendConnections = 1
	balancer := startBalancer(t, config)

	first := dial(t, balancer)
	roundTrip(t, first, "the one that fits")

	second := dial(t, balancer)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := second.Write(ctx, []byte("one too many")); err == nil {
		if _, err := second.Read(ctx); err == nil {
			t.Error("a client over the connection limit was served, want a refusal")
		}
	}
	eventually(t, "the refused client", func() bool { return balancer.Stats().Refused > 0 })
	if stats := balancer.Stats(); stats.Served != 1 {
		t.Errorf("served = %d, want 1: the second client must not have been placed", stats.Served)
	}
}

// A backend that is already holding the limit is passed over and the client is
// served by another one, because a balancer that refused a client whenever the
// backend the round robin reached first happened to be full would be refusing
// traffic it has room for.
func TestABackendAtItsOwnLimitIsPassedOver(t *testing.T) {
	t.Parallel()
	backends := []*backendServer{startBackend(t, "a"), startBackend(t, "b")}
	config := testConfig(PolicyRoundRobin, backends[0].address, backends[1].address)
	config.Limits.MaxBackendConnections = 1
	config.Limits.MaxConnections = 8
	balancer := startBalancer(t, config)

	// The first backend is given the one connection it allows, held by a session
	// that is not this test's to place: which backend a client gets is the
	// balancer's decision to make, so the state that has to be set up is set
	// directly.
	holdConnection(balancer, 0, 1)

	if served := servedBy(t, balancer, "and one more"); served != "b" {
		t.Errorf("the client was served by backend %q, want %q: the full backend must be passed over", served, "b")
	}
	if stats := balancer.Stats(); stats.Skipped == 0 {
		t.Error("skipped = 0, want the pass-over counted so a laboratory can see it")
	}
	if stats := balancer.Stats(); stats.Refused != 0 {
		t.Errorf("refused = %d, want 0: a client another backend can take is not refused", stats.Refused)
	}
	if stats := balancer.Stats(); stats.Served != 1 {
		t.Errorf("served = %d, want 1", stats.Served)
	}
}

// Nothing that reaches the balancer can add a backend to it, and nothing that
// reaches it can change one. The backend set comes from a configuration, and the
// socket a client connects to is a data path rather than a control path.
func TestNothingThatReachesTheBalancerCanRegisterABackend(t *testing.T) {
	t.Parallel()
	backends := []*backendServer{startBackend(t, "a"), startBackend(t, "b")}
	balancer := startBalancer(t, testConfig(PolicyRoundRobin, backends[0].address, backends[1].address))
	before := balancer.Backends()

	// A document naming a backend of its own, a transport frame built by hand,
	// and a frame that looks like a probe of the balancer itself. The socket is
	// unbound because the address under test is the balancer's.
	socket, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("a client socket could not be opened: %v", err)
	}
	defer func() { _ = socket.Close() }()
	for _, datagram := range [][]byte{
		[]byte(`{"backends":["127.0.0.1:9999"],"policy":"consistent_hash"}`),
		{0x4e, 0x46, 0x01, 0x02, 0, 0, 0, 1, 0, 0, 0, 0, 0, 20},
		{0x4e, 0x46, 0x01, 0x04, 0, 0, 0, 0, 0, 0, 0, 0, 0, 20},
		[]byte("netforge.health.v1"),
	} {
		if _, err := socket.WriteTo(datagram, mustAddr(t, balancer.Address())); err != nil {
			t.Fatalf("WriteTo() = %v, want nil", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	after := balancer.Backends()
	if len(after) != len(before) {
		t.Fatalf("the balancer now holds %d backends, want the %d it was built with", len(after), len(before))
	}
	for index := range after {
		if after[index].Address != before[index].Address {
			t.Errorf("backend %d is %s, want the %s it was registered with",
				index, after[index].Address, before[index].Address)
		}
	}
	// It is still serving the clients it registered, which is what says the
	// datagrams above were dropped rather than interpreted.
	if servedBy(t, balancer, "still serving") == "" {
		t.Error("the balancer stopped serving after being sent a registration")
	}
}

// A balancer that is closed closes its listener, its health checks and every
// connection it proxied, and leaves nothing running. It is the one test in the
// package that does not run in parallel with the others, because a goroutine
// count taken while other tests are opening connections cannot tell a leak from
// a neighbour.
func TestAClosedBalancerLeavesNothingRunning(t *testing.T) {
	backend := startBackend(t, "a")
	balancer, err := New(testConfig(PolicyRoundRobin, backend.address))
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- balancer.Serve(ctx) }()

	// Measured after the first connection has been closed once, so the one-time
	// runtime and package initialisation is not counted as a leak.
	conn := dial(t, balancer)
	roundTrip(t, conn, "one connection")
	_ = conn.Close()
	before := runtime.NumGoroutine()

	for index := range 8 {
		held := dial(t, balancer)
		roundTrip(t, held, "payload in flight")
		if index%2 == 0 {
			if err := held.Close(); err != nil {
				t.Fatalf("Close() = %v, want nil", err)
			}
		}
	}
	if err := balancer.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve() = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve() did not return within five seconds of the balancer being closed")
	}
	// Closing a connection makes its reader goroutine notice on its next
	// scheduling pass rather than inside the call that closed it, so the count is
	// given a moment to settle before it is called a leak. A balancer that left
	// something running would not settle, which is the property being measured.
	settled, after := false, 0
	for range 200 {
		after = runtime.NumGoroutine()
		if after <= before {
			settled = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !settled {
		t.Errorf("goroutines went from %d to %d and stayed there, want the balancer to leave "+
			"nothing running", before, after)
	}
}

// Closing is idempotent, and a balancer that was closed before it was served can
// still be closed, because a laboratory tears its namespaces down in whatever
// order the teardown stack gives it.
func TestClosingIsSafeFromEitherDirection(t *testing.T) {
	t.Parallel()
	backend := startBackend(t, "a")
	balancer, err := New(testConfig(PolicyRoundRobin, backend.address))
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	if err := balancer.Close(); err != nil {
		t.Fatalf("Close() of a balancer that was never served = %v, want nil", err)
	}
	if err := balancer.Close(); err != nil {
		t.Fatalf("Close() again = %v, want nil: closing twice is safe", err)
	}
	// Serving a balancer that has been closed is refused rather than opening a
	// socket nothing will ever close.
	if err := balancer.Serve(context.Background()); err != nil {
		t.Errorf("Serve() after Close() = %v, want nil: a closed balancer simply stops", err)
	}
}

// A cancelled context stops the balancer and is not reported as a failure, or a
// laboratory tearing its namespaces down would be told that the balancer it asked
// to stop went wrong.
func TestACancelledContextStopsTheBalancerWithoutAnError(t *testing.T) {
	t.Parallel()
	backend := startBackend(t, "a")
	balancer, err := New(testConfig(PolicyRoundRobin, backend.address))
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- balancer.Serve(ctx) }()
	conn := dial(t, balancer)
	roundTrip(t, conn, "before the stop")
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve() = %v, want nil for a balancer that was asked to stop", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve() did not return within five seconds of its context being cancelled")
	}
	if err := balancer.Close(); err != nil {
		t.Errorf("Close() after the context was cancelled = %v, want nil", err)
	}
}

// contextTODO is the context an exchange in a test that is only establishing a
// session is written with, where the failure it is waiting for is the point.
func contextTODO() context.Context { return context.Background() }

// contextWithTimeout is a deadline for a read whose failure is the thing under
// test.
func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// splitPort reads an address the balancer reported, for a test that has to say
// which port the kernel chose.
func splitPort(address string) (string, uint16, error) {
	endpoint, err := netip.ParseAddrPort(address)
	if err != nil {
		return "", 0, err
	}
	return endpoint.Addr().String(), endpoint.Port(), nil
}

// servedByAt is a client that connects to an address a report gave, which is how a
// laboratory finds a balancer that asked the kernel for a port.
func servedByAt(t *testing.T, address, payload string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := transport.Dial(ctx, "127.0.0.1:0", address, testLimits().Transport)
	if err != nil {
		cancel()
		t.Fatalf("a client could not reach %s: %v", address, err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		cancel()
	})
	answer := roundTrip(t, conn, payload)
	name, _, found := strings.Cut(answer, "|")
	if !found {
		t.Fatalf("the answer %q does not name a backend", answer)
	}
	return name
}
