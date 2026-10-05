package transport

import (
	"time"

	"github.com/netforge/netforge/internal/apperr"
)

// Limits bound one connection. Every number a peer could influence is bounded
// here, and a limit that a configuration cannot raise is a limit that a
// misconfiguration cannot turn into an allocation.
type Limits struct {
	// MaxPayload is the largest payload one segment may carry.
	MaxPayload int
	// MaxWindow is the most unacknowledged segments that may be in flight.
	// This is the bound on memory a sender may hold on the peer's behalf.
	MaxWindow int
	// MaxReorder is how many out-of-order segments the receiver may hold
	// waiting for a gap to be filled. This is the bound on memory a receiver
	// may spend on a peer that reorders.
	MaxReorder int
	// MaxDatagram is the largest datagram read from the socket.
	MaxDatagram int
	// RetransmitTimeout is how long a segment waits for an acknowledgement
	// before it is sent again.
	RetransmitTimeout time.Duration
	// MaxRetransmits is how many times one segment may be sent again before
	// the connection is abandoned. Without it a dead peer costs a
	// retransmission every timeout forever.
	MaxRetransmits int
	// HandshakeTimeout bounds the whole connect, so a peer that never answers
	// costs one timeout rather than the process's lifetime.
	HandshakeTimeout time.Duration
	// IdleTimeout closes a connection that has carried nothing for this long.
	IdleTimeout time.Duration
	// MaxConnections is how many connections one endpoint may hold.
	MaxConnections int
	// MaxRTT is the ceiling on the retransmission timeout derived from a
	// measured round trip, which stops one slow sample from stretching the
	// timeout past what the caller agreed to wait.
	MaxRTT time.Duration
}

// DefaultLimits returns the limits used when a caller configures none. They
// are sized for a laboratory path rather than for a datacentre: a window wide
// enough to fill a 10 ms path, a reorder buffer deep enough to absorb the
// reordering a lab queue produces, and a retransmit timeout above the latency
// the lab itself injects.
func DefaultLimits() Limits {
	return Limits{
		MaxPayload:        MaxPayloadSize,
		MaxWindow:         64,
		MaxReorder:        64,
		MaxDatagram:       MaxFrameSize,
		RetransmitTimeout: 250 * time.Millisecond,
		MaxRetransmits:    8,
		HandshakeTimeout:  5 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxConnections:    64,
		MaxRTT:            2 * time.Second,
	}
}

// Validate fills in the defaults and refuses limits that would make the
// transport's guarantees meaningless or its memory unbounded. It is called
// before a connection exists, so a rejected set of limits has no effect
// anywhere.
func (l Limits) Validate() (Limits, error) {
	const op = "transport.Limits.Validate"
	fallback := DefaultLimits()
	if l.MaxPayload <= 0 {
		l.MaxPayload = fallback.MaxPayload
	}
	if l.MaxWindow <= 0 {
		l.MaxWindow = fallback.MaxWindow
	}
	if l.MaxReorder <= 0 {
		l.MaxReorder = fallback.MaxReorder
	}
	if l.MaxDatagram <= 0 {
		l.MaxDatagram = fallback.MaxDatagram
	}
	if l.RetransmitTimeout <= 0 {
		l.RetransmitTimeout = fallback.RetransmitTimeout
	}
	if l.MaxRetransmits <= 0 {
		l.MaxRetransmits = fallback.MaxRetransmits
	}
	if l.HandshakeTimeout <= 0 {
		l.HandshakeTimeout = fallback.HandshakeTimeout
	}
	if l.IdleTimeout <= 0 {
		l.IdleTimeout = fallback.IdleTimeout
	}
	if l.MaxConnections <= 0 {
		l.MaxConnections = fallback.MaxConnections
	}
	if l.MaxRTT <= 0 {
		l.MaxRTT = fallback.MaxRTT
	}
	// A payload larger than a frame can hold would be accepted by the limits
	// and then refused by every send, which is a configuration that appears to
	// work and does not.
	if l.MaxPayload > MaxPayloadSize {
		return l, apperr.LimitExceeded(op, "the payload limit is larger than a frame can carry", nil)
	}
	if l.MaxDatagram > MaxFrameSize {
		return l, apperr.LimitExceeded(op, "the datagram limit is larger than this transport reads", nil)
	}
	if l.MaxDatagram < HeaderSize+l.MaxPayload {
		return l, apperr.InvalidArgument(op, "the datagram limit cannot hold the header and a full payload", nil)
	}
	if l.MaxRTT < l.RetransmitTimeout {
		return l, apperr.InvalidArgument(op, "the round trip ceiling is below the "+
			"retransmission timeout it bounds", nil)
	}
	if l.HandshakeTimeout < l.RetransmitTimeout {
		return l, apperr.InvalidArgument(op, "the handshake timeout is shorter than one retransmission", nil)
	}
	return l, nil
}
