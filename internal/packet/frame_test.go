package packet

import (
	"bytes"
	"net/netip"
	"testing"

	"github.com/netforge/netforge/internal/apperr"
)

func TestParseFrameRejectsUnsupportedEtherTypes(t *testing.T) {
	tests := []struct {
		name    string
		encoded string
	}{
		{name: "ARP", encoded: broadcastARPFrame},
		{name: "VLAN tagged", encoded: vlanFrame},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {

			link, err := ParseEthernet(DefaultLimits(), decodeVector(t, tc.encoded))
			if err != nil {
				t.Fatalf("ParseEthernet() = %v, want nil", err)
			}
			if link.EtherType == EtherTypeIPv4 {
				t.Fatalf("EtherType = 0x%04x, want a non-IPv4 type", uint16(link.EtherType))
			}

			_, err = ParseFrame(DefaultLimits(), decodeVector(t, tc.encoded))
			if !apperr.IsKind(err, apperr.KindUnsupported) {
				t.Fatalf("ParseFrame() = %v, want unsupported", err)
			}
		})
	}
}

func TestFrameRoundTripForEveryProtocol(t *testing.T) {
	for name, encoded := range map[string]string{
		"UDP":             udpFrame,
		"UDP no checksum": udpZeroChecksumFrame,
		"ICMP echo":       icmpEchoFrame,
		"ICMP error":      icmpUnreachableFrame,
		"TCP":             tcpSynFrame,
		"IPv4 options":    ipOptionsFrame,
	} {
		t.Run(name, func(t *testing.T) {
			original := decodeVector(t, encoded)

			frame, err := ParseFrame(DefaultLimits(), original)
			if err != nil {
				t.Fatalf("ParseFrame() = %v, want nil", err)
			}
			serialized, err := frame.Serialize(DefaultLimits())
			if err != nil {
				t.Fatalf("Serialize() = %v, want nil", err)
			}
			if !bytes.Equal(serialized, original) {
				t.Fatalf("Serialize(ParseFrame(x)) = %x, want %x", serialized, original)
			}
		})
	}
}

func TestFrameSizeMatchesSerialization(t *testing.T) {
	for name, encoded := range map[string]string{
		"UDP":       udpFrame,
		"ICMP":      icmpEchoFrame,
		"TCP":       tcpSynFrame,
		"IPv4 opts": ipOptionsFrame,
	} {
		t.Run(name, func(t *testing.T) {
			frame, err := ParseFrame(DefaultLimits(), decodeVector(t, encoded))
			if err != nil {
				t.Fatalf("ParseFrame() = %v, want nil", err)
			}
			serialized, err := frame.Serialize(DefaultLimits())
			if err != nil {
				t.Fatalf("Serialize() = %v, want nil", err)
			}
			if got, want := frame.Size(), len(serialized)-len(frame.Padding); got != want {
				t.Fatalf("Size() = %d, want %d", got, want)
			}
		})
	}
}

func TestFrameKeepsUnhandledProtocols(t *testing.T) {

	link, err := ParseEthernet(DefaultLimits(), decodeVector(t, udpFrame))
	if err != nil {
		t.Fatalf("ParseEthernet() = %v, want nil", err)
	}
	payload := make([]byte, len(link.Payload))
	copy(payload, link.Payload)
	payload[9] = 47
	putChecksum(payload[10:12], 0)
	payload[10], payload[11] = 0, 0
	packet := &IPv4{
		TypeOfService: 0,
		Protocol:      Protocol(47),
		TimeToLive:    64,
		Source:        netip.MustParseAddr("10.42.0.1"),
		Destination:   netip.MustParseAddr("10.42.0.2"),
		Payload:       payload[MinIPv4HeaderSize:],
	}
	serializedPacket, err := packet.Serialize(DefaultLimits())
	if err != nil {
		t.Fatalf("Serialize(IPv4) = %v, want nil", err)
	}
	link.Payload = serializedPacket
	original, err := link.Serialize(DefaultLimits())
	if err != nil {
		t.Fatalf("Serialize(Ethernet) = %v, want nil", err)
	}

	frame, err := ParseFrame(DefaultLimits(), original)
	if err != nil {
		t.Fatalf("ParseFrame() = %v, want nil", err)
	}
	if frame.IP.Protocol != Protocol(47) {
		t.Fatalf("Protocol = %d, want 47", frame.IP.Protocol)
	}
	if frame.ICMP != nil || frame.UDP != nil || frame.TCP != nil {
		t.Fatal("a protocol this phase does not handle was decoded anyway")
	}
	roundTripped, err := frame.Serialize(DefaultLimits())
	if err != nil {
		t.Fatalf("Serialize(frame) = %v, want nil", err)
	}
	if !bytes.Equal(roundTripped, original) {
		t.Fatalf("Serialize(ParseFrame(x)) = %x, want %x", roundTripped, original)
	}
}

func TestParseFrameRejectsIPv4FrameShort(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxPacketSize = 24

	_, err := ParseFrame(limits, decodeVector(t, udpFrame))
	if !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Fatalf("ParseFrame(tight packet limit) = %v, want limit_exceeded", err)
	}
}

func TestFrameSerializeRejectsIncompleteFrames(t *testing.T) {
	for name, frame := range map[string]*Frame{
		"nil":       nil,
		"no link":   {IP: &IPv4{}},
		"no packet": {Link: &Ethernet{}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := frame.Serialize(DefaultLimits()); !apperr.IsKind(err, apperr.KindInvalidArgument) {
				t.Fatalf("Serialize(%s) = %v, want invalid_argument", name, err)
			}
		})
	}
}

func TestFramePaddingIsPreserved(t *testing.T) {
	link, err := ParseEthernet(DefaultLimits(), decodeVector(t, udpFrame))
	if err != nil {
		t.Fatalf("ParseEthernet() = %v, want nil", err)
	}

	padded := make([]byte, 0, 60)
	padded = append(padded, link.Payload...)
	for len(padded) < 60-EthernetHeaderSize {
		padded = append(padded, 0)
	}
	link.Payload = padded
	original, err := link.Serialize(DefaultLimits())
	if err != nil {
		t.Fatalf("Serialize() = %v, want nil", err)
	}

	frame, err := ParseFrame(DefaultLimits(), original)
	if err != nil {
		t.Fatalf("ParseFrame(padded) = %v, want nil", err)
	}
	if got, want := len(frame.Padding), len(padded)-36; got != want {
		t.Fatalf("Padding is %d bytes, want %d", got, want)
	}
	if got, want := string(frame.UDP.Payload), "netforge"; got != want {
		t.Fatalf("Payload = %q, want %q", got, want)
	}
	roundTripped, err := frame.Serialize(DefaultLimits())
	if err != nil {
		t.Fatalf("Serialize(padded) = %v, want nil", err)
	}
	if !bytes.Equal(roundTripped, original) {
		t.Fatalf("Serialize(ParseFrame(padded)) = %x, want %x", roundTripped, original)
	}
}

func TestFrameRejectsOversizePadding(t *testing.T) {
	link, err := ParseEthernet(DefaultLimits(), decodeVector(t, udpFrame))
	if err != nil {
		t.Fatalf("ParseEthernet() = %v, want nil", err)
	}
	padded := make([]byte, 0, len(link.Payload)+MaxPaddingSize+1)
	padded = append(padded, link.Payload...)
	padded = append(padded, make([]byte, MaxPaddingSize+1)...)

	if _, err := ParseFrame(DefaultLimits(), padded); !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Fatalf("ParseFrame(oversized padding) = %v, want limit_exceeded", err)
	}
}

func TestParseFrameReportsTheFirstFailure(t *testing.T) {

	frame := decodeVector(t, frameCorruptIPChecksum)
	_, err := ParseFrame(DefaultLimits(), frame)
	if !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("ParseFrame() = %v, want invalid_argument", err)
	}
	if got := err.Error(); !bytes.Contains([]byte(got), []byte("ParseIPv4")) {
		t.Fatalf("error %q does not name the failing layer", got)
	}
}

func TestFrameKeepsTheBytesUDPLengthDoesNotCover(t *testing.T) {

	link, err := ParseEthernet(DefaultLimits(), decodeVector(t, udpFrame))
	if err != nil {
		t.Fatalf("ParseEthernet() = %v, want nil", err)
	}
	packet, err := ParseIPv4(DefaultLimits(), link.Payload)
	if err != nil {
		t.Fatalf("ParseIPv4() = %v, want nil", err)
	}
	packet.Payload = append(append([]byte(nil), packet.Payload...), 0xde, 0xad)
	padded, err := packet.Serialize(DefaultLimits())
	if err != nil {
		t.Fatalf("Serialize(IPv4) = %v, want nil", err)
	}
	link.Payload = padded
	original, err := link.Serialize(DefaultLimits())
	if err != nil {
		t.Fatalf("Serialize() = %v, want nil", err)
	}

	frame, err := ParseFrame(DefaultLimits(), original)
	if err != nil {
		t.Fatalf("ParseFrame() = %v, want nil", err)
	}
	if got, want := len(frame.IP.Payload), 18; got != want {
		t.Fatalf("the IP payload is %d bytes, want %d", got, want)
	}
	if got, want := string(frame.UDP.Payload), "netforge"; got != want {
		t.Fatalf("Payload = %q, want %q", got, want)
	}
	if got, want := len(frame.PayloadTail), 2; got != want {
		t.Fatalf("PayloadTail is %d bytes, want %d", got, want)
	}
	if got, want := len(frame.Padding), 0; got != want {
		t.Fatalf("Padding is %d bytes, want %d", got, want)
	}

	roundTripped, err := frame.Serialize(DefaultLimits())
	if err != nil {
		t.Fatalf("Serialize() = %v, want nil", err)
	}
	if !bytes.Equal(roundTripped, original) {
		t.Fatalf("Serialize(ParseFrame(x)) = %x, want %x", roundTripped, original)
	}

	frame.PayloadTail = nil
	shortened, err := frame.Serialize(DefaultLimits())
	if err != nil {
		t.Fatalf("Serialize(no tail) = %v, want nil", err)
	}
	shortFrame, err := ParseFrame(DefaultLimits(), shortened)
	if err != nil {
		t.Fatalf("ParseFrame(no tail) = %v, want nil", err)
	}
	if len(shortFrame.PayloadTail) != 0 {
		t.Fatal("the tail came back")
	}
	if got, want := string(shortFrame.UDP.Payload), "netforge"; got != want {
		t.Fatalf("Payload = %q, want %q", got, want)
	}
}
