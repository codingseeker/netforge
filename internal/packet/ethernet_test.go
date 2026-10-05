package packet

import (
	"bytes"
	"net"
	"testing"

	"github.com/netforge/netforge/internal/apperr"
)

func TestParseEthernetFromVector(t *testing.T) {
	frame := decodeVector(t, udpFrame)

	link, err := ParseEthernet(DefaultLimits(), frame)
	if err != nil {
		t.Fatalf("ParseEthernet() = %v, want nil", err)
	}
	if got, want := link.Destination.String(), "02:00:00:00:00:01"; got != want {
		t.Fatalf("Destination = %s, want %s", got, want)
	}
	if got, want := link.Source.String(), "02:00:00:00:00:02"; got != want {
		t.Fatalf("Source = %s, want %s", got, want)
	}
	if link.EtherType != EtherTypeIPv4 {
		t.Fatalf("EtherType = 0x%04x, want 0x0800", uint16(link.EtherType))
	}
	if got, want := len(link.Payload), 36; got != want {
		t.Fatalf("Payload is %d bytes, want %d", got, want)
	}
	if link.IsBroadcast() || link.IsMulticast() {
		t.Fatal("a unicast frame was reported as broadcast or multicast")
	}
}

func TestEthernetRoundTrip(t *testing.T) {
	original := decodeVector(t, udpFrame)

	link, err := ParseEthernet(DefaultLimits(), original)
	if err != nil {
		t.Fatalf("ParseEthernet() = %v, want nil", err)
	}
	serialized, err := link.Serialize(DefaultLimits())
	if err != nil {
		t.Fatalf("Serialize() = %v, want nil", err)
	}
	if !bytes.Equal(serialized, original) {
		t.Fatalf("Serialize(ParseEthernet(x)) = %x, want %x", serialized, original)
	}
}

func TestParseEthernetRejectsTruncatedInput(t *testing.T) {
	full := decodeVector(t, udpFrame)
	for length := 0; length < EthernetHeaderSize; length++ {
		_, err := ParseEthernet(DefaultLimits(), full[:length])
		if !apperr.IsKind(err, apperr.KindInvalidArgument) {
			t.Fatalf("ParseEthernet(%d bytes) = %v, want invalid_argument", length, err)
		}
	}
}

func TestParseEthernetRejectsOversizeFrame(t *testing.T) {
	oversized := make([]byte, MaxFrameSize+1)

	_, err := ParseEthernet(DefaultLimits(), oversized)
	if !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Fatalf("ParseEthernet(oversized) = %v, want limit_exceeded", err)
	}
}

func TestParseEthernetHonoursCallerLimits(t *testing.T) {
	frame := decodeVector(t, udpFrame)
	limits := DefaultLimits()
	limits.MaxFrameSize = 40

	_, err := ParseEthernet(limits, frame)
	if !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Fatalf("ParseEthernet(tight limit) = %v, want limit_exceeded", err)
	}
}

func TestParseEthernetRejectsMulticastSource(t *testing.T) {
	frame := decodeVector(t, udpFrame)
	frame[6] |= 0x01

	_, err := ParseEthernet(DefaultLimits(), frame)
	if !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("ParseEthernet(multicast source) = %v, want invalid_argument", err)
	}
}

func TestEthernetBroadcastAndMulticast(t *testing.T) {
	broadcast := decodeVector(t, broadcastARPFrame)
	link, err := ParseEthernet(DefaultLimits(), broadcast)
	if err != nil {
		t.Fatalf("ParseEthernet(broadcast) = %v, want nil", err)
	}
	if !link.IsBroadcast() {
		t.Fatal("IsBroadcast() = false for a broadcast frame")
	}
	if !link.IsMulticast() {
		t.Fatal("IsMulticast() = false for a broadcast frame")
	}
}

func TestEthernetSerializeRejectsOversizePayload(t *testing.T) {
	link := &Ethernet{
		Destination: net.HardwareAddr{2, 0, 0, 0, 0, 1},
		Source:      net.HardwareAddr{2, 0, 0, 0, 0, 2},
		EtherType:   EtherTypeIPv4,
		Payload:     make([]byte, MaxFrameSize),
	}

	_, err := link.Serialize(DefaultLimits())
	if !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Fatalf("Serialize(oversized payload) = %v, want limit_exceeded", err)
	}
}

func TestEthernetSerializeRejectsInvalidAddress(t *testing.T) {
	link := &Ethernet{
		Destination: net.HardwareAddr{2, 0, 0},
		Source:      net.HardwareAddr{2, 0, 0, 0, 0, 2},
		EtherType:   EtherTypeIPv4,
	}

	if _, err := link.Serialize(DefaultLimits()); !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("Serialize(short address) = %v, want invalid_argument", err)
	}
}

func TestLimitsValidation(t *testing.T) {
	tests := []struct {
		name   string
		limits Limits
		valid  bool
	}{
		{name: "defaults", limits: DefaultLimits(), valid: true},
		{name: "zero frame size", limits: Limits{MaxFrameSize: 0, MaxPacketSize: 1500}},
		{name: "negative packet size", limits: Limits{MaxFrameSize: 1500, MaxPacketSize: -1}},
		{name: "packet size above protocol", limits: Limits{MaxFrameSize: MaxFrameSize, MaxPacketSize: MaxIPv4TotalLength + 1}},
		{name: "frame size above protocol", limits: Limits{MaxFrameSize: MaxFrameSize + 1, MaxPacketSize: MaxIPv4TotalLength}},
		{name: "frame too small for a packet", limits: Limits{MaxFrameSize: EthernetHeaderSize + 19, MaxPacketSize: 1500}},
		{name: "tight but usable", limits: Limits{MaxFrameSize: 200, MaxPacketSize: 128}, valid: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.limits.Validate()
			if tc.valid && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if !tc.valid {
				if err == nil {
					t.Fatal("Validate() = nil, want an error")
				}
				if !apperr.IsKind(err, apperr.KindInvalidArgument) {
					t.Fatalf("Validate() = %v, want invalid_argument", err)
				}
			}
		})
	}
}

func TestParsersRejectInvalidLimits(t *testing.T) {
	bad := Limits{}
	frame := decodeVector(t, udpFrame)

	if _, err := ParseEthernet(bad, frame); err == nil {
		t.Fatal("ParseEthernet(invalid limits) = nil, want an error")
	}
	if _, err := ParseIPv4(bad, frame[EthernetHeaderSize:]); err == nil {
		t.Fatal("ParseIPv4(invalid limits) = nil, want an error")
	}
	if _, err := ParseFrame(bad, frame); err == nil {
		t.Fatal("ParseFrame(invalid limits) = nil, want an error")
	}
}
