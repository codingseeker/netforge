package transport

import (
	"time"

	"github.com/netforge/netforge/internal/apperr"
)

type Limits struct {
	MaxPayload int

	MaxWindow int

	MaxReorder int

	MaxDatagram int

	RetransmitTimeout time.Duration

	MaxRetransmits int

	HandshakeTimeout time.Duration

	IdleTimeout time.Duration

	MaxConnections int

	MaxRTT time.Duration
}

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
