package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/netforge/netforge/internal/apperr"
)

// ErrClosed is returned by a connection that is no longer usable.
var ErrClosed = errors.New("the connection is closed")

// Counters are what a connection observed about the path it used. Every number
// here was measured, so they can be compared against a configured fault without
// the comparison being circular.
type Counters struct {
	// Sent is how many frames were written to the socket, including
	// retransmissions and acknowledgements.
	Sent int
	// Acknowledged is how many distinct segments were confirmed.
	Acknowledged int
	// Retransmitted is how many segments were sent more than once.
	Retransmitted int
	// Received is how many payload bytes were delivered to the caller.
	Received int
	// Duplicated is how many frames were refused because they had already been
	// seen.
	Duplicated int
	// Dropped is how many segments were discarded because the reorder buffer
	// was full or the payload was too large.
	Dropped int
	// Timeouts is how many retransmission deadlines expired.
	Timeouts int
	// Window is the congestion window the last segment was sent under.
	Window uint32
	// RTT is the most recent measured round trip.
	RTT time.Duration
	// Jitter is how far the most recent round trip moved from the smoothed one.
	Jitter time.Duration
	// Started is when the connection was built.
	Started time.Time
	// Closed is when it stopped, and is zero while it is open.
	Closed time.Time
}

// event is what a connection did, for a benchmark that needs the timeline
// rather than the totals.
type event struct {
	kind     string
	sequence uint32
	bytes    int
	retry    bool
	at       time.Time
}

// Conn is one reliable connection over UDP.
//
// A connection owns exactly two goroutines, both started when it is created and
// both stopped by Close, and one timer that is reused rather than one per
// segment. That is what makes "does not leak goroutines" checkable: a closed
// connection leaves nothing running.
//
// The sending side differs between a client and a served connection, because a
// client owns a connected socket while a served connection shares the
// listener's socket and writes to one peer address. Everything above that
// difference, including the window, the retransmission and the receiver, is the
// same code.
type Conn struct {
	// writeFrame puts one encoded frame on the wire towards the peer.
	writeFrame func([]byte) error
	// stop tears down whatever the connection is built on.
	stop     func() error
	limits   Limits
	receiver *receiver
	control  *congestion

	mu sync.Mutex
	// out holds the segments sent and not yet acknowledged.
	out map[uint32]*outstanding
	// sentAt is when each outstanding segment last went out, which is what the
	// round trip is measured from and what the timer compares against.
	sentAt map[uint32]time.Time
	// tried counts how many times each segment has been sent, which is what the
	// retransmit ceiling is checked against.
	tried map[uint32]int
	// nextSeq is the sequence the next segment carries.
	nextSeq uint32
	// lastAckSent is the highest acknowledgement already sent, so an unchanged
	// one is not repeated.
	lastAckSent uint32
	closed      bool
	err         error
	// finSent and finReceived track the two halves of an orderly shutdown.
	finSent     bool
	finReceived bool
	counters    Counters
	// dataC is a one-slot signal that a segment is waiting in the receiver's
	// queue. The queue itself lives in the receiver, because a caller that
	// stops reading must be bounded and must not hold the arrival path: putting
	// the segments on this connection's channel and blocking on it would let
	// one unread connection stall every other connection on a shared listener.
	dataC chan struct{}
	// windowC is signalled when an acknowledgement frees window space, so a
	// blocked sender wakes without consuming received data.
	windowC chan struct{}
	// retryC is signalled when a segment goes outstanding, so the
	// retransmission loop recomputes its deadline at once. Without it the loop
	// would sleep until whatever deadline it had already chosen, and a segment
	// sent just after it parked would wait the whole of the idle timeout before
	// its first retransmission: thirty seconds with the default limits, for a
	// loss the transport exists to recover from in a quarter of one.
	retryC chan struct{}
	// idleAt is when this connection last saw anything from its peer, which is
	// what the idle timeout is measured from. A peer that opens a connection
	// and then goes away would otherwise hold a socket, a goroutine and its
	// window slots for as long as the process lives.
	idleAt time.Time
	// events carries what was sent.
	events chan event
	done   chan struct{}
	// doneOnce closes done exactly once, whether the stop came from Close or
	// from a failure. A separate Once from the one guarding teardown matters:
	// a connection that failed must still be closeable afterwards.
	doneOnce sync.Once
	// closeOnce guards the teardown, which must happen exactly once.
	closeOnce sync.Once
	// local and remote are the socket's ends, kept because a served connection
	// shares a socket and has no address of its own to ask.
	local  net.Addr
	remote net.Addr
	now    func() time.Time
}

// outstanding is a segment that has been sent and not yet confirmed.
type outstanding struct {
	frame Frame
	at    time.Time
}

// Dial opens a connection to a peer. The socket is connected, so nothing this
// connection sends can reach an address other than the peer's and nothing it
// accepts can come from one.
func Dial(ctx context.Context, laddr, raddr string, limits Limits) (*Conn, error) {
	const op = "transport.Dial"
	checked, err := limits.Validate()
	if err != nil {
		return nil, err
	}
	local, err := net.ResolveUDPAddr("udp", laddr)
	if err != nil {
		return nil, apperr.InvalidArgument(op, "the local address is not a UDP address", err)
	}
	remote, err := net.ResolveUDPAddr("udp", raddr)
	if err != nil {
		return nil, apperr.InvalidArgument(op, "the peer address is not a UDP address", err)
	}
	socket, err := net.DialUDP("udp", local, remote)
	if err != nil {
		return nil, apperr.Wrap(err, apperr.KindIO, op, "the socket could not be opened")
	}
	// A read buffer larger than the ceiling would let the kernel queue frames
	// this transport then has to refuse, so it is sized to what the reorder
	// buffer can actually hold.
	_ = socket.SetReadBuffer(checked.MaxDatagram * checked.MaxReorder)
	conn := newConn(checked)
	conn.local, conn.remote = socket.LocalAddr(), socket.RemoteAddr()
	conn.writeFrame = func(frame []byte) error {
		_, err := socket.Write(frame)
		return err
	}
	conn.stop = socket.Close
	// The context is the connection's lifetime, not the caller's patience with
	// setting it up: a caller that cancels the context it dialled with has asked
	// for this connection to stop, and a connection that outlives its context is
	// a connection nothing is holding on to.
	conn.start(ctx, func() {
		// The client's own socket is its input, so the read loop lives here.
		buffer := make([]byte, checked.MaxDatagram)
		for {
			read, err := socket.Read(buffer)
			if err != nil {
				select {
				case <-conn.done:
				default:
					conn.fail(apperr.Wrap(err, apperr.KindIO, "transport.Dial",
						"the socket could not be read"))
				}
				return
			}
			frame, err := Unmarshal(buffer[:read])
			if err != nil {
				// A malformed frame is dropped without a reply: answering one
				// is how a parser is turned into a reflector, and a peer that
				// sent it by mistake will retransmit.
				continue
			}
			conn.deliver(frame, conn.now())
		}
	})
	return conn, nil
}

// Listen binds a socket and serves reliable connections on it. Every connection
// it serves shares the limits given here, and the number of them is bounded, so
// a peer that opens more than the limit is refused rather than served.
func Listen(ctx context.Context, laddr string, limits Limits) (*Listener, error) {
	const op = "transport.Listen"
	checked, err := limits.Validate()
	if err != nil {
		return nil, err
	}
	local, err := net.ResolveUDPAddr("udp", laddr)
	if err != nil {
		return nil, apperr.InvalidArgument(op, "the address is not a UDP address", err)
	}
	socket, err := net.ListenUDP("udp", local)
	if err != nil {
		return nil, apperr.Wrap(err, apperr.KindIO, op, "the socket could not be opened")
	}
	_ = socket.SetReadBuffer(checked.MaxDatagram * checked.MaxReorder)
	l := &Listener{socket: socket, limits: checked, ctx: ctx,
		peers: make(map[string]*Conn), watch: make(chan struct{})}
	// A read on a socket blocks until a datagram arrives or the socket is
	// closed, so a cancelled context would otherwise be noticed only when the
	// next peer happened to send something. Closing the socket is what turns
	// the cancellation into that, and it is the only reason this listener runs a
	// goroutine of its own. It is one per listener, not one per connection, and
	// it is stopped by Close.
	go func() {
		select {
		case <-ctx.Done():
			_ = l.Close()
		case <-l.watch:
		}
	}()
	return l, nil
}

// Listener serves connections on one socket.
type Listener struct {
	socket *net.UDPConn
	limits Limits
	ctx    context.Context

	// watch stops the cancellation watcher when the listener is closed by other
	// means, so that a long-lived process does not keep a goroutine per
	// listener that was opened and closed.
	watch chan struct{}

	mu      sync.Mutex
	closed  bool
	peers   map[string]*Conn
	connsAt int
	// watchOnce guards stopping the cancellation watcher.
	watchOnce sync.Once
	// refused counts the datagrams turned away because the connection limit
	// was reached, which is a limit doing its job rather than an error.
	refused int
}

// Serve reads one datagram and applies it to the connection it belongs to,
// creating that connection if this is a new peer. A datagram that cannot be
// parsed, or that comes from a peer over the connection limit, is turned away
// and reported by the error only when the caller asked to hear about it.
func (l *Listener) Serve(ctx context.Context) error {
	_, err := l.ServePeer(ctx)
	return err
}

// ServePeer reads one datagram and returns the connection it was applied to,
// which is what an echo endpoint needs: the datagram names its peer, so the
// answer has to be written on that peer's connection rather than on whichever
// one the endpoint happened to be holding. A datagram that was not a data
// frame returns a nil connection, because there is nothing to answer.
func (l *Listener) ServePeer(ctx context.Context) (*Conn, error) {
	if err := l.ctx.Err(); err != nil {
		return nil, apperr.Wrap(err, apperr.KindCanceled, "transport.Listener.ServePeer",
			"the listener was cancelled")
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, ErrClosed
	}
	l.mu.Unlock()

	buffer := make([]byte, l.limits.MaxDatagram)
	read, from, err := l.socket.ReadFromUDP(buffer)
	if err != nil {
		l.mu.Lock()
		closed := l.closed
		l.mu.Unlock()
		if closed {
			return nil, ErrClosed
		}
		return nil, apperr.Wrap(err, apperr.KindIO, "transport.Listener.ServePeer",
			"the datagram could not be read")
	}
	// The frame is parsed before the peer is believed, and an unparsable
	// datagram is dropped without a reply.
	frame, err := Unmarshal(buffer[:read])
	if err != nil {
		return nil, nil
	}
	conn, err := l.connectionFor(ctx, from)
	if err != nil {
		return nil, err
	}
	conn.deliver(frame, conn.now())
	return conn, nil
}

// connectionFor returns the connection a peer's datagram belongs to, creating
// one when the peer is new and the limit allows it.
func (l *Listener) connectionFor(ctx context.Context, from *net.UDPAddr) (*Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, ErrClosed
	}
	if existing, ok := l.peers[from.String()]; ok {
		return existing, nil
	}
	if l.connsAt >= l.limits.MaxConnections {
		// Over the limit. The peer is told and the datagram is dropped, and the
		// socket stays open, because one peer asking for too much is not a
		// reason to stop serving the others.
		l.refused++
		l.resetTo(from)
		return nil, apperr.LimitExceeded("transport.Listener.connectionFor", fmt.Sprintf(
			"the listener already holds %d connections", l.connsAt), nil)
	}
	conn := newConn(l.limits)
	conn.writeFrame = func(frame []byte) error {
		_, err := l.socket.WriteToUDP(frame, from)
		return err
	}
	conn.local, conn.remote = l.socket.LocalAddr(), from
	conn.stop = func() error { return l.remove(conn) }
	l.peers[from.String()] = conn
	l.connsAt++
	// A served connection has no read loop of its own: the listener is its
	// input, and it runs the retransmission timer only.
	conn.start(ctx, nil)
	return conn, nil
}

// resetTo answers a peer that is over the connection limit.
func (l *Listener) resetTo(to *net.UDPAddr) {
	frame, err := Frame{Flags: FlagReset}.Marshal()
	if err != nil {
		return
	}
	_, _ = l.socket.WriteToUDP(frame, to)
}

// remove takes a served connection out of the listener. It is idempotent, so
// closing a connection twice through either path is safe.
func (l *Listener) remove(conn *Conn) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := ""
	for address, existing := range l.peers {
		if existing == conn {
			key = address
			break
		}
	}
	if key == "" {
		return nil
	}
	delete(l.peers, key)
	l.connsAt--
	return nil
}

// Conn returns the connection held for a peer address.
func (l *Listener) Conn(addr string) (*Conn, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	conn, ok := l.peers[addr]
	return conn, ok
}

// LocalAddr reports the address the listener is bound to. A caller that asked
// for port zero is told which port it got, which is the only way a test or a
// laboratory that lets the kernel choose can find the listener at all.
func (l *Listener) LocalAddr() net.Addr { return l.socket.LocalAddr() }

// Count reports how many connections are held and how many datagrams have been
// refused for want of a connection slot.
func (l *Listener) Count() (held, refused int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.connsAt, l.refused
}

// Close stops the listener and every connection it holds. It is idempotent.
func (l *Listener) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.watchOnce.Do(func() { close(l.watch) })
	l.closed = true
	conns := make([]*Conn, 0, len(l.peers))
	for _, conn := range l.peers {
		conns = append(conns, conn)
	}
	l.peers = make(map[string]*Conn)
	l.connsAt = 0
	l.mu.Unlock()

	var firstErr error
	for _, conn := range conns {
		if err := conn.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := l.socket.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// newConn builds a connection that has not been attached to anything yet.
func newConn(limits Limits) *Conn {
	return &Conn{
		limits:   limits,
		receiver: newReceiver(limits),
		control:  newCongestion(limits),
		out:      make(map[uint32]*outstanding, limits.MaxWindow),
		sentAt:   make(map[uint32]time.Time, limits.MaxWindow),
		tried:    make(map[uint32]int, limits.MaxWindow),
		nextSeq:  1,
		dataC:    make(chan struct{}, 1),
		retryC:   make(chan struct{}, 1),
		windowC:  make(chan struct{}, 1),
		events:   make(chan event, limits.MaxWindow*2),
		done:     make(chan struct{}),
		counters: Counters{Started: time.Now(), Window: 2},
		now:      time.Now,
		idleAt:   time.Now(),
	}
}

// start runs the retransmission loop and, when the caller supplied one, the
// read loop that feeds this connection.
func (c *Conn) start(ctx context.Context, read func()) {
	if read != nil {
		go read()
	}
	go c.retransmitLoop(ctx)
}

// Close stops the connection. It is idempotent, and after it returns nothing
// this connection started is still running.
func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.counters.Closed = c.now()
		// An orderly close is attempted first, so a peer that is listening
		// learns the stream ended rather than waiting for a timeout.
		if !c.finSent {
			sequence := c.nextSeq
			c.finSent = true
			c.mu.Unlock()
			_ = c.write(Frame{Flags: FlagFin, Sequence: sequence})
			c.mu.Lock()
		}
		c.mu.Unlock()
		c.onceFinish()
		if c.stop != nil {
			err = c.stop()
		}
	})
	return err
}

// onceFinish closes the done channel exactly once, whichever path got here
// first, so a read error caused by a close is not reported as a second stop.
func (c *Conn) onceFinish() {
	c.doneOnce.Do(func() { close(c.done) })
}

// Done is closed when the connection stops for any reason.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err reports why the connection stopped, and is nil while it is open.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// LocalAddr and RemoteAddr report the socket's ends.
func (c *Conn) LocalAddr() net.Addr  { return c.local }
func (c *Conn) RemoteAddr() net.Addr { return c.remote }

// Write sends a payload, splitting it into segments the payload limit allows,
// and blocks while the window is full. Blocking there is the backpressure that
// keeps a fast sender from turning a slow path into a queue inside this
// process, which is the failure mode the window exists to prevent.
func (c *Conn) Write(ctx context.Context, payload []byte) (int, error) {
	const op = "transport.Conn.Write"
	if len(payload) == 0 {
		return 0, apperr.InvalidArgument(op, "an empty payload has nothing to send", nil)
	}
	if len(payload) > c.limits.MaxPayload*c.limits.MaxWindow {
		return 0, apperr.LimitExceeded(op, "the payload is larger than one window can carry", nil)
	}
	written := 0
	for len(payload) > 0 {
		if err := ctx.Err(); err != nil {
			return written, apperr.Wrap(err, apperr.KindCanceled, op, "the write was cancelled")
		}
		size := len(payload)
		if size > c.limits.MaxPayload {
			size = c.limits.MaxPayload
		}
		if err := c.sendOne(ctx, payload[:size]); err != nil {
			return written, err
		}
		payload = payload[size:]
		written += size
	}
	return written, nil
}

// sendOne puts one segment on the wire once the window allows it.
//
// The payload is copied before the frame is retained, because the frame stays
// in the outstanding set until the peer acknowledges it and may be sent again
// after that. Holding on to the caller's slice instead would resend whatever
// the caller has written into that memory since, which is how a sender that
// reuses one buffer, as io.Copy does, delivers another message's bytes.
func (c *Conn) sendOne(ctx context.Context, payload []byte) error {
	for {
		if err := c.usable(); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return apperr.Wrap(err, apperr.KindCanceled, "transport.Conn.sendOne",
				"the write was cancelled")
		}
		c.mu.Lock()
		ready := c.control.canSend()
		var seq uint32
		if ready {
			seq = c.nextSeq
			c.nextSeq++
		}
		c.mu.Unlock()
		if ready {
			return c.write(Frame{Flags: FlagData, Sequence: seq,
				Payload: append([]byte(nil), payload...)})
		}
		// The window is full. Wait for an acknowledgement rather than queueing
		// here, and never consume a received segment to do it: the inbound
		// direction is independent and data this connection has already
		// accepted must not be dropped to make room for outbound. The caller's
		// context is waited on as well as the window, so a write that is
		// abandoned while the path is full still ends rather than holding the
		// caller's goroutine until the peer answers or the connection dies.
		select {
		case <-c.done:
			return c.reason()
		case <-c.windowC:
		case <-ctx.Done():
			return apperr.Wrap(ctx.Err(), apperr.KindCanceled, "transport.Conn.sendOne",
				"the write was cancelled while the window was full")
		}
	}
}

// Read returns the next payload in order, blocking until one is delivered, the
// peer finishes sending, or the connection stops. Anything already delivered
// stays readable after a close, so a peer that finished sending does not lose
// what it sent.
func (c *Conn) Read(ctx context.Context) ([]byte, error) {
	for {
		// The queue is checked before every park and the signal is set after
		// the queue is filled, so a segment that arrives between the check and
		// the park cannot be missed.
		c.mu.Lock()
		if segment := c.receiver.pop(); segment != nil {
			c.counters.Received += len(segment.payload)
			finished := c.finReceived && c.receiver.buffered() == 0
			c.mu.Unlock()
			if finished {
				// The peer finished sending and the last thing it sent has now
				// been delivered, so the slot can go.
				_ = c.Close()
			}
			return segment.payload, nil
		}
		drained := c.receiver.drained()
		c.mu.Unlock()
		if drained {
			return nil, io.EOF
		}
		select {
		case <-c.dataC:
		case <-c.done:
			// Anything the peer sent before it stopped is still returned, so an
			// orderly end does not lose what was already in order.
			c.mu.Lock()
			segment := c.receiver.pop()
			if segment != nil {
				c.counters.Received += len(segment.payload)
				c.mu.Unlock()
				return segment.payload, nil
			}
			drained := c.receiver.drained()
			c.mu.Unlock()
			if drained {
				return nil, io.EOF
			}
			return nil, c.reason()
		case <-ctx.Done():
			return nil, apperr.Wrap(ctx.Err(), apperr.KindCanceled, "transport.Conn.Read",
				"the read was cancelled")
		}
	}
}

// signalReader tells a reader that the queue is no longer empty, without
// blocking the arrival path and without a second segment being lost: the token
// is dropped when one is already there, because one is all the reader needs.
func (c *Conn) signalReader() {
	select {
	case c.dataC <- struct{}{}:
	default:
	}
}

// Counters returns a copy of what this connection measured.
// ReceiveStats reports the state of the receive side: how much arrived, how
// much of it is waiting to be read, and how much is held behind a gap. A
// caller that has been told to expect data uses it to tell a path that sent
// nothing from a queue this side was never woken for.
func (c *Conn) ReceiveStats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.receiver.stats()
}

func (c *Conn) Counters() Counters {
	c.mu.Lock()
	defer c.mu.Unlock()
	snapshot := c.counters
	snapshot.Window = c.control.window
	snapshot.RTT = c.control.rtt
	snapshot.Jitter = c.control.rttVariance()
	return snapshot
}

// write encodes and sends one frame, recording it if it carries payload.
//
// Deciding whether a data frame is recorded and recording it happen under one
// acquisition of the connection's lock, because whether a segment is still
// wanted can change between the two: the retransmission loop collects the
// segments it will resend and then releases the lock to write them, so an
// acknowledgement for a segment already in that list can arrive in between.
// Recording such a segment would put one the peer has confirmed back into the
// outstanding set and count it as another segment in flight, and because the
// congestion window is the difference between two cumulative totals the error
// would never unwind: the sender would stop for good rather than slow down,
// reporting a connection with nothing outstanding that cannot send.
func (c *Conn) write(frame Frame) error {
	// The pre-check keeps a segment the peer has already confirmed off the wire
	// in the ordinary case. It cannot be the whole answer, because the peer may
	// confirm the segment between here and the bookkeeping under the lock, and
	// that second check is the one that keeps the accounting right.
	if frame.IsData() && c.confirmed(frame.Sequence) {
		return nil
	}
	encoded, err := frame.Marshal()
	if err != nil {
		return err
	}
	if c.writeFrame == nil {
		return ErrClosed
	}
	if err := c.writeFrame(encoded); err != nil {
		c.fail(apperr.Wrap(err, apperr.KindIO, "transport.Conn.write",
			"the frame could not be written"))
		return c.reason()
	}
	now := c.now()
	fresh := false
	c.mu.Lock()
	if frame.IsData() {
		switch {
		case c.out[frame.Sequence] != nil:
			// The segment is still outstanding and is being sent again, so its
			// attempt count is left alone: resetting it here would let a peer
			// that never answers be retransmitted to for as long as the process
			// lived instead of being abandoned.
			c.out[frame.Sequence] = &outstanding{frame: frame, at: now}
			c.sentAt[frame.Sequence] = now
		case frame.Sequence <= c.control.acknowledged:
			// The peer caught up with this segment before it was written. The
			// datagram has gone and will be refused as the duplicate it is, and
			// nothing is recorded: a sequence at or below the peer's cumulative
			// acknowledgement is a sequence this side has already been told
			// about, and a fresh segment always carries a higher one.
		default:
			c.tried[frame.Sequence] = 1
			c.control.sentOne()
			// Something new is outstanding, so the loop's deadline is no longer
			// the one it chose for an idle connection.
			fresh = true
			c.out[frame.Sequence] = &outstanding{frame: frame, at: now}
			c.sentAt[frame.Sequence] = now
		}
	}
	c.counters.Sent++
	c.counters.Window = c.control.window
	c.mu.Unlock()
	if fresh {
		select {
		case c.retryC <- struct{}{}:
		default:
		}
	}
	c.note(event{kind: "frame", sequence: frame.Sequence, bytes: len(frame.Payload), at: now})
	return nil
}

// confirmed reports whether the peer has already acknowledged a sequence and
// nothing is holding it. Only a cumulative acknowledgement is consulted, and a
// sequence is only ever handed out above the acknowledgement, so this is true
// for a retransmission the peer has caught up with and false for anything the
// sender is offering now.
func (c *Conn) confirmed(sequence uint32) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out[sequence] == nil && sequence <= c.control.acknowledged
}

// note records an event without stalling the connection behind a reader that
// is not draining. The channel is bounded and a full one drops the event,
// because the timeline is diagnostic and the data is not.
func (c *Conn) note(e event) {
	select {
	case c.events <- e:
	default:
	}
}

// deliver applies one frame from the peer. It is the only path a frame from the
// wire takes into this connection's state.
func (c *Conn) deliver(frame Frame, now time.Time) (stopped bool) {
	c.mu.Lock()
	c.idleAt = now
	c.mu.Unlock()
	switch {
	case frame.IsReset():
		c.fail(apperr.Wrap(ErrClosed, apperr.KindIO, "transport.Conn.deliver",
			"the peer reset the connection"))
		return true
	case frame.IsAck():
		c.acknowledge(frame.Ack, now)
		return false
	case frame.IsFin():
		// A fin means the peer has finished sending. This transport has no
		// half-close, so the connection is over rather than half over, and the
		// resources it holds are released now instead of at the idle timeout:
		// a server that served a client which then left must not sit on that
		// client's slot, or a burst of short connections would fill the limit
		// with peers that are already gone and refuse the ones arriving.
		//
		// What the peer already sent is still delivered, because the queue in
		// the receiver outlives the close. A frame still held behind a gap is
		// the one exception: closing there would report the end of the stream
		// while data the peer sent was waiting, so the close waits for the gap
		// to close instead.
		c.mu.Lock()
		c.finReceived = true
		c.receiver.close()
		waiting := c.receiver.buffered() > 0
		c.mu.Unlock()
		c.signalReader()
		if !waiting {
			_ = c.Close()
		}
		return false
	case frame.IsProbe():
		// A probe is answered with the current acknowledgement, which is how a
		// connection measures a path it has nothing to send on.
		c.sendCurrentAck()
		return false
	case !frame.IsData():
		return false
	}
	// The receiver is guarded by this connection's mutex because the reader,
	// the arrival path and the retransmission loop all reach it. Everything
	// that follows the arrival is a bounded, non-blocking step: this runs on the
	// read loop, which on a served connection is the loop serving every other
	// peer on the same socket.
	c.mu.Lock()
	duplicate, refused := c.receiver.accept(frame, now)
	c.counters.Duplicated = c.receiver.duplicated
	c.counters.Dropped = c.receiver.dropped
	ack := c.receiver.ack()
	moved := ack != 0 && ack != c.lastAckSent
	if moved {
		c.lastAckSent = ack
	}
	wake := c.receiver.waiting()
	c.mu.Unlock()

	if wake {
		c.signalReader()
	}
	if duplicate || refused || moved {
		// A duplicate is acknowledged again because the peer either missed the
		// first acknowledgement or the path copied the frame, and the answer is
		// the same either way. A refusal is acknowledged with what has actually
		// arrived, which is what makes the sender retransmit the segment that
		// had nowhere to go rather than continuing past it.
		_ = c.write(Frame{Flags: FlagAck, Ack: ack})
	}
	return false
}

// sendCurrentAck answers with the current acknowledgement even when it has not
// moved, which is what makes a probe a request rather than a state change.
func (c *Conn) sendCurrentAck() {
	c.mu.Lock()
	ack := c.receiver.ack()
	c.mu.Unlock()
	if ack == 0 {
		return
	}
	_ = c.write(Frame{Flags: FlagAck, Ack: ack})
}

// acknowledge applies a peer's cumulative acknowledgement, releasing the
// segments it covers and measuring the round trip of each.
func (c *Conn) acknowledge(ack uint32, now time.Time) {
	c.mu.Lock()
	c.idleAt = now
	if ack == 0 || ack < c.control.acknowledged {
		// Nothing to release. A retransmitted or a stale acknowledgement is
		// answered by doing nothing, which is also what makes the
		// acknowledgement safe to repeat.
		c.mu.Unlock()
		return
	}
	if ack > c.control.sent {
		// An acknowledgement of something never sent. It releases nothing,
		// because every outstanding segment is keyed by a sequence this side
		// chose, but it must not be allowed to move the congestion window as
		// though a lot of data had arrived, or a peer could inflate the window
		// and be answered with more data than the path carries.
		//
		// The ceiling is the number of segments sent rather than the highest
		// sequence handed out. A sequence is taken before its frame is written,
		// so while a write is in flight the two differ, and a peer's cumulative
		// acknowledgement of a sequence that is still being written would be
		// allowed through by the other one. That is not a small mistake: the
		// congestion window is the difference between two cumulative totals, so
		// acknowledging more than was sent leaves the window believing data is in
		// flight that is not, and the two totals are never reconciled again.
		c.counters.Dropped++
		c.mu.Unlock()
		return
	}
	released := 0
	for seq := range c.out {
		if seq > ack {
			continue
		}
		c.control.measure(now.Sub(c.sentAt[seq]))
		delete(c.out, seq)
		delete(c.sentAt, seq)
		delete(c.tried, seq)
		released++
		c.counters.Acknowledged++
	}
	c.control.acknowledgedOne(ack)
	c.counters.Window = c.control.window
	c.mu.Unlock()
	if released > 0 {
		c.note(event{kind: "ack", sequence: ack, bytes: released, at: now})
		// Wake a sender waiting for room, without consuming anything inbound.
		select {
		case c.windowC <- struct{}{}:
		default:
		}
	}
}

// retransmitLoop resends what the peer has not confirmed. It gives up when a
// segment has been retried too many times, which matters because without a
// ceiling a dead peer costs a retransmission every timeout for as long as the
// process lives.
func (c *Conn) retransmitLoop(ctx context.Context) {
	timer := time.NewTimer(c.limits.RetransmitTimeout)
	defer timer.Stop()
	for {
		wait := c.deadline()
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)
		select {
		case <-c.done:
			return
		case <-ctx.Done():
			c.fail(apperr.Wrap(ctx.Err(), apperr.KindCanceled, "transport.retransmitLoop",
				"the connection was cancelled"))
			return
		case <-c.retryC:
			// A segment went out, so the deadline just chosen was for a
			// connection with nothing to retransmit. It is recomputed at the top
			// of the loop rather than waited out.
			continue
		case <-timer.C:
			c.expire(c.now())
		}
	}
}

// deadline is how long until this connection next has to do something: either
// the oldest outstanding segment must be sent again, or a connection that has
// heard nothing for the idle timeout has to be stopped. Taking the earlier of
// the two is what makes the idle timeout a deadline rather than a suggestion.
func (c *Conn) deadline() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	wait := c.limits.IdleTimeout - c.now().Sub(c.idleAt)
	if len(c.out) > 0 {
		if retry := c.control.timeout(c.limits.RetransmitTimeout, c.limits.MaxRTT); retry < wait {
			wait = retry
		}
	}
	if wait < 0 {
		// A deadline already past still has to fire, so it is rounded up to
		// something the timer will accept rather than firing in a spin.
		return time.Millisecond
	}
	return wait
}

// expire runs when the deadline passes. A connection with nothing outstanding
// and no traffic from its peer for the idle timeout is stopped, which is the
// bound on what a peer can hold by opening a connection and walking away.
func (c *Conn) expire(now time.Time) {
	c.mu.Lock()
	idle := len(c.out) == 0 && now.Sub(c.idleAt) >= c.limits.IdleTimeout
	c.mu.Unlock()
	if idle {
		c.fail(apperr.Wrap(ErrClosed, apperr.KindTimeout, "transport.Conn.expire",
			fmt.Sprintf("the peer sent nothing for %s", c.limits.IdleTimeout)))
		return
	}
	c.retransmitDue(now)
}

// retransmitDue resends every segment whose deadline has passed.
func (c *Conn) retransmitDue(now time.Time) {
	c.mu.Lock()
	var due []*outstanding
	for _, pending := range c.out {
		if now.Sub(pending.at) < c.control.timeout(c.limits.RetransmitTimeout, c.limits.MaxRTT) {
			continue
		}
		due = append(due, pending)
	}
	c.mu.Unlock()

	// The attempts are made one segment at a time rather than in one pass under
	// the lock, because deciding that a segment is still unconfirmed and writing
	// it are the same decision: a segment confirmed while the rest of the batch
	// was being written must not be written afterwards, and only the check under
	// the lock can see that.
	for _, pending := range due {
		if err := c.resend(pending); err != nil {
			return
		}
	}
}

// resend puts one segment on the wire again, or gives up on it.
//
// Whether the segment is still outstanding and whether it has been tried too
// often are settled under one acquisition of the connection's lock, the socket
// write happens after it, and the attempt is only counted once the write is done
// and the segment is still there. Holding the lock across a write would stall the
// read loop of every other peer on a shared listener if that socket's send buffer
// were ever full, which is a cost no retransmission is worth.
func (c *Conn) resend(pending *outstanding) error {
	sequence := pending.frame.Sequence
	c.mu.Lock()
	if c.out[sequence] != pending {
		// The peer confirmed this segment while it was waiting to be written.
		// It is not sent and not counted, because nothing was lost and nothing
		// goes out.
		c.mu.Unlock()
		return nil
	}
	if c.tried[sequence] >= c.limits.MaxRetransmits {
		giveUp := apperr.Wrap(ErrClosed, apperr.KindTimeout, "transport.Conn.resend",
			fmt.Sprintf("a segment was not acknowledged after %d attempts", c.tried[sequence]))
		c.mu.Unlock()
		c.fail(giveUp)
		return giveUp
	}
	c.mu.Unlock()

	if err := c.write(pending.frame); err != nil {
		return err
	}

	// The attempt is counted only if the segment is still outstanding once the
	// frame is on the wire, so the counters describe frames that were written
	// rather than decisions that were taken. write replaces the entry with a
	// fresh one, so this asks whether the sequence is still there rather than
	// whether the same entry is: a segment confirmed while it was being written
	// left nothing behind, and one that is still there had a frame sent for it.
	now := c.now()
	c.mu.Lock()
	counted := c.out[sequence] != nil
	if counted {
		c.tried[sequence]++
		// Sent is counted where a frame goes on the wire, which is write. A
		// retransmission is counted here, once, so that a segment sent twice is
		// one retransmission rather than two.
		c.counters.Retransmitted++
		c.counters.Timeouts++
		// The loss is attributed to the segment that has been outstanding
		// longest, because that is the one whose absence the peer noticed, and
		// every segment collected in one pass shares a deadline, so the window
		// is cut once for the pass rather than once for each of its segments.
		c.control.timedOut(c.oldestLocked())
		c.counters.Window = c.control.window
	}
	c.mu.Unlock()
	if counted {
		c.note(event{kind: "retry", sequence: sequence,
			bytes: len(pending.frame.Payload), retry: true, at: now})
	}
	return nil
}

// oldestLocked is the lowest sequence still outstanding, which is the one a
// timeout is attributed to.
func (c *Conn) oldestLocked() uint32 {
	var lowest uint32
	first := true
	for seq := range c.out {
		if first || seq < lowest {
			lowest, first = seq, false
		}
	}
	return lowest
}

// usable reports whether the connection can still be written to.
func (c *Conn) usable() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return c.errLocked()
	}
	return nil
}

// reason reports why the connection stopped, defaulting to a closed connection.
func (c *Conn) reason() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.errLocked()
}

func (c *Conn) errLocked() error {
	if c.err != nil {
		return c.err
	}
	return ErrClosed
}

// fail records why the connection stopped and stops it. The first reason is
// kept, because a read error caused by the close that followed it is a
// consequence rather than a cause.
func (c *Conn) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.closed = true
	c.counters.Closed = c.now()
	c.mu.Unlock()
	c.onceFinish()
}
