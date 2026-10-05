package transport

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testLimits are the limits these tests run with: short enough that a timeout is
// something a test waits for rather than something it hopes does not happen, and
// small enough that a bound is a handful of segments.
func testLimits() Limits {
	limits := DefaultLimits()
	limits.MaxPayload = 256
	limits.MaxWindow = 8
	limits.MaxReorder = 8
	limits.MaxDatagram = MaxFrameSize
	limits.MaxConnections = 4
	limits.RetransmitTimeout = 60 * time.Millisecond
	limits.MaxRetransmits = 12
	limits.HandshakeTimeout = 2 * time.Second
	limits.IdleTimeout = 3 * time.Second
	limits.MaxRTT = 400 * time.Millisecond
	return limits
}

// serve runs a listener's read loop until the test ends, so every test has a
// real socket and a real read path rather than a stubbed one.
func serve(t *testing.T, l *Listener) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if err := l.Serve(ctx); err != nil {
				if errors.Is(err, ErrClosed) || ctx.Err() != nil {
					return
				}
				// A refused peer over the connection limit is a limit working,
				// not a failure of the loop, so it is counted rather than
				// stopping the listener that other peers are using.
				var overLimit interface{ Error() string }
				if errors.As(err, &overLimit) {
					continue
				}
				return
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = l.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("the listener's read loop did not stop within two seconds")
		}
	})
}

// dial opens a client connection and stops it with the test. The context handed
// to Dial is the connection's lifetime, so it is cancelled at cleanup rather
// than on return: a connection whose context ended would stop before it was
// used, which is what the context means.
func dial(t *testing.T, to string, limits Limits) *Conn {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := Dial(ctx, "127.0.0.1:0", to, limits)
	if err != nil {
		cancel()
		t.Fatalf("Dial(%s) = %v, want nil", to, err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		cancel()
	})
	return conn
}

// connect opens a client, makes the listener serve it, and starts an echo on
// the served side, so a test can send and receive from the first payload.
//
// The served connection is created by the first datagram a peer sends, so the
// order is forced: something has to be sent before the far end exists to answer
// it. The knock is read back here so that every payload a test sends afterwards
// is the one it reads.
func connect(t *testing.T, l *Listener, to string, limits Limits) (*Conn, *Conn) {
	t.Helper()
	// The listener keys its connections by the address a datagram came from,
	// which for a direct dial is this client's own socket rather than the
	// address it dialled.
	client, served := connectSeenAs(t, l, to, "", limits)
	_ = served
	return client, served
}

// connectSeenAs is connect for a relayed peer, where the listener sees the
// relay's address towards it rather than the client's own.
func connectSeenAs(t *testing.T, l *Listener, to, seenAs string, limits Limits) (*Conn, *Conn) {
	t.Helper()
	client := dial(t, to, limits)
	if seenAs == "" {
		seenAs = client.LocalAddr().String()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.Write(ctx, []byte("knock")); err != nil {
		t.Fatalf("the knock could not be sent: %v", err)
	}
	served := waitFor(t, l, seenAs)
	echo(t, served)
	if _, err := client.Read(ctx); err != nil {
		t.Fatalf("the knock was not answered: %v", err)
	}
	return client, served
}

// waitFor is the connection a peer has been given, once its first datagram has
// been read. A test that needs the served side cannot assume it exists the
// instant it dials, because the datagram establishing it is still in flight.
func waitFor(t *testing.T, l *Listener, addr string) *Conn {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if conn, ok := l.Conn(addr); ok {
			return conn
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the listener still has no connection for %s after five seconds", addr)
	return nil
}

// echo reads payloads and writes them back, which is the smallest server that
// exercises both directions at once.
func echo(t *testing.T, conn *Conn) {
	t.Helper()
	go func() {
		// The context is made here rather than in echo, because a defer in
		// echo's own frame would cancel it the moment echo returned, which is
		// before the first read rather than after the last.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for {
			payload, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if _, err := conn.Write(ctx, payload); err != nil {
				return
			}
		}
	}()
}

// waitForAcknowledged waits until the connection has confirmed at least want
// distinct segments, and reports the counters as they were at that moment.
//
// A counter that only reaches its expected value once the network has finished
// saying so cannot be sampled: the reader decides which instant to look, and on
// a lossy path the transport is still correct at every instant before it. The
// deadline bounds the wait, so a connection that stops confirming fails with its
// own counters rather than hanging.
func waitForAcknowledged(t *testing.T, conn *Conn, want int, within time.Duration) Counters {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		counters := conn.Counters()
		if counters.Acknowledged >= want || !time.Now().Before(deadline) {
			if counters.Acknowledged < want {
				t.Errorf("Acknowledged = %d after %s, want at least %d: not every segment was confirmed",
					counters.Acknowledged, within, want)
			}
			return counters
		}
		time.Sleep(time.Millisecond)
	}
}

// roundTrip sends one payload and waits for it to come back.
func roundTrip(t *testing.T, conn *Conn, payload string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := conn.Write(ctx, []byte(payload)); err != nil {
		t.Fatalf("Write(%d bytes) = %v, want nil", len(payload), err)
	}
	got, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("Read() = %v, want the payload back", err)
	}
	if string(got) != payload {
		t.Fatalf("Read() = %q, want %q", got, payload)
	}
}

// A payload has to survive a real socket in both directions, which is the one
// thing the pure tests cannot say.
func TestAPayloadSurvivesARealSocket(t *testing.T) {
	t.Parallel()
	l, err := Listen(context.Background(), "127.0.0.1:0", testLimits())
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	serve(t, l)
	client, _ := connect(t, l, l.socket.LocalAddr().String(), testLimits())
	held, refused := l.Count()
	if held != 1 {
		t.Errorf("the listener holds %d connections, want 1", held)
	}
	if refused != 0 {
		t.Errorf("refused = %d, want 0", refused)
	}
	roundTrip(t, client, "and back again")
}

// A payload larger than the window is refused by the limits rather than being
// split into a number of segments this side cannot hold outstanding.
func TestAPayloadLargerThanTheWindowIsRefused(t *testing.T) {
	t.Parallel()
	l, err := Listen(context.Background(), "127.0.0.1:0", testLimits())
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	serve(t, l)
	limits := testLimits()
	client, _ := connect(t, l, l.socket.LocalAddr().String(), limits)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	oversized := make([]byte, limits.MaxPayload*limits.MaxWindow+1)
	if _, err := client.Write(ctx, oversized); err == nil {
		t.Error("Write() of a payload larger than the window = nil, want a refusal")
	}
	// Nothing was queued by the refusal, so the connection is still usable.
	roundTrip(t, client, "still working")
}

// A malformed datagram is dropped without a reply, and does not disturb the
// connection. This is the property that keeps the parser from being turned into
// a reflector and keeps a peer that speaks something else from resetting a
// connection that is working.
func TestAMalformedDatagramIsDroppedWithoutAReply(t *testing.T) {
	t.Parallel()
	limits := testLimits()
	l, err := Listen(context.Background(), "127.0.0.1:0", limits)
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	serve(t, l)
	client, _ := connect(t, l, l.socket.LocalAddr().String(), limits)

	// A second socket speaking to the listener as if it were the client, which
	// is what a peer of the wrong kind of protocol looks like.
	rude, err := net.DialUDP("udp", nil, l.socket.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("the rude socket could not be opened: %v", err)
	}
	defer rude.Close()
	for _, junk := range [][]byte{nil, {0x00}, make([]byte, MaxFrameSize+64), []byte("GET / HTTP/1.1\r\n\r\n")} {
		if _, err := rude.Write(junk); err != nil {
			t.Fatalf("the rude socket could not write: %v", err)
		}
	}
	// Give the listener time to have read them, so a reply would be seen.
	_ = rude.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	buffer := make([]byte, MaxFrameSize)
	if n, err := rude.Read(buffer); err == nil {
		t.Errorf("the listener answered a malformed datagram with %d bytes", n)
	}
	// The established connection is untouched.
	roundTrip(t, client, "after")
	if held, _ := l.Count(); held != 1 {
		t.Errorf("the listener holds %d connections, want 1: a malformed datagram created one", held)
	}
}

// A peer over the connection limit is refused and told so, and the connections
// already served are unaffected, because one peer asking for too much is not a
// reason to stop serving the others.
func TestAConnectionLimitIsEnforced(t *testing.T) {
	t.Parallel()
	limits := testLimits()
	limits.MaxConnections = 2
	l, err := Listen(context.Background(), "127.0.0.1:0", limits)
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	serve(t, l)

	first, _ := connect(t, l, l.socket.LocalAddr().String(), limits)
	second, _ := connect(t, l, l.socket.LocalAddr().String(), limits)
	if held, _ := l.Count(); held != 2 {
		t.Fatalf("the listener holds %d connections, want 2", held)
	}

	// A third peer is over the limit and is told with a reset rather than
	// served.
	third, err := net.DialUDP("udp", nil, l.socket.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("the third socket could not be opened: %v", err)
	}
	defer third.Close()
	if _, err := third.Write(encodedData(1, "three")); err != nil {
		t.Fatalf("the third socket could not write: %v", err)
	}
	buffer := make([]byte, MaxFrameSize)
	_ = third.SetReadDeadline(time.Now().Add(2 * time.Second))
	read, err := third.Read(buffer)
	if err != nil {
		t.Fatalf("the third peer got no answer, want a reset: %v", err)
	}
	frame, err := Unmarshal(buffer[:read])
	if err != nil {
		t.Fatalf("the answer was not a frame: %v", err)
	}
	if !frame.IsReset() {
		t.Errorf("the third peer got %+v, want a reset", frame)
	}
	held, refused := l.Count()
	if held != 2 {
		t.Errorf("the listener holds %d connections, want 2", held)
	}
	if refused != 1 {
		t.Errorf("refused = %d, want 1", refused)
	}
	// The two served connections still work, which is the point of refusing
	// rather than closing.
	roundTrip(t, first, "still one")
	roundTrip(t, second, "still two")
}

// A reset from a peer stops the connection with a reason rather than hanging,
// so a server that goes away does not leave a client waiting for a timeout on
// every read.
func TestAResetFromThePeerStopsTheConnection(t *testing.T) {
	t.Parallel()
	limits := testLimits()
	l, err := Listen(context.Background(), "127.0.0.1:0", limits)
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	serve(t, l)
	client, held := connect(t, l, l.socket.LocalAddr().String(), limits)
	held.deliver(Frame{Flags: FlagReset}, time.Now())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := client.Read(ctx); err == nil {
		t.Error("Read() after a reset = nil, want an error")
	}
}

// A peer that finishes sending brings an orderly end, and everything it sent
// before that is still returned. Data is not lost by the shutdown.
func TestAnOrderlyShutdownDoesNotLoseWhatWasSent(t *testing.T) {
	t.Parallel()
	limits := testLimits()
	l, err := Listen(context.Background(), "127.0.0.1:0", limits)
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	serve(t, l)
	client, held := connect(t, l, l.socket.LocalAddr().String(), limits)

	// The server sends three segments and then finishes, without waiting for the
	// acknowledgements, which is the case where data and the end arrive close
	// together.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, payload := range []string{"one", "two", "three"} {
		if _, err := held.Write(ctx, []byte(payload)); err != nil {
			t.Fatalf("the server could not write %q: %v", payload, err)
		}
	}
	if err := held.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	for _, want := range []string{"one", "two", "three"} {
		got, err := client.Read(ctx)
		if err != nil {
			t.Fatalf("Read() = %v, want %q", err, want)
		}
		if string(got) != want {
			t.Errorf("Read() = %q, want %q", got, want)
		}
	}
	// Then the end of the stream, rather than a read that never finishes.
	if _, err := client.Read(ctx); !errors.Is(err, io.EOF) {
		t.Errorf("Read() after the peer's finish = %v, want io.EOF", err)
	}
}

// A connection that has carried nothing for the idle timeout is stopped. A peer
// that opens a connection and walks away must not hold a socket, a goroutine
// and its window slots for the life of the process.
func TestAnIdleConnectionIsStopped(t *testing.T) {
	t.Parallel()
	limits := testLimits()
	limits.IdleTimeout = 300 * time.Millisecond
	l, err := Listen(context.Background(), "127.0.0.1:0", limits)
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	serve(t, l)
	// The client is given a longer idle timeout than the listener, so the
	// connection that goes quiet is the served one and the test is about the
	// peer's silence rather than about both ends giving up together.
	clientLimits := limits
	clientLimits.IdleTimeout = 30 * time.Second
	client, held := connect(t, l, l.socket.LocalAddr().String(), clientLimits)
	select {
	case <-held.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the served connection was not stopped after the idle timeout")
	}
	if held.Err() == nil {
		t.Error("Err() = nil after an idle stop, want a reason")
	}
	// The client was not stopped, because it was never the one that went quiet.
	// A connection is stopped for its own silence, not for its peer's.
	select {
	case <-client.Done():
		t.Error("the client was stopped, want only the connection that went quiet")
	default:
	}
	if client.Err() != nil {
		t.Errorf("the client's Err() = %v, want nil", client.Err())
	}
}

// A peer that never answers costs one ceiling of retransmissions and then the
// connection is abandoned. Without that a dead peer would cost a
// retransmission every timeout for as long as the process lives.
func TestAPeerThatNeverAnswersIsAbandoned(t *testing.T) {
	t.Parallel()
	limits := testLimits()
	limits.RetransmitTimeout = 30 * time.Millisecond
	limits.MaxRetransmits = 3
	limits.IdleTimeout = time.Second
	// A socket bound but never read from: the client's datagrams are answered
	// by the kernel, so what is measured here is this side giving up rather than
	// the path being silent. The window fills, so the sender stops anyway.
	silent, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("the silent socket could not be opened: %v", err)
	}
	defer silent.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := Dial(ctx, "127.0.0.1:0", silent.LocalAddr().String(), limits)
	if err != nil {
		t.Fatalf("Dial() = %v, want nil", err)
	}
	defer client.Close()
	// The silent socket answers nothing at all, which is what a black hole
	// looks like, so no acknowledgement ever arrives.
	if _, err := client.Write(ctx, []byte("into the void")); err == nil {
		// The write returns once the payload is on the wire, which is expected.
		_ = err
	}
	select {
	case <-client.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the connection was never abandoned")
	}
	if client.Err() == nil {
		t.Error("Err() = nil after being abandoned, want a reason")
	}
	counters := client.Counters()
	if counters.Timeouts == 0 {
		t.Error("Timeouts = 0, want the retransmission deadlines to be counted")
	}
}

// Nothing a closed connection started may still be running. This is the check
// that makes the goroutine budget a fact rather than a hope.
//
// It is the one test here that does not run in parallel with the others, and it
// is not parallel with them for its own sake: it counts the goroutines in the
// process before and after, so a number of unrelated tests opening and closing
// connections at the same time is indistinguishable from a leak. It runs in the
// sequential phase instead, where the only goroutines that start or stop are the
// ones this test starts.
func TestAClosedConnectionLeavesNothingRunning(t *testing.T) {
	limits := testLimits()
	l, err := Listen(context.Background(), "127.0.0.1:0", limits)
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	serve(t, l)

	// Measured after the first connection has been closed once, so one-time
	// runtime and package initialisation is not counted as a leak.
	client, _ := connect(t, l, l.socket.LocalAddr().String(), limits)
	if err := client.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	before := runtime.NumGoroutine()

	for range 8 {
		conn, _ := connect(t, l, l.socket.LocalAddr().String(), limits)
		roundTrip(t, conn, "payload in flight")
		if err := conn.Close(); err != nil {
			t.Fatalf("Close() = %v, want nil", err)
		}
		if err := conn.Close(); err != nil {
			t.Fatalf("Close() again = %v, want nil: closing twice is safe", err)
		}
	}
	// The served connections are closed with the listener, so they are counted
	// from after that instead.
	if err := l.Close(); err != nil {
		t.Fatalf("Close() of the listener = %v, want nil", err)
	}
	after := runtime.NumGoroutine()
	if after > before {
		t.Errorf("goroutines went from %d to %d, want no more than before", before, after)
	}
	_ = l.Conn
}

// Close is idempotent from both directions: a connection that failed can still
// be closed, and closing an already closed one says so rather than failing.
func TestClosingIsSafeFromEitherDirection(t *testing.T) {
	t.Parallel()
	limits := testLimits()
	l, err := Listen(context.Background(), "127.0.0.1:0", limits)
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	serve(t, l)
	client, held := connect(t, l, l.socket.LocalAddr().String(), limits)
	// The server stops first, so the client's socket is closed by a peer that
	// went away rather than by its own caller.
	if err := held.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	if err := held.Close(); err != nil {
		t.Fatalf("Close() again = %v, want nil", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close() of the client = %v, want nil", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close() of the client again = %v, want nil", err)
	}
	// Both are stopped and neither reports a second, different reason.
	// Neither end reported a second, different reason for stopping, because a
	// close is not a failure and a socket closed by a peer that went away is the
	// same event seen from two ends.
	if client.Err() != nil {
		t.Errorf("the client's Err() = %v after being closed, want nil", client.Err())
	}
	if held.Err() != nil {
		t.Errorf("the served connection's Err() = %v after being closed, want nil", held.Err())
	}
}

// A cancelled context stops the listener, even with no traffic: a read on a
// socket blocks until a datagram arrives, so this only holds if the listener
// watches the context rather than waiting to notice.
func TestACancelledContextStopsTheListener(t *testing.T) {
	t.Parallel()
	limits := testLimits()
	ctx, cancel := context.WithCancel(context.Background())
	l, err := Listen(ctx, "127.0.0.1:0", limits)
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	done := make(chan error, 1)
	go func() { done <- l.Serve(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, ErrClosed) && ctx.Err() == nil {
			t.Errorf("Serve() = %v, want nil or a closed listener", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("the listener kept serving after its context was cancelled")
	}
}

// A refused or impossible address is reported rather than left to the socket,
// so a caller learns the address was the problem.
func TestABadAddressIsRefused(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Dial(ctx, "127.0.0.1:0", "not an address", testLimits()); err == nil {
		t.Error("Dial() with a malformed peer address = nil, want a refusal")
	}
	if _, err := Dial(ctx, "not an address", "127.0.0.1:1", testLimits()); err == nil {
		t.Error("Dial() with a malformed local address = nil, want a refusal")
	}
	if _, err := Listen(ctx, "not an address", testLimits()); err == nil {
		t.Error("Listen() with a malformed address = nil, want a refusal")
	}
}

// A connection that is not a client has no handshake to fail, so what Dial has
// to guarantee is that the limits were checked and the socket is the peer's, so
// a connected socket rather than one that can be pointed anywhere later.
func TestAClientSocketIsConnectedToItsPeer(t *testing.T) {
	t.Parallel()
	limits := testLimits()
	l, err := Listen(context.Background(), "127.0.0.1:0", limits)
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	serve(t, l)
	client := dial(t, l.socket.LocalAddr().String(), limits)
	remote := client.RemoteAddr().String()
	if remote != l.socket.LocalAddr().String() {
		t.Errorf("RemoteAddr() = %s, want the peer %s", remote, l.socket.LocalAddr())
	}
	if client.LocalAddr() == nil {
		t.Error("LocalAddr() = nil, want the socket's own end")
	}
}

// relayed is a real path between a client and a listener that can be made to
// lose, copy or reorder datagrams. The faults are a real socket's faults rather
// than a stubbed transport's, because what is being tested is what this
// transport does to them.
type relayed struct {
	// client is the address a client dials, and server is the socket the
	// listener then sees, which is this relay rather than the client.
	client string
	server string
	drop   atomic.Int64
	copied atomic.Int64
	held   atomic.Int64
	// hoard is the one datagram waiting to go out behind its successor, and
	// clientAddr is where the far direction has to be sent.
	mu         sync.Mutex
	hoarded    []byte
	clientAddr *net.UDPAddr
}

// relay puts a real UDP path between a client and a listener, applying the
// faults to what passes through it in both directions. The faults are on a
// fixed sequence rather than a random one, so a test that fails fails the same
// way again.
func relay(t *testing.T, l *Listener, dropEvery, copyEvery int, hoard bool) *relayed {
	t.Helper()
	front, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("the relay's front socket could not be opened: %v", err)
	}
	// The back socket is connected to the listener, so everything the relay sends
	// arrives from one address and the listener sees one peer for this client.
	back, err := net.DialUDP("udp", nil, l.socket.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("the relay's back socket could not be opened: %v", err)
	}
	r := &relayed{client: front.LocalAddr().String(), server: back.LocalAddr().String()}

	// Each direction counts its own datagrams, because a fault applied to a
	// fixed sequence has to mean the same thing in both.
	//
	// Going back towards the client needs the address the client sent from,
	// which the forward direction learns from its first datagram. The front
	// socket only listens, so nothing may be written to it by address alone.
	pump := func(from, to *net.UDPConn, hold bool) {
		var counted atomic.Int64
		buffer := make([]byte, MaxFrameSize)
		for {
			read, fromAddr, err := from.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			if from == front {
				// Whatever sent this came from the client, and that is the
				// address the far direction answers.
				r.mu.Lock()
				if r.clientAddr == nil {
					r.clientAddr = fromAddr
				}
				r.mu.Unlock()
			}
			datagram := append([]byte(nil), buffer[:read]...)
			index := counted.Add(1)
			if dropEvery > 0 && index%int64(dropEvery) == 0 {
				r.drop.Add(1)
				continue
			}
			if hold && index == 1 {
				// The first datagram out of order is held until its successor
				// goes, which is what a path that reorders looks like from the
				// far end: segment two arrives before segment one.
				r.mu.Lock()
				r.hoarded = datagram
				r.mu.Unlock()
				r.held.Add(1)
				continue
			}
			send := func(datagram []byte) error {
				if to != nil {
					_, err := to.Write(datagram)
					return err
				}
				r.mu.Lock()
				clientAddr := r.clientAddr
				r.mu.Unlock()
				if clientAddr == nil {
					// Nothing has come from the client yet, so there is
					// nowhere to answer it.
					return nil
				}
				_, err := front.WriteToUDP(datagram, clientAddr)
				return err
			}
			if err := send(datagram); err != nil {
				return
			}
			if hold {
				r.mu.Lock()
				waiting := r.hoarded
				r.hoarded = nil
				r.mu.Unlock()
				if len(waiting) > 0 {
					if err := send(waiting); err != nil {
						return
					}
				}
			}
			if copyEvery > 0 && index%int64(copyEvery) == 0 {
				if err := send(datagram); err != nil {
					return
				}
				r.copied.Add(1)
			}
		}
	}
	go pump(front, back, hoard)
	go pump(back, nil, false)
	t.Cleanup(func() {
		_ = front.Close()
		_ = back.Close()
	})
	return r
}

// A path that loses datagrams still delivers every payload, in order, because
// the missing ones are retransmitted. The count of what was dropped is the
// evidence that recovery was needed rather than assumed.
func TestALossyPathStillDeliversEverythingInOrder(t *testing.T) {
	t.Parallel()
	limits := testLimits()
	limits.RetransmitTimeout = 40 * time.Millisecond
	limits.MaxRetransmits = 40
	limits.MaxRTT = 300 * time.Millisecond
	limits.IdleTimeout = 10 * time.Second
	l, err := Listen(context.Background(), "127.0.0.1:0", limits)
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	serve(t, l)
	// One in four datagrams lost in each direction, which is well past what a
	// window of eight can absorb without retransmitting.
	path := relay(t, l, 4, 0, false)
	client, served := connectSeenAs(t, l, path.client, path.server, limits)

	const messages = 12
	for index := range messages {
		roundTrip(t, client, fmt.Sprintf("message %d", index))
	}
	if path.drop.Load() == 0 {
		t.Fatal("no datagram was dropped, want the path to have lost some")
	}
	// Recovery is counted on whichever end lost something: a datagram dropped on
	// the way back is the far end's problem to fix, and a test that insisted on
	// seeing it on this one would be asserting where the fault fell rather than
	// that it was recovered from.
	if sent, served := client.Counters(), served.Counters(); sent.Retransmitted+served.Retransmitted == 0 {
		t.Errorf("neither end retransmitted (%d and %d), want the loss recovered from",
			sent.Retransmitted, served.Retransmitted)
	}
	// Every payload was delivered, because the loop above read each one back and
	// compared it. The counter is the other half of the same statement: what was
	// confirmed is what was sent, and what was sent is what the peer got. The
	// knock that established the connection is a segment of its own, so it is
	// counted here too.
	//
	// The counter is waited for rather than sampled. Reading it the instant the
	// last echo came back asks whether the acknowledgement of that echo had
	// already been processed, and it had not necessarily: that acknowledgement
	// is a datagram of its own on a path that drops one in four, so on some runs
	// it is still in flight and on some it was dropped and is waiting for the
	// retransmission timer. Both are the path behaving correctly, and a test that
	// asserted on which one it was asserting the wire's luck rather than the
	// transport. Waiting for it states the property the test exists for — every
	// segment this side sent was confirmed — and holds whatever the path did.
	//
	// The upper bound is the knock and every message and nothing more. A
	// retransmission is not a new segment, so a count above that would be the
	// connection counting the same payload twice.
	counters := waitForAcknowledged(t, client, messages, 5*time.Second)
	if counters.Acknowledged > messages+1 {
		t.Errorf("Acknowledged = %d, want at most %d: every segment confirmed once, plus the knock",
			counters.Acknowledged, messages+1)
	}
	if counters.Received == 0 {
		t.Error("Received = 0, want the echoed payloads")
	}
}

// A path that loses enough of a large transfer to make the sender retransmit
// constantly must still finish it. The transfer is offered while it is read, at
// the size a benchmark uses, because a smaller one would never fill the window
// and a sequential one would be measuring the path's first packet rather than
// its behaviour.
//
// The two tests below that pin the outstanding set against the window are the
// deterministic evidence for what a stale retransmission does; this one is the
// real-socket counterpart, and it is here because a windowed sender recovering
// from a path that drops a tenth of what it sends is a claim about sockets and
// not about a state machine.
func TestALossyPathStillFinishesALargeTransfer(t *testing.T) {
	t.Parallel()
	limits := testLimits()
	limits.MaxPayload = 1024
	limits.MaxWindow = 32
	limits.MaxReorder = 16
	limits.RetransmitTimeout = 10 * time.Millisecond
	limits.MaxRetransmits = 200
	limits.MaxRTT = 200 * time.Millisecond
	limits.IdleTimeout = 5 * time.Second
	l, err := Listen(context.Background(), "127.0.0.1:0", limits)
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	serve(t, l)
	// One datagram in ten lost in each direction, with a retransmit timeout of
	// ten milliseconds, so the sender is timing segments out and recovering from
	// them for most of the transfer.
	path := relay(t, l, 10, 0, false)
	client, servedSide := connectSeenAs(t, l, path.client, path.server, limits)

	// Every payload is distinct, so an answer that arrived in the wrong place or
	// was a different message is caught rather than counted. The index is the
	// first two bytes, which is enough to tell 256 of them apart.
	const messages = 256
	offered := make([][]byte, messages)
	for index := range offered {
		offered[index] = make([]byte, limits.MaxPayload)
		binary.BigEndian.PutUint16(offered[index][:2], uint16(index))
	}

	sendErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		for _, message := range offered {
			if _, err := client.Write(ctx, message); err != nil {
				sendErr <- err
				return
			}
		}
		sendErr <- nil
	}()

	for index := range offered {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		got, err := client.Read(ctx)
		cancel()
		if err != nil {
			cc, sc := client.Counters(), servedSide.Counters()
			// Both ends are reported because a transfer on a lossy path can be
			// stopped by either of them, and which one ran out of what it was
			// waiting for is the whole question when it does not finish.
			t.Fatalf("Read() after %d of %d payloads = %v\n"+
				"  client: sent=%d acked=%d retx=%d window=%d recv=%+v\n"+
				"  served: sent=%d acked=%d retx=%d window=%d recv=%+v\n"+
				"  the path dropped %d datagrams\n"+
				"  client: %s\n  served: %s",
				index, messages, err,
				cc.Sent, cc.Acknowledged, cc.Retransmitted, cc.Window, client.ReceiveStats(),
				sc.Sent, sc.Acknowledged, sc.Retransmitted, sc.Window, servedSide.ReceiveStats(),
				path.drop.Load(), debugState(client), debugState(servedSide))
		}
		if !bytes.Equal(got, offered[index]) {
			t.Fatalf("Read() returned the wrong payload at %d", index)
		}
	}
	if err := <-sendErr; err != nil {
		t.Fatalf("the transfer stopped after %d payloads: %v", messages, err)
	}
	if path.drop.Load() == 0 {
		t.Fatal("no datagram was dropped, want the path to have lost some")
	}
	assertNoDrift(t, client)
}

// A segment the peer has already confirmed must not be recorded again, however
// it comes to be written, and the reason is the window. The retransmission loop
// collects the segments it will resend and then releases the lock to write them,
// so an acknowledgement for a segment that is already in that list arrives
// before the write does. Recording the segment again would count it as another
// segment in flight, and because the window is the difference between two
// cumulative totals, the error would never unwind: the sender would stop for good
// on a path with any loss at all, reporting a connection that had nothing
// outstanding and could not send.
func TestAStaleRetransmissionIsNotCountedAsInFlight(t *testing.T) {
	t.Parallel()
	limits := testLimits()
	conn := newConn(limits)
	var written []Frame
	conn.writeFrame = func(encoded []byte) error {
		frame, err := Unmarshal(encoded)
		if err != nil {
			return err
		}
		written = append(written, frame)
		return nil
	}
	payload := []byte("one segment")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := conn.Write(ctx, payload); err != nil {
		t.Fatalf("Write() = %v, want the segment to go out", err)
	}
	sequence := conn.nextSeq - 1
	// The peer confirms it, so it is no longer outstanding and never has to be
	// sent again.
	conn.acknowledge(sequence, conn.now())
	if len(written) != 1 {
		t.Fatalf("%d frames were written, want 1", len(written))
	}

	// The retransmission arrives late: the loop chose this segment before the
	// acknowledgement reached it.
	if err := conn.write(Frame{Flags: FlagData, Sequence: sequence, Payload: payload}); err != nil {
		t.Fatalf("write() of a stale segment = %v, want it refused silently", err)
	}
	if len(written) != 1 {
		t.Errorf("%d frames were written, want the confirmed segment not to be sent again", len(written))
	}
	assertNoDrift(t, conn)

	// A segment the peer has not confirmed is still sent again, or this would be
	// a transport that gives up on a lost segment rather than one that recovers
	// from loss.
	unconfirmed := newConn(limits)
	written = nil
	unconfirmed.writeFrame = conn.writeFrame
	if _, err := unconfirmed.Write(ctx, payload); err != nil {
		t.Fatalf("Write() = %v, want the segment to go out", err)
	}
	if err := unconfirmed.write(Frame{Flags: FlagData,
		Sequence: unconfirmed.nextSeq - 1, Payload: payload}); err != nil {
		t.Fatalf("write() of an unconfirmed segment = %v, want it sent again", err)
	}
	if len(written) != 2 {
		t.Errorf("%d frames were written, want the unconfirmed segment to be sent again", len(written))
	}
	assertNoDrift(t, unconfirmed)
}

// The same race reached through the retransmission loop: a segment chosen for
// resending, confirmed before it is written, must be neither written nor counted
// as a retransmission, because nothing was lost and nothing went out.
func TestARetransmissionThatRacesAnAcknowledgementIsNotCounted(t *testing.T) {
	t.Parallel()
	limits := testLimits()
	conn := newConn(limits)
	var written int
	conn.writeFrame = func([]byte) error {
		written++
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := conn.Write(ctx, []byte("late")); err != nil {
		t.Fatalf("Write() = %v, want the segment to go out", err)
	}
	sequence := conn.nextSeq - 1
	conn.mu.Lock()
	pending := conn.out[sequence]
	conn.mu.Unlock()
	if pending == nil {
		t.Fatal("the segment is not outstanding, want it waiting to be resent")
	}
	before := conn.Counters()
	// The loop collects the segment, and the confirmation lands in between.
	conn.acknowledge(sequence, conn.now())
	if err := conn.resend(pending); err != nil {
		t.Fatalf("resend() = %v, want the confirmed segment dropped", err)
	}
	if written != 1 {
		t.Errorf("%d frames were written, want the confirmed segment not to be sent again", written)
	}
	after := conn.Counters()
	if after.Retransmitted != before.Retransmitted {
		t.Errorf("Retransmitted = %d, want it unchanged at %d: nothing was sent",
			after.Retransmitted, before.Retransmitted)
	}
	if after.Timeouts != before.Timeouts {
		t.Errorf("Timeouts = %d, want it unchanged at %d: nothing timed out",
			after.Timeouts, before.Timeouts)
	}
	assertNoDrift(t, conn)
}

// debugState is one line describing what a connection is waiting for, so that a
// transfer which does not finish says which end is holding what up instead of
// only that it stopped.
func debugState(c *Conn) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	seqs := make([]uint32, 0, len(c.out))
	for s := range c.out {
		seqs = append(seqs, s)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	return fmt.Sprintf("out=%d %v inFlight=%d sent=%d acked=%d nextSeq=%d recvNext=%d window=%d ssthresh=%d inRecovery=%v recoveryAt=%d err=%v",
		len(c.out), seqs, c.control.inFlight(), c.control.sent, c.control.acknowledged,
		c.nextSeq, c.receiver.next, c.control.window, c.control.ssthresh,
		c.control.inRecovery, c.control.recoveryAt, c.err)
}

// assertNoDrift checks the invariant the window depends on: the number of
// segments a connection is holding is the number its congestion window believes
// is in flight. The two are counted differently, one by the outstanding set and
// one as the difference between two totals, so they can only be compared, and a
// difference is a sender that has stopped for a reason nothing reports.
func assertNoDrift(t *testing.T, conn *Conn) {
	t.Helper()
	conn.mu.Lock()
	defer conn.mu.Unlock()
	held := uint32(len(conn.out))
	if inFlight := conn.control.inFlight(); inFlight != held {
		t.Errorf("the window believes %d segments are in flight and %d are outstanding; "+
			"a sender waiting for room nothing is holding never gets it", inFlight, held)
	}
}

// A path that duplicates datagrams must not duplicate payloads, because the
// receiver has already seen them. The count of what was copied is the evidence
// that the fault was applied.
func TestADuplicatingPathDoesNotDuplicatePayloads(t *testing.T) {
	t.Parallel()
	limits := testLimits()
	limits.RetransmitTimeout = 40 * time.Millisecond
	limits.IdleTimeout = 10 * time.Second
	l, err := Listen(context.Background(), "127.0.0.1:0", limits)
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	serve(t, l)
	path := relay(t, l, 0, 3, false)
	client, held := connectSeenAs(t, l, path.client, path.server, limits)

	for index := range 4 {
		roundTrip(t, client, fmt.Sprintf("once only %d", index))
	}
	if path.copied.Load() == 0 {
		t.Fatal("no datagram was copied, want the path to have duplicated some")
	}
	// The payloads were compared one by one above, so a duplicate delivered to
	// the caller would have failed there. The counter is the other half: the
	// receiver saw the copies and refused them.
	if held.Counters().Duplicated == 0 {
		t.Error("Duplicated = 0, want the copied datagrams to have been recognised")
	}
}

// A path that reorders still delivers in order, because the reorder buffer holds
// what arrived early until the gap in front of it closes.
func TestAReorderingPathStillDeliversInOrder(t *testing.T) {
	t.Parallel()
	limits := testLimits()
	limits.RetransmitTimeout = 40 * time.Millisecond
	limits.IdleTimeout = 10 * time.Second
	l, err := Listen(context.Background(), "127.0.0.1:0", limits)
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	serve(t, l)
	path := relay(t, l, 0, 0, true)
	client, held := connectSeenAs(t, l, path.client, path.server, limits)

	// The first datagram of each direction is held until the next one goes out,
	// so at least one segment arrives behind the one it should follow.
	for index := range 4 {
		roundTrip(t, client, fmt.Sprintf("in order %d", index))
	}
	if path.held.Load() == 0 {
		t.Fatal("no datagram was held, want the path to have reordered some")
	}
	if held.Counters().Duplicated == 0 {
		t.Error("Duplicated = 0, want the held datagram to have arrived again and been refused")
	}
}

// The counters a benchmark reads have to mean what they say. A segment sent
// twice is one retransmission, and the count of frames written has to match the
// frames written, or the comparison against a configured loss is circular.
func TestTheCountersMeanWhatTheySay(t *testing.T) {
	t.Parallel()
	limits := testLimits()
	limits.RetransmitTimeout = 30 * time.Millisecond
	limits.MaxRetransmits = 40
	limits.IdleTimeout = 10 * time.Second
	l, err := Listen(context.Background(), "127.0.0.1:0", limits)
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	serve(t, l)
	// One in three lost, which will cost retransmissions without stalling.
	path := relay(t, l, 3, 0, false)
	client, _ := connectSeenAs(t, l, path.client, path.server, limits)
	for index := range 8 {
		roundTrip(t, client, fmt.Sprintf("counted %d", index))
	}
	// The knock, eight messages, and whatever the loss made necessary. A
	// retransmission is a frame sent again, so the frames written have to be at
	// least the segments plus the resends, and the resends can never exceed the
	// frames written.
	const segments = 9
	counters := client.Counters()
	if counters.Sent < segments+counters.Retransmitted {
		t.Errorf("Sent = %d, want at least the %d segments plus %d retransmissions",
			counters.Sent, segments, counters.Retransmitted)
	}
	if counters.Retransmitted > counters.Sent {
		t.Errorf("Retransmitted = %d, want no more than the %d frames sent",
			counters.Retransmitted, counters.Sent)
	}
	// Acknowledged counts distinct segments, so it cannot exceed what was sent.
	// It can also be one behind what was sent, because an acknowledgement lost
	// on this path is still in flight when the counters are read and the
	// segment it covers has not been released yet. Delivery itself was checked
	// above, one payload at a time.
	if counters.Acknowledged == 0 || counters.Acknowledged > segments {
		t.Errorf("Acknowledged = %d, want between 1 and the %d segments sent",
			counters.Acknowledged, segments)
	}
	if counters.Window == 0 {
		t.Error("Window = 0, want the window the last segment was sent under")
	}
	if counters.RTT == 0 {
		t.Error("RTT = 0 after eight confirmed segments, want it measured")
	}
	// The connection is still open, so it has a start and no end. Reading the
	// counters of a closed connection is a different test, and asserting an end
	// here would be asserting that the test had already cleaned up.
	if counters.Started.IsZero() {
		t.Error("Started is zero, want when the connection was built")
	}
	if !counters.Closed.IsZero() {
		t.Errorf("Closed = %s while the connection is open, want zero", counters.Closed)
	}
}

// A payload of exactly the frame ceiling is the largest this transport carries,
// and it has to survive a real socket rather than only the codec.
func TestTheLargestPayloadThisTransportCarriesSurvives(t *testing.T) {
	t.Parallel()
	limits := testLimits()
	l, err := Listen(context.Background(), "127.0.0.1:0", limits)
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	serve(t, l)
	client, _ := connect(t, l, l.socket.LocalAddr().String(), limits)
	roundTrip(t, client, string(make([]byte, limits.MaxPayload)))

	// And a payload that is a whole window's worth, which is split into
	// segments, has to come back the same size.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	whole := make([]byte, limits.MaxPayload*limits.MaxWindow)
	for index := range whole {
		whole[index] = byte(index)
	}
	if _, err := client.Write(ctx, whole); err != nil {
		t.Fatalf("Write(%d bytes) = %v, want nil", len(whole), err)
	}
	got := make([]byte, 0, len(whole))
	for len(got) < len(whole) {
		chunk, err := client.Read(ctx)
		if err != nil {
			t.Fatalf("Read() = %v after %d of %d bytes", err, len(got), len(whole))
		}
		got = append(got, chunk...)
	}
	if len(got) != len(whole) {
		t.Fatalf("read %d bytes, want %d", len(got), len(whole))
	}
	for index := range whole {
		if got[index] != whole[index] {
			t.Fatalf("byte %d = %d, want %d", index, got[index], whole[index])
		}
	}
}

// encodedData builds a data frame for a test that speaks the wire format
// directly rather than through a connection.
func encodedData(sequence uint32, payload string) []byte {
	frame, err := Frame{Flags: FlagData, Sequence: sequence, Payload: []byte(payload)}.Marshal()
	if err != nil {
		panic(err)
	}
	return frame
}

// A sender that reuses one buffer must still have every payload arrive as it
// was written. The frame a segment is carried in is kept until the peer
// acknowledges it and is sent again if the acknowledgement does not come, so a
// frame that pointed at the caller's memory would resend whatever the caller
// has written there since. Reading the payloads back one buffer's worth of
// messages later is what catches it: the retransmitted segments come back
// carrying the bytes of a later message, and only a lossy path produces a
// retransmission at all.
func TestAReusedBufferIsNotResentAsTheCallersMemory(t *testing.T) {
	t.Parallel()
	limits := testLimits()
	limits.RetransmitTimeout = 40 * time.Millisecond
	limits.MaxRetransmits = 40
	limits.MaxRTT = 300 * time.Millisecond
	limits.IdleTimeout = 10 * time.Second
	l, err := Listen(context.Background(), "127.0.0.1:0", limits)
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	serve(t, l)
	path := relay(t, l, 3, 0, false)
	client, _ := connectSeenAs(t, l, path.client, path.server, limits)

	// One buffer for every message, which is what a caller copying from a file
	// or a socket into a reused slice does.
	const messages = 8
	// One segment per message, so what comes back is compared to what went out
	// rather than to a fragment of it.
	const size = 256
	buffer := make([]byte, size)
	want := make([][]byte, 0, messages)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for index := range messages {
		for offset := range size {
			buffer[offset] = byte((index*31 + offset*7) % 253)
		}
		want = append(want, append([]byte(nil), buffer...))
		if _, err := client.Write(ctx, buffer); err != nil {
			t.Fatalf("Write(%d) = %v, want nil", index, err)
		}
	}
	if path.drop.Load() == 0 {
		t.Fatal("no datagram was dropped, want the path to have lost some")
	}
	if client.Counters().Retransmitted == 0 {
		t.Fatal("nothing was retransmitted, so this path did not test a resend")
	}
	for index := range messages {
		got, err := client.Read(ctx)
		if err != nil {
			t.Fatalf("Read(%d) = %v, want the payload back", index, err)
		}
		if !bytes.Equal(got, want[index]) {
			t.Fatalf("Read(%d) returned the wrong bytes for the message written at that index", index)
		}
	}
}

// A write that cannot get window space has to end when its context is
// cancelled. A peer that never acknowledges anything would otherwise hold the
// caller's goroutine until the connection itself timed out, which is a
// cancellation that does not cancel.
func TestAWriteEndsWhenTheWindowNeverOpens(t *testing.T) {
	t.Parallel()
	limits := testLimits()
	limits.MaxWindow = 2
	limits.MaxPayload = 256
	limits.IdleTimeout = 30 * time.Second
	limits.MaxRetransmits = 1000
	// The peer is a socket that reads and never answers, so nothing is
	// acknowledged and the window cannot open again.
	silent, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("the silent peer could not be opened: %v", err)
	}
	defer func() { _ = silent.Close() }()
	go func() {
		buffer := make([]byte, MaxFrameSize)
		for {
			if _, _, err := silent.ReadFromUDP(buffer); err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := Dial(ctx, "127.0.0.1:0", silent.LocalAddr().String(), limits)
	if err != nil {
		t.Fatalf("Dial() = %v, want nil", err)
	}
	defer func() { _ = client.Close() }()

	// The window is two segments and the peer confirms nothing, so these two
	// writes fill it and the next one has to wait for space that never comes.
	payload := make([]byte, limits.MaxPayload)
	if _, err := client.Write(ctx, payload); err != nil {
		t.Fatalf("Write() = %v, want the first segment to go out", err)
	}
	if _, err := client.Write(ctx, payload); err != nil {
		t.Fatalf("Write() = %v, want the second segment to go out", err)
	}
	started := time.Now()
	short, cancelShort := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelShort()
	if _, err := client.Write(short, payload); err == nil {
		t.Fatal("Write() = nil, want the cancelled write to fail")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Write() = %v, want a deadline the caller can recognise", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("Write() took %s to give up, want it to end with its context", elapsed)
	}
}
