package transport

import (
	"testing"
	"time"
)

// A configuration that leaves a limit out must get a working one, because a
// transport that refuses to start unless every number is set is one nobody
// starts.
func TestUnsetLimitsFallBackToWorkingOnes(t *testing.T) {
	t.Parallel()
	checked, err := Limits{}.Validate()
	if err != nil {
		t.Fatalf("Validate() = %v, want nil for an empty configuration", err)
	}
	fallback := DefaultLimits()
	if checked != fallback {
		t.Errorf("Validate() = %+v, want the defaults %+v", checked, fallback)
	}
}

// Every limit a peer could push on has to be bounded, and a configuration that
// would make one unbounded has to be refused before a connection exists.
func TestLimitsThatWouldBreakTheTransportAreRefused(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		limits Limits
	}{
		{"a payload larger than a frame", Limits{MaxPayload: MaxPayloadSize + 1}},
		{"a datagram larger than a frame", Limits{MaxDatagram: MaxFrameSize + 1}},
		{"a datagram that cannot hold a full payload",
			Limits{MaxPayload: 1024, MaxDatagram: HeaderSize + 512}},
		{"a round trip ceiling below the timeout it bounds",
			Limits{RetransmitTimeout: time.Second, MaxRTT: 500 * time.Millisecond}},
		{"a handshake shorter than one retransmission",
			Limits{RetransmitTimeout: 2 * time.Second, HandshakeTimeout: time.Second}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.limits.Validate(); err == nil {
				t.Error("Validate() = nil, want a refusal")
			}
		})
	}
}

// The fallback is only applied to a limit that was left out. A limit that was
// given is used as given, or refused, never quietly replaced: a caller that set
// a small window has to get a small window.
func TestAGivenLimitIsNotReplacedByTheDefault(t *testing.T) {
	t.Parallel()
	checked, err := Limits{MaxWindow: 4, MaxReorder: 3, MaxPayload: 128}.Validate()
	if err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	if checked.MaxWindow != 4 {
		t.Errorf("MaxWindow = %d, want the 4 that was given", checked.MaxWindow)
	}
	if checked.MaxReorder != 3 {
		t.Errorf("MaxReorder = %d, want the 3 that was given", checked.MaxReorder)
	}
	if checked.MaxPayload != 128 {
		t.Errorf("MaxPayload = %d, want the 128 that was given", checked.MaxPayload)
	}
}

// The defaults are the numbers the rest of the transport is reasoned about, and
// they are derived from the frame ceiling rather than chosen next to it, so a
// change to the frame cannot leave the defaults describing a frame that no
// longer exists.
func TestTheDefaultsMatchTheFrame(t *testing.T) {
	t.Parallel()
	checked, err := DefaultLimits().Validate()
	if err != nil {
		t.Fatalf("Validate() = %v, want nil for this package's own defaults", err)
	}
	if checked.MaxDatagram != MaxFrameSize {
		t.Errorf("MaxDatagram = %d, want the frame size of %d", checked.MaxDatagram, MaxFrameSize)
	}
	if checked.MaxPayload != MaxPayloadSize {
		t.Errorf("MaxPayload = %d, want %d", checked.MaxPayload, MaxPayloadSize)
	}
	// The round trip ceiling has to be able to hold a derived timeout, or a
	// measured path would be clamped by the ceiling on every send.
	if checked.MaxRTT < checked.RetransmitTimeout {
		t.Errorf("MaxRTT = %s, want it above the retransmission timeout of %s",
			checked.MaxRTT, checked.RetransmitTimeout)
	}
	if checked.HandshakeTimeout < checked.RetransmitTimeout {
		t.Errorf("HandshakeTimeout = %s, want it above one retransmission of %s",
			checked.HandshakeTimeout, checked.RetransmitTimeout)
	}
	// An idle timeout shorter than a handshake would close a connection that was
	// still being established.
	if checked.IdleTimeout < checked.HandshakeTimeout {
		t.Errorf("IdleTimeout = %s, want it above the handshake of %s",
			checked.IdleTimeout, checked.HandshakeTimeout)
	}
}
