package transport

import (
	"encoding/binary"

	"github.com/codingseeker/netforge/internal/apperr"
)

const (
	Magic uint16 = 0x4E46

	Version uint8 = 1

	HeaderSize = 20

	MaxFrameSize = 1400

	MaxPayloadSize = MaxFrameSize - HeaderSize
)

const (
	FlagData uint8 = 1 << 0

	FlagAck uint8 = 1 << 1

	FlagProbe uint8 = 1 << 2

	FlagFin uint8 = 1 << 3

	FlagReset uint8 = 1 << 4
)

const validFlags = FlagData | FlagAck | FlagProbe | FlagFin | FlagReset

type Frame struct {
	Flags uint8

	Sequence uint32

	Ack uint32

	Payload []byte
}

func (f Frame) IsData() bool { return f.Flags&FlagData != 0 }

func (f Frame) IsAck() bool { return f.Flags&FlagAck != 0 }

func (f Frame) IsProbe() bool { return f.Flags&FlagProbe != 0 }

func (f Frame) IsFin() bool { return f.Flags&FlagFin != 0 }

func (f Frame) IsReset() bool { return f.Flags&FlagReset != 0 }

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

	copy(out[HeaderSize:], f.Payload)
	return out, nil
}

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

func kindIsWellFormed(f Frame) bool {
	switch {
	case f.Flags&FlagReset != 0:

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
