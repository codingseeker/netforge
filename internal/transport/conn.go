package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/codingseeker/netforge/internal/apperr"
)

var ErrClosed = errors.New("the connection is closed")

type Counters struct {
	Sent int

	Acknowledged int

	Retransmitted int

	Received int

	Duplicated int

	Dropped int

	Timeouts int

	Window uint32

	RTT time.Duration

	Jitter time.Duration

	Started time.Time

	Closed time.Time
}

type event struct {
	kind     string
	sequence uint32
	bytes    int
	retry    bool
	at       time.Time
}

type Conn struct {
	writeFrame func([]byte) error

	stop     func() error
	limits   Limits
	receiver *receiver
	control  *congestion

	mu sync.Mutex

	out map[uint32]*outstanding

	sentAt map[uint32]time.Time

	tried map[uint32]int

	nextSeq uint32

	lastAckSent uint32
	closed      bool
	err         error

	finSent     bool
	finReceived bool
	counters    Counters

	dataC chan struct{}

	windowC chan struct{}

	retryC chan struct{}

	idleAt time.Time

	events chan event
	done   chan struct{}

	doneOnce sync.Once

	closeOnce sync.Once

	local  net.Addr
	remote net.Addr
	now    func() time.Time
}

type outstanding struct {
	frame Frame
	at    time.Time
}

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

	_ = socket.SetReadBuffer(checked.MaxDatagram * checked.MaxReorder)
	conn := newConn(checked)
	conn.local, conn.remote = socket.LocalAddr(), socket.RemoteAddr()
	conn.writeFrame = func(frame []byte) error {
		_, err := socket.Write(frame)
		return err
	}
	conn.stop = socket.Close

	conn.start(ctx, func() {

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

				continue
			}
			conn.deliver(frame, conn.now())
		}
	})
	return conn, nil
}

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

	go func() {
		select {
		case <-ctx.Done():
			_ = l.Close()
		case <-l.watch:
		}
	}()
	return l, nil
}

type Listener struct {
	socket *net.UDPConn
	limits Limits
	ctx    context.Context

	watch chan struct{}

	mu      sync.Mutex
	closed  bool
	peers   map[string]*Conn
	connsAt int

	watchOnce sync.Once

	refused int
}

func (l *Listener) Serve(ctx context.Context) error {
	_, err := l.ServePeer(ctx)
	return err
}

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

	conn.start(ctx, nil)
	return conn, nil
}

func (l *Listener) resetTo(to *net.UDPAddr) {
	frame, err := Frame{Flags: FlagReset}.Marshal()
	if err != nil {
		return
	}
	_, _ = l.socket.WriteToUDP(frame, to)
}

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

func (l *Listener) Conn(addr string) (*Conn, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	conn, ok := l.peers[addr]
	return conn, ok
}

func (l *Listener) LocalAddr() net.Addr { return l.socket.LocalAddr() }

func (l *Listener) Count() (held, refused int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.connsAt, l.refused
}

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

func (c *Conn) start(ctx context.Context, read func()) {
	if read != nil {
		go read()
	}
	go c.retransmitLoop(ctx)
}

func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.counters.Closed = c.now()

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

func (c *Conn) onceFinish() {
	c.doneOnce.Do(func() { close(c.done) })
}

func (c *Conn) Done() <-chan struct{} { return c.done }

func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Conn) LocalAddr() net.Addr  { return c.local }
func (c *Conn) RemoteAddr() net.Addr { return c.remote }

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

func (c *Conn) Read(ctx context.Context) ([]byte, error) {
	for {

		c.mu.Lock()
		if segment := c.receiver.pop(); segment != nil {
			c.counters.Received += len(segment.payload)
			finished := c.finReceived && c.receiver.buffered() == 0
			c.mu.Unlock()
			if finished {

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

func (c *Conn) signalReader() {
	select {
	case c.dataC <- struct{}{}:
	default:
	}
}

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

func (c *Conn) write(frame Frame) error {

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

			c.out[frame.Sequence] = &outstanding{frame: frame, at: now}
			c.sentAt[frame.Sequence] = now
		case frame.Sequence <= c.control.acknowledged:

		default:
			c.tried[frame.Sequence] = 1
			c.control.sentOne()

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

func (c *Conn) confirmed(sequence uint32) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out[sequence] == nil && sequence <= c.control.acknowledged
}

func (c *Conn) note(e event) {
	select {
	case c.events <- e:
	default:
	}
}

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

		c.sendCurrentAck()
		return false
	case !frame.IsData():
		return false
	}

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

		_ = c.write(Frame{Flags: FlagAck, Ack: ack})
	}
	return false
}

func (c *Conn) sendCurrentAck() {
	c.mu.Lock()
	ack := c.receiver.ack()
	c.mu.Unlock()
	if ack == 0 {
		return
	}
	_ = c.write(Frame{Flags: FlagAck, Ack: ack})
}

func (c *Conn) acknowledge(ack uint32, now time.Time) {
	c.mu.Lock()
	c.idleAt = now
	if ack == 0 || ack < c.control.acknowledged {

		c.mu.Unlock()
		return
	}
	if ack > c.control.sent {

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

		select {
		case c.windowC <- struct{}{}:
		default:
		}
	}
}

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

			continue
		case <-timer.C:
			c.expire(c.now())
		}
	}
}

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

		return time.Millisecond
	}
	return wait
}

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

	for _, pending := range due {
		if err := c.resend(pending); err != nil {
			return
		}
	}
}

func (c *Conn) resend(pending *outstanding) error {
	sequence := pending.frame.Sequence
	c.mu.Lock()
	if c.out[sequence] != pending {

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

	now := c.now()
	c.mu.Lock()
	counted := c.out[sequence] != nil
	if counted {
		c.tried[sequence]++

		c.counters.Retransmitted++
		c.counters.Timeouts++

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

func (c *Conn) usable() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return c.errLocked()
	}
	return nil
}

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
