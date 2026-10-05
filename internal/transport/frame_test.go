package transport

import (
	"bytes"
	"strings"
	"testing"
)

// A frame has to survive a round trip through the wire format, because the two
// sides of this transport are the only implementations of it.
func TestAFrameSurvivesTheWire(t *testing.T) {
	t.Parallel()
	cases := []Frame{
		{Flags: FlagData, Sequence: 1, Payload: []byte("hello")},
		{Flags: FlagData, Sequence: 4294967295, Payload: bytes.Repeat([]byte{0xAB}, MaxPayloadSize)},
		{Flags: FlagData, Sequence: 7, Payload: nil},
		{Flags: FlagAck, Ack: 12},
		{Flags: FlagAck, Ack: 4294967295},
		{Flags: FlagProbe},
		{Flags: FlagFin, Sequence: 99},
		{Flags: FlagReset},
	}
	for _, give := range cases {
		encoded, err := give.Marshal()
		if err != nil {
			t.Errorf("Marshal(%+v) = %v, want nil", give, err)
			continue
		}
		if len(encoded) > MaxFrameSize {
			t.Errorf("Marshal(%+v) = %d bytes, want at most %d", give, len(encoded), MaxFrameSize)
		}
		back, err := Unmarshal(encoded)
		if err != nil {
			t.Errorf("Unmarshal(%+v) = %v, want nil", give, err)
			continue
		}
		if back.Flags != give.Flags || back.Sequence != give.Sequence || back.Ack != give.Ack {
			t.Errorf("Unmarshal(Marshal(%+v)) = %+v, want the same frame", give, back)
		}
		if !bytes.Equal(back.Payload, give.Payload) && !(len(back.Payload) == 0 && len(give.Payload) == 0) {
			t.Errorf("Unmarshal(Marshal(%+v)).Payload = %q, want %q", give, back.Payload, give.Payload)
		}
	}
}

// A datagram that is not one of this transport's frames must be refused, and
// the refusal must not depend on what is in it.
func TestAFrameThatIsNotOursIsRefused(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		datagram []byte
	}{
		{"empty", nil},
		{"one byte", []byte{0x4E}},
		{"a header with no body", make([]byte, HeaderSize)},
		{"the wrong magic", func() []byte {
			out := make([]byte, HeaderSize+4)
			out[0], out[1] = 0xFF, 0xFF
			return out
		}()},
		{"a future version", func() []byte {
			out := make([]byte, HeaderSize+4)
			out[0], out[1] = 0x4E, 0x46
			out[2] = 99
			return out
		}()},
		{"an undefined flag", func() []byte {
			out := make([]byte, HeaderSize+4)
			out[0], out[1] = 0x4E, 0x46
			out[2] = Version
			out[3] = 1 << 7
			return out
		}()},
		{"a length shorter than the header", func() []byte {
			out := make([]byte, HeaderSize+4)
			out[0], out[1] = 0x4E, 0x46
			out[2] = Version
			out[3] = FlagData
			out[12], out[13] = 0, 3
			return out
		}()},
		{"a length longer than the datagram", func() []byte {
			out := make([]byte, HeaderSize+4)
			out[0], out[1] = 0x4E, 0x46
			out[2] = Version
			out[3] = FlagAck
			out[12], out[13] = 0xFF, 0xFF
			return out
		}()},
		{"trailing bytes past the declared length", func() []byte {
			out := make([]byte, HeaderSize+8)
			out[0], out[1] = 0x4E, 0x46
			out[2] = Version
			out[3] = FlagAck
			out[12], out[13] = 0, byte(HeaderSize)
			return out
		}()},
		{"a data frame with sequence zero", func() []byte {
			out := make([]byte, HeaderSize+1)
			out[0], out[1] = 0x4E, 0x46
			out[2] = Version
			out[3] = FlagData
			out[14] = 0xFF
			return out
		}()},
		{"data and ack together", func() []byte {
			out := make([]byte, HeaderSize+1)
			out[0], out[1] = 0x4E, 0x46
			out[2] = Version
			out[3] = FlagData | FlagAck
			out[4] = 0
			out[5] = 0
			out[6] = 0
			out[7] = 1
			out[14] = 0xFF
			return out
		}()},
		{"a reset carrying payload", func() []byte {
			out := make([]byte, HeaderSize+1)
			out[0], out[1] = 0x4E, 0x46
			out[2] = Version
			out[3] = FlagReset
			out[14] = 0xFF
			return out
		}()},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Unmarshal(test.datagram); err == nil {
				t.Errorf("Unmarshal(%s) = nil error, want a refusal", test.name)
			}
		})
	}
}

// A peer is not trusted to keep its datagrams small. The size is checked before
// anything is read out of the buffer.
func TestAnOversizedDatagramIsRefusedBeforeItIsRead(t *testing.T) {
	t.Parallel()
	oversized := make([]byte, MaxFrameSize+1)
	oversized[0], oversized[1] = 0x4E, 0x46
	if _, err := Unmarshal(oversized); err == nil {
		t.Error("Unmarshal() of an oversized datagram = nil error, want a refusal")
	} else if !strings.Contains(err.Error(), "larger") {
		t.Errorf("Unmarshal() = %v, want it to say the datagram is too large", err)
	}
}

// The payload ceiling is enforced on the way out as well as on the way in, so a
// caller cannot make this side build a buffer larger than a frame.
func TestAPayloadLargerThanAFrameIsRefused(t *testing.T) {
	t.Parallel()
	oversized := Frame{Flags: FlagData, Sequence: 1, Payload: make([]byte, MaxPayloadSize+1)}
	if _, err := oversized.Marshal(); err == nil {
		t.Error("Marshal() of an oversized payload = nil error, want a refusal")
	}
	exact := Frame{Flags: FlagData, Sequence: 1, Payload: make([]byte, MaxPayloadSize)}
	encoded, err := exact.Marshal()
	if err != nil {
		t.Fatalf("Marshal() of a full payload = %v, want nil", err)
	}
	if len(encoded) != MaxFrameSize {
		t.Errorf("Marshal() = %d bytes, want %d", len(encoded), MaxFrameSize)
	}
}

// Encoding must not depend on anything outside the frame, so a frame encoded
// twice is byte-identical and a peer cannot be confused by padding.
func TestEncodingIsDeterministic(t *testing.T) {
	t.Parallel()
	give := Frame{Flags: FlagData, Sequence: 42, Payload: []byte("payload")}
	first, err := give.Marshal()
	if err != nil {
		t.Fatalf("Marshal() = %v, want nil", err)
	}
	for range 4 {
		again, err := give.Marshal()
		if err != nil {
			t.Fatalf("Marshal() = %v, want nil", err)
		}
		if !bytes.Equal(first, again) {
			t.Fatal("Marshal() produced different bytes for the same frame")
		}
	}
	// The reserved bytes are written as zero rather than left as they were.
	if first[14] != 0 || first[15] != 0 || first[16] != 0 || first[17] != 0 {
		t.Errorf("the reserved bytes are %v, want them written as zero", first[14:18])
	}
}

// Bytes a frame does not describe are refused rather than ignored. Accepting
// them would leave the length of a frame meaning two things, and a peer that
// can choose where a payload starts can choose what a peer reads.
func TestBytesPastTheDeclaredLengthAreRefused(t *testing.T) {
	t.Parallel()
	encoded, err := Frame{Flags: FlagData, Sequence: 3, Payload: []byte("abc")}.Marshal()
	if err != nil {
		t.Fatalf("Marshal() = %v, want nil", err)
	}
	padded := append(append([]byte(nil), encoded...), make([]byte, 64)...)
	if _, err := Unmarshal(padded); err == nil {
		t.Error("Unmarshal() of a padded frame = nil error, want a refusal")
	}
	// The frame without the padding is still accepted, so the refusal above is
	// about the disagreement and not about the frame itself.
	if _, err := Unmarshal(encoded); err != nil {
		t.Errorf("Unmarshal() = %v, want nil", err)
	}
}
