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

func testConfig(policy Policy, backends ...string) Config {
	return Config{
		Listen:   "127.0.0.1:0",
		Backends: backends,
		Policy:   policy,
		Limits:   testLimits(),
	}
}

type backendServer struct {
	name    string
	address string

	silent bool

	listener *transport.Listener
	cancel   context.CancelFunc
	done     chan struct{}

	conns map[string]*transport.Conn
	mu    sync.Mutex
}

func startBackend(t *testing.T, name string) *backendServer {
	t.Helper()
	server := newBackend(t, name, false)
	t.Cleanup(server.stop)
	return server
}

func startSilentBackend(t *testing.T, name string) *backendServer {
	t.Helper()
	server := newBackend(t, name, true)
	t.Cleanup(server.stop)
	return server
}

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

func (s *backendServer) serve() {
	defer close(s.done)
	for {
		conn, err := s.listener.ServePeer(context.Background())
		if err != nil {

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

func (s *backendServer) stop() {
	s.cancel()
	_ = s.listener.Close()
	<-s.done
}

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

func stateOf(balancer *Balancer, address string) string {
	for _, backend := range balancer.Backends() {
		if backend.Address == address {
			return backend.State
		}
	}
	return ""
}

func mustAddr(t *testing.T, address string) *net.UDPAddr {
	t.Helper()
	resolved, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		t.Fatalf("ResolveUDPAddr(%q) = %v, want nil", address, err)
	}
	return resolved
}

func holdConnection(balancer *Balancer, index, count int) {
	balancer.mu.Lock()
	defer balancer.mu.Unlock()
	balancer.backends[index].active += count
}

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

func TestATransferLargerThanOneWindowSurvivesTheProxy(t *testing.T) {
	t.Parallel()
	backend := startBackend(t, "a")
	balancer := startBalancer(t, testConfig(PolicyRoundRobin, backend.address))

	conn := dial(t, balancer)
	limits := testLimits().Transport
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const messages = 64

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

		if backend.State != StateHealthy {
			t.Errorf("backend %d is %s before anything has failed, want %s", index, backend.State, StateHealthy)
		}
		if backend.Active != 0 {
			t.Errorf("backend %d holds %d connections before any client arrived, want 0", index, backend.Active)
		}
	}
}

func TestARegistrationWithoutABackendIsRefused(t *testing.T) {
	t.Parallel()
	if _, err := New(testConfig(PolicyRoundRobin)); err == nil {
		t.Error("New() with no backends = nil, want a refusal")
	}
}

func TestABackendRegisteredTwiceIsRefused(t *testing.T) {
	t.Parallel()
	config := testConfig(PolicyRoundRobin, "127.0.0.1:9101", "127.0.0.1:9101")
	if _, err := New(config); err == nil {
		t.Error("New() with a repeated backend = nil, want a refusal")
	}
}

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

	config.Limits.MaxBackends = 8
	if _, err := New(config); err != nil {
		t.Errorf("New() with 8 backends and a limit of 8 = %v, want nil", err)
	}
}

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

func TestTheConnectionLimitIsEnforced(t *testing.T) {
	t.Parallel()
	backend := startBackend(t, "a")
	config := testConfig(PolicyRoundRobin, backend.address)

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

func TestABackendAtItsOwnLimitIsPassedOver(t *testing.T) {
	t.Parallel()
	backends := []*backendServer{startBackend(t, "a"), startBackend(t, "b")}
	config := testConfig(PolicyRoundRobin, backends[0].address, backends[1].address)
	config.Limits.MaxBackendConnections = 1
	config.Limits.MaxConnections = 8
	balancer := startBalancer(t, config)

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

func TestNothingThatReachesTheBalancerCanRegisterABackend(t *testing.T) {
	t.Parallel()
	backends := []*backendServer{startBackend(t, "a"), startBackend(t, "b")}
	balancer := startBalancer(t, testConfig(PolicyRoundRobin, backends[0].address, backends[1].address))
	before := balancer.Backends()

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

	if servedBy(t, balancer, "still serving") == "" {
		t.Error("the balancer stopped serving after being sent a registration")
	}
}

func TestAClosedBalancerLeavesNothingRunning(t *testing.T) {
	backend := startBackend(t, "a")
	balancer, err := New(testConfig(PolicyRoundRobin, backend.address))
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- balancer.Serve(ctx) }()

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

	if err := balancer.Serve(context.Background()); err != nil {
		t.Errorf("Serve() after Close() = %v, want nil: a closed balancer simply stops", err)
	}
}

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

func contextTODO() context.Context { return context.Background() }

func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

func splitPort(address string) (string, uint16, error) {
	endpoint, err := netip.ParseAddrPort(address)
	if err != nil {
		return "", 0, err
	}
	return endpoint.Addr().String(), endpoint.Port(), nil
}

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
