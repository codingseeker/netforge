// Package transport implements a bounded reliable transport over UDP.
//
// UDP does not retransmit, does not order, does not deduplicate and does not
// know when a peer has stopped listening. A laboratory that wants to measure
// what an application does when the path misbehaves needs a transport that
// behaves predictably under loss, reordering and duplication, and this one is
// that transport: sequence numbers, cumulative acknowledgements, a sliding
// window, retransmission on a timer, duplicate suppression, and congestion
// control that slows down when the path stops confirming delivery.
//
// Everything here is bounded. A peer is not trusted to send a well-formed
// frame, a small frame, or a frame this connection asked for: the receive path
// allocates nothing before a length has been checked, the reorder buffer holds
// a fixed number of segments, the window cannot grow past its ceiling, and a
// connection that runs out of either fails rather than growing. The package
// holds no global state and no goroutine of its own beyond the read loop a
// caller starts, so a connection that is closed leaves nothing running.
package transport

import (
	"encoding/binary"

	"github.com/netforge/netforge/internal/apperr"
)

// The wire format. A frame is a fixed header followed by a payload, and both
// halves are bounded before either is used.
const (
	// Magic is the first two bytes of every frame. A datagram that does not
	// begin with it was not meant for this transport and is dropped rather
	// than parsed, so an unrelated UDP service on the same port is not fed
	// into a parser.
	Magic uint16 = 0x4E46
	// Version is the frame version this build speaks.
	Version uint8 = 1
	// HeaderSize is the size of a frame header in bytes.
	HeaderSize = 20
	// MaxFrameSize is the largest datagram this transport reads or writes. It
	// is below the IPv4 reassembly limit on purpose: a frame this transport
	// accepts is always delivered whole, and one it could not deliver whole is
	// never sent.
	MaxFrameSize = 1400
	// MaxPayloadSize is the largest payload a frame may carry, which is the
	// frame ceiling less the header.
	MaxPayloadSize = MaxFrameSize - HeaderSize
)

// Frame flags. A frame carries exactly one kind, so a frame whose flags
// contradict its kind is refused rather than guessed at.
const (
	// FlagData carries payload.
	FlagData uint8 = 1 << 0
	// FlagAck acknowledges delivery up to a sequence number.
	FlagAck uint8 = 1 << 1
	// FlagProbe asks a peer to acknowledge without sending payload. It is how
	// a connection measures the path when it has nothing to send.
	FlagProbe uint8 = 1 << 2
	// FlagFin closes the sending side after everything before it.
	FlagFin uint8 = 1 << 3
	// FlagReset abandons the connection immediately, and is the answer to a
	// frame this transport cannot use.
	FlagReset uint8 = 1 << 4
)

// validFlags is every flag this build understands. A frame with a bit set
// outside this mask was written by something else or corrupted, and either way
// its length cannot be trusted.
const validFlags = FlagData | FlagAck | FlagProbe | FlagFin | FlagReset

// Frame is one datagram on the wire.
type Frame struct {
	// Flags says what kind of frame this is.
	Flags uint8
	// Sequence is the position of a data frame in the stream, counting from
	// one. It is zero for a frame that carries no payload.
	Sequence uint32
	// Ack acknowledges every sequence up to and including this one, so an
	// acknowledgement is cumulative and a lost acknowledgement costs a
	// retransmission rather than correctness.
	Ack uint32
	// Payload is the data a data frame carries.
	Payload []byte
}

// IsData reports whether the frame carries payload.
func (f Frame) IsData() bool { return f.Flags&FlagData != 0 }

// IsAck reports whether the frame acknowledges delivery.
func (f Frame) IsAck() bool { return f.Flags&FlagAck != 0 }

// IsProbe reports whether the frame asks for an acknowledgement.
func (f Frame) IsProbe() bool { return f.Flags&FlagProbe != 0 }

// IsFin reports whether the frame closes the sending side.
func (f Frame) IsFin() bool { return f.Flags&FlagFin != 0 }

// IsReset reports whether the frame abandons the connection.
func (f Frame) IsReset() bool { return f.Flags&FlagReset != 0 }

// Marshal encodes a frame. The length of the result is decided by the frame,
// never by the caller, so a caller cannot make this allocate.
func (f Frame) Marshal() ([]byte, error) {
	const op = "transport.Frame.Marshal"
	if f.Flags&^validFlags != 0 {
		return nil, apperr.InvalidArgument(op, "the frame sets a flag this transport does not define", nil)
	}
	if !kindIsWellFormed(f) {
		return nil, apperr.InvalidArgument(op, "the frame contradicts its own kind", nil)
	}
	if len(f.Payload) > MaxPayloadSize {
		return nil, apperr.LimitExceeded(op, "the payload is larger than a frame may carry", nil)
	}
	// The length is checked against the ceiling here as well as in the
	// arguments, so the encoding step itself cannot produce an oversized
	// buffer even if this build is changed to add a field.
	length := HeaderSize + len(f.Payload)
	if length > MaxFrameSize {
		return nil, apperr.LimitExceeded(op, "the frame would be larger than a datagram may be", nil)
	}
	out := make([]byte, length)
	binary.BigEndian.PutUint16(out[0:2], Magic)
	out[2] = Version
	out[3] = f.Flags
	binary.BigEndian.PutUint32(out[4:8], f.Sequence)
	binary.BigEndian.PutUint32(out[8:12], f.Ack)
	binary.BigEndian.PutUint16(out[12:14], uint16(length))
	// Bytes 14 to 16 are reserved and written as zero, so a frame built here
	// is byte-identical to one this transport will accept later.
	copy(out[HeaderSize:], f.Payload)
	return out, nil
}

// Unmarshal decodes a frame. The datagram length is checked before anything is
// copied out of it, and the payload slice aliases the caller's buffer rather
// than copying it, because a receive path that copied every datagram would be
// a receive path that allocates once per packet.
func Unmarshal(datagram []byte) (Frame, error) {
	const op = "transport.Unmarshal"
	if len(datagram) > MaxFrameSize {
		return Frame{}, apperr.LimitExceeded(op, "the datagram is larger than this transport reads", nil)
	}
	if len(datagram) < HeaderSize {
		return Frame{}, apperr.InvalidArgument(op, "the datagram is too short to be a frame", nil)
	}
	if binary.BigEndian.Uint16(datagram[0:2]) != Magic {
		return Frame{}, apperr.InvalidArgument(op, "the datagram was not sent by this transport", nil)
	}
	if version := datagram[2]; version != Version {
		return Frame{}, apperr.Unsupported(op, "the frame speaks a version this build does not", nil)
	}
	frame := Frame{Flags: datagram[3]}
	if frame.Flags&^validFlags != 0 {
		return Frame{}, apperr.InvalidArgument(op, "the frame sets a flag this transport does not define", nil)
	}
	frame.Sequence = binary.BigEndian.Uint32(datagram[4:8])
	frame.Ack = binary.BigEndian.Uint32(datagram[8:12])
	length := int(binary.BigEndian.Uint16(datagram[12:14]))
	// The declared length has to be the datagram's length exactly. A frame that
	// claims to be shorter than the header is malformed, one that claims to be
	// longer than the datagram is truncated, and one that claims to be shorter
	// than the datagram has bytes after it that nothing describes. Refusing all
	// three is what keeps a frame's length from meaning two things, which is
	// the ambiguity that lets a peer choose where a payload starts.
	if length < HeaderSize {
		return Frame{}, apperr.InvalidArgument(op, "the frame declares a length shorter than its header", nil)
	}
	if length > MaxFrameSize {
		return Frame{}, apperr.LimitExceeded(op, "the frame declares more bytes than this transport reads", nil)
	}
	if length != len(datagram) {
		return Frame{}, apperr.InvalidArgument(op, "the frame and the datagram disagree about the length", nil)
	}
	if !kindIsWellFormed(frame) {
		return Frame{}, apperr.InvalidArgument(op, "the frame contradicts its own kind", nil)
	}
	frame.Payload = datagram[HeaderSize:length]
	return frame, nil
}

// kindIsWellFormed reports whether a frame's flags and fields agree. A data
// frame has a sequence and a payload, an acknowledgement has no payload, and
// neither may be combined with the flags that end a connection, because a peer
// that sent both would be describing two different things at once.
func kindIsWellFormed(f Frame) bool {
	switch {
	case f.Flags&FlagReset != 0:
		// A reset carries nothing and is always allowed, because its whole
		// purpose is to be understood when nothing else can be.
		return f.Flags == FlagReset
	case f.Flags&FlagFin != 0:
		return f.Flags == FlagFin && f.Sequence > 0
	case f.Flags&FlagData != 0:
		if f.Flags != FlagData {
			return false
		}
		return f.Sequence > 0
	case f.Flags&FlagProbe != 0:
		return f.Flags == FlagProbe
	case f.Flags&FlagAck != 0:
		return f.Flags == FlagAck
	default:
		return false
	}
}
