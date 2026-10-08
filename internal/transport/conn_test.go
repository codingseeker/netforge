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
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

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

func connect(t *testing.T, l *Listener, to string, limits Limits) (*Conn, *Conn) {
	t.Helper()

	return connectSeenAs(t, l, to, "", limits)
}

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

func echo(t *testing.T, conn *Conn) {
	t.Helper()
	go func() {

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

	roundTrip(t, client, "still working")
}

func TestAMalformedDatagramIsDroppedWithoutAReply(t *testing.T) {
	t.Parallel()
	limits := testLimits()
	l, err := Listen(context.Background(), "127.0.0.1:0", limits)
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	serve(t, l)
	client, _ := connect(t, l, l.socket.LocalAddr().String(), limits)

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

	_ = rude.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	buffer := make([]byte, MaxFrameSize)
	if n, err := rude.Read(buffer); err == nil {
		t.Errorf("the listener answered a malformed datagram with %d bytes", n)
	}

	roundTrip(t, client, "after")
	if held, _ := l.Count(); held != 1 {
		t.Errorf("the listener holds %d connections, want 1: a malformed datagram created one", held)
	}
}

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

	roundTrip(t, first, "still one")
	roundTrip(t, second, "still two")
}

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

func TestAnOrderlyShutdownDoesNotLoseWhatWasSent(t *testing.T) {
	t.Parallel()
	limits := testLimits()
	l, err := Listen(context.Background(), "127.0.0.1:0", limits)
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	serve(t, l)
	client, held := connect(t, l, l.socket.LocalAddr().String(), limits)

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

	if _, err := client.Read(ctx); !errors.Is(err, io.EOF) {
		t.Errorf("Read() after the peer's finish = %v, want io.EOF", err)
	}
}

func TestAnIdleConnectionIsStopped(t *testing.T) {
	t.Parallel()
	limits := testLimits()
	limits.IdleTimeout = 300 * time.Millisecond
	l, err := Listen(context.Background(), "127.0.0.1:0", limits)
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	serve(t, l)

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

	select {
	case <-client.Done():
		t.Error("the client was stopped, want only the connection that went quiet")
	default:
	}
	if client.Err() != nil {
		t.Errorf("the client's Err() = %v, want nil", client.Err())
	}
}

func TestAPeerThatNeverAnswersIsAbandoned(t *testing.T) {
	t.Parallel()
	limits := testLimits()
	limits.RetransmitTimeout = 30 * time.Millisecond
	limits.MaxRetransmits = 3
	limits.IdleTimeout = time.Second

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

	if _, err := client.Write(ctx, []byte("into the void")); err == nil {

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

func TestAClosedConnectionLeavesNothingRunning(t *testing.T) {
	limits := testLimits()
	l, err := Listen(context.Background(), "127.0.0.1:0", limits)
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	serve(t, l)

	before := runtime.NumGoroutine()

	client, _ := connect(t, l, l.socket.LocalAddr().String(), limits)
	if err := client.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	assertFinished(t, "the client connection", client)

	for round := range 8 {
		conn, _ := connect(t, l, l.socket.LocalAddr().String(), limits)
		roundTrip(t, conn, "payload in flight")
		if err := conn.Close(); err != nil {
			t.Fatalf("Close() = %v, want nil", err)
		}
		if err := conn.Close(); err != nil {
			t.Fatalf("Close() again = %v, want nil: closing twice is safe", err)
		}
		assertFinished(t, "connection "+strconv.Itoa(round), conn)
	}

	if !awaitHeld(t, l, 0) {
		held, _ := l.Count()
		t.Errorf("the listener still holds %d connections after every client was closed", held)
	}

	if err := l.Close(); err != nil {
		t.Fatalf("Close() of the listener = %v, want nil", err)
	}
	if held, _ := l.Count(); held != 0 {
		t.Errorf("the listener still holds %d connections after it was closed", held)
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
		t.Errorf("goroutines went from %d to %d and did not settle, want no more than before", before, after)
	}
}

func assertFinished(t *testing.T, what string, conn *Conn) {
	t.Helper()
	select {
	case <-conn.Done():
	case <-time.After(2 * time.Second):
		t.Fatalf("%s is still running two seconds after it was closed", what)
	}
}

func awaitHeld(t *testing.T, l *Listener, want int) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		held, _ := l.Count()
		if held == want {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestClosingIsSafeFromEitherDirection(t *testing.T) {
	t.Parallel()
	limits := testLimits()
	l, err := Listen(context.Background(), "127.0.0.1:0", limits)
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	serve(t, l)
	client, held := connect(t, l, l.socket.LocalAddr().String(), limits)

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

	if client.Err() != nil {
		t.Errorf("the client's Err() = %v after being closed, want nil", client.Err())
	}
	if held.Err() != nil {
		t.Errorf("the served connection's Err() = %v after being closed, want nil", held.Err())
	}
}

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

type relayed struct {
	client string
	server string
	drop   atomic.Int64
	copied atomic.Int64
	held   atomic.Int64

	mu         sync.Mutex
	hoarded    []byte
	clientAddr *net.UDPAddr
}

func relay(t *testing.T, l *Listener, dropEvery, copyEvery int, hoard bool) *relayed {
	t.Helper()
	front, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("the relay's front socket could not be opened: %v", err)
	}

	back, err := net.DialUDP("udp", nil, l.socket.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("the relay's back socket could not be opened: %v", err)
	}
	r := &relayed{client: front.LocalAddr().String(), server: back.LocalAddr().String()}

	pump := func(from, to *net.UDPConn, hold bool) {
		var counted atomic.Int64
		buffer := make([]byte, MaxFrameSize)
		for {
			read, fromAddr, err := from.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			if from == front {

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

	path := relay(t, l, 4, 0, false)
	client, served := connectSeenAs(t, l, path.client, path.server, limits)

	const messages = 12
	for index := range messages {
		roundTrip(t, client, fmt.Sprintf("message %d", index))
	}
	if path.drop.Load() == 0 {
		t.Fatal("no datagram was dropped, want the path to have lost some")
	}

	if sent, served := client.Counters(), served.Counters(); sent.Retransmitted+served.Retransmitted == 0 {
		t.Errorf("neither end retransmitted (%d and %d), want the loss recovered from",
			sent.Retransmitted, served.Retransmitted)
	}

	counters := waitForAcknowledged(t, client, messages, 5*time.Second)
	if counters.Acknowledged > messages+1 {
		t.Errorf("Acknowledged = %d, want at most %d: every segment confirmed once, plus the knock",
			counters.Acknowledged, messages+1)
	}
	if counters.Received == 0 {
		t.Error("Received = 0, want the echoed payloads")
	}
}

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

	path := relay(t, l, 10, 0, false)
	client, servedSide := connectSeenAs(t, l, path.client, path.server, limits)

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

	conn.acknowledge(sequence, conn.now())
	if len(written) != 1 {
		t.Fatalf("%d frames were written, want 1", len(written))
	}

	if err := conn.write(Frame{Flags: FlagData, Sequence: sequence, Payload: payload}); err != nil {
		t.Fatalf("write() of a stale segment = %v, want it refused silently", err)
	}
	if len(written) != 1 {
		t.Errorf("%d frames were written, want the confirmed segment not to be sent again", len(written))
	}
	assertNoDrift(t, conn)

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

	if held.Counters().Duplicated == 0 {
		t.Error("Duplicated = 0, want the copied datagrams to have been recognised")
	}
}

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

	path := relay(t, l, 3, 0, false)
	client, _ := connectSeenAs(t, l, path.client, path.server, limits)
	for index := range 8 {
		roundTrip(t, client, fmt.Sprintf("counted %d", index))
	}

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

	if counters.Started.IsZero() {
		t.Error("Started is zero, want when the connection was built")
	}
	if !counters.Closed.IsZero() {
		t.Errorf("Closed = %s while the connection is open, want zero", counters.Closed)
	}
}

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

func encodedData(sequence uint32, payload string) []byte {
	frame, err := Frame{Flags: FlagData, Sequence: sequence, Payload: []byte(payload)}.Marshal()
	if err != nil {
		panic(err)
	}
	return frame
}

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

	const messages = 8

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

func TestAWriteEndsWhenTheWindowNeverOpens(t *testing.T) {
	t.Parallel()
	limits := testLimits()
	limits.MaxWindow = 2
	limits.MaxPayload = 256
	limits.IdleTimeout = 30 * time.Second
	limits.MaxRetransmits = 1000

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
