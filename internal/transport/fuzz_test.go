package transport

import "testing"

// FuzzUnmarshal requires that no datagram, however malformed, can panic the
// decoder or produce a frame the sender would not have written. The second half
// matters as much as the first: a frame that decodes but re-encodes differently
// is a frame two sides of this transport would disagree about.
func FuzzUnmarshal(f *testing.F) {
	seeds := [][]byte{
		nil,
		{},
		{0x4E},
		{0x4E, 0x46, Version, FlagData, 0, 0, 0, 1, 0, 0, 0, 0, 0, HeaderSize},
		{0x4E, 0x46, Version, FlagAck, 0, 0, 0, 0, 0, 0, 0, 9, 0, HeaderSize},
		{0x4E, 0x46, Version, FlagData, 0xFF, 0xFF, 0xFF, 0xFF, 0, 0, 0, 0, 0xFF, 0xFF},
		{0x4E, 0x46, Version, FlagFin, 0, 0, 0, 1, 0, 0, 0, 0, 0, HeaderSize},
		{0x4E, 0x46, Version, FlagReset, 0, 0, 0, 0, 0, 0, 0, 0, 0, HeaderSize},
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, datagram []byte) {
		frame, err := Unmarshal(datagram)
		if err != nil {
			// A refused datagram must not have produced anything usable, and
			// refusing must not have panicked on the way.
			if frame.Payload != nil {
				t.Fatalf("a refused datagram produced payload %q", frame.Payload)
			}
			return
		}
		if len(datagram) > MaxFrameSize {
			t.Fatalf("a %d byte datagram was accepted, want at most %d",
				len(datagram), MaxFrameSize)
		}
		if len(frame.Payload) > MaxPayloadSize {
			t.Fatalf("the decoded payload is %d bytes, want at most %d",
				len(frame.Payload), MaxPayloadSize)
		}
		// Whatever was accepted has to re-encode to something this decoder
		// accepts again, or the two halves of the transport disagree.
		encoded, err := frame.Marshal()
		if err != nil {
			t.Fatalf("an accepted frame did not re-encode: %v", err)
		}
		if len(encoded) > MaxFrameSize {
			t.Fatalf("a re-encoded frame is %d bytes, want at most %d", len(encoded), MaxFrameSize)
		}
		again, err := Unmarshal(encoded)
		if err != nil {
			t.Fatalf("a re-encoded frame did not decode again: %v", err)
		}
		if again.Flags != frame.Flags || again.Sequence != frame.Sequence || again.Ack != frame.Ack {
			t.Fatalf("a frame changed across a round trip: %+v became %+v", frame, again)
		}
		if len(again.Payload) != len(frame.Payload) {
			t.Fatalf("the payload changed length across a round trip: %d became %d",
				len(frame.Payload), len(again.Payload))
		}
	})
}

// FuzzFrameMarshal requires that no frame a caller can describe is encoded into
// a buffer larger than the ceiling, whatever the flags and lengths say.
func FuzzFrameMarshal(f *testing.F) {
	f.Add(uint8(FlagData), uint32(1), []byte("payload"))
	f.Add(uint8(FlagAck), uint32(7), []byte(nil))
	f.Add(uint8(FlagData|FlagAck), uint32(1), []byte("x"))
	f.Add(uint8(1<<7), uint32(0), []byte("x"))
	f.Fuzz(func(t *testing.T, flags uint8, sequence uint32, payload []byte) {
		encoded, err := Frame{Flags: flags, Sequence: sequence, Payload: payload}.Marshal()
		if err != nil {
			if encoded != nil {
				t.Fatalf("a refused frame produced %d bytes", len(encoded))
			}
			return
		}
		if len(encoded) > MaxFrameSize {
			t.Fatalf("Marshal() produced %d bytes, want at most %d", len(encoded), MaxFrameSize)
		}
		if _, err := Unmarshal(encoded); err != nil {
			t.Fatalf("Marshal() produced a frame Unmarshal() refuses: %v", err)
		}
	})
}
