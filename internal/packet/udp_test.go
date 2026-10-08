package packet

import (
	"bytes"
	"net"
	"net/netip"
	"testing"

	"github.com/codingseeker/netforge/internal/apperr"
)

func TestParseUDPFromVector(t *testing.T) {
	frame, err := ParseFrame(DefaultLimits(), decodeVector(t, udpFrame))
	if err != nil {
		t.Fatalf("ParseFrame() = %v, want nil", err)
	}
	if frame.UDP == nil {
		t.Fatal("ParseFrame() produced no UDP datagram")
	}
	if got, want := frame.UDP.SourcePort, uint16(53); got != want {
		t.Fatalf("SourcePort = %d, want %d", got, want)
	}
	if got, want := frame.UDP.DestinationPort, uint16(5353); got != want {
		t.Fatalf("DestinationPort = %d, want %d", got, want)
	}
	if got, want := frame.UDP.Checksum, uint16(0x1cb6); got != want {
		t.Fatalf("Checksum = 0x%04x, want 0x%04x", got, want)
	}
	if frame.UDP.ChecksumOmitted {
		t.Fatal("ChecksumOmitted = true, the vector carries a checksum")
	}
	if got, want := string(frame.UDP.Payload), "netforge"; got != want {
		t.Fatalf("Payload = %q, want %q", got, want)
	}
	if got, want := len(frame.UDP.Payload), 8; got != want {
		t.Fatalf("Payload is %d bytes, want %d", got, want)
	}
}

func TestUDPRoundTrip(t *testing.T) {
	datagram := &UDP{
		SourcePort:      53,
		DestinationPort: 5353,
		Payload:         []byte("netforge"),
	}
	serialized, err := datagram.Serialize(
		netip.MustParseAddr("10.42.0.1"), netip.MustParseAddr("10.42.0.2"))
	if err != nil {
		t.Fatalf("Serialize() = %v, want nil", err)
	}
	if want := decodeVector(t, udpFrame)[EthernetHeaderSize+MinIPv4HeaderSize:]; !bytes.Equal(serialized, want) {
		t.Fatalf("Serialize() = %x, want %x", serialized, want)
	}

	parsed, err := ParseUDP(DefaultLimits(), serialized)
	if err != nil {
		t.Fatalf("ParseUDP() = %v, want nil", err)
	}
	if err := parsed.Validate(netip.MustParseAddr("10.42.0.1"), netip.MustParseAddr("10.42.0.2")); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestUDPOmittedChecksumFromVector(t *testing.T) {
	frame, err := ParseFrame(DefaultLimits(), decodeVector(t, udpZeroChecksumFrame))
	if err != nil {
		t.Fatalf("ParseFrame() = %v, want nil", err)
	}
	if frame.UDP == nil {
		t.Fatal("ParseFrame() produced no UDP datagram")
	}
	if !frame.UDP.ChecksumOmitted {
		t.Fatal("ChecksumOmitted = false for a datagram with a zero checksum field")
	}
	if frame.UDP.Checksum != 0 {
		t.Fatalf("Checksum = 0x%04x, want 0", frame.UDP.Checksum)
	}
	if got, want := string(frame.UDP.Payload), "abc"; got != want {
		t.Fatalf("Payload = %q, want %q", got, want)
	}
	if err := frame.UDP.Validate(frame.IP.Source, frame.IP.Destination); err != nil {
		t.Fatalf("Validate(omitted) = %v, want nil", err)
	}
}

func TestUDPSerializeNoSum(t *testing.T) {
	datagram := &UDP{SourcePort: 1234, DestinationPort: 5678, Payload: []byte("abc")}

	serialized, err := datagram.SerializeNoSum()
	if err != nil {
		t.Fatalf("SerializeNoSum() = %v, want nil", err)
	}
	if want := decodeVector(t, udpZeroChecksumFrame)[EthernetHeaderSize+MinIPv4HeaderSize:]; !bytes.Equal(serialized, want) {
		t.Fatalf("SerializeNoSum() = %x, want %x", serialized, want)
	}
	if !datagram.ChecksumOmitted {
		t.Fatal("ChecksumOmitted = false after SerializeNoSum()")
	}

	if _, err := datagram.Serialize(netip.MustParseAddr("10.42.0.1"), netip.MustParseAddr("10.42.0.2")); err != nil {
		t.Fatalf("Serialize() = %v, want nil", err)
	}
	if datagram.ChecksumOmitted {
		t.Fatal("ChecksumOmitted = true after a Serialize that computed a checksum")
	}
	if datagram.Checksum == 0 {
		t.Fatal("Checksum = 0 after Serialize, a computed zero is transmitted as all ones")
	}
}

func TestParseUDPRejectsTruncatedInput(t *testing.T) {
	valid := decodeVector(t, udpFrame)[EthernetHeaderSize+MinIPv4HeaderSize:]

	for length := 0; length < len(valid); length++ {
		if _, err := ParseUDP(DefaultLimits(), valid[:length]); err == nil {
			t.Fatalf("ParseUDP(%d of %d bytes) = nil, want an error", length, len(valid))
		}
	}
}

func TestParseUDPRejectsBadLengthField(t *testing.T) {
	valid := decodeVector(t, udpFrame)[EthernetHeaderSize+MinIPv4HeaderSize:]

	tests := []struct {
		name  string
		field uint16
	}{
		{name: "zero", field: 0},
		{name: "below the header size", field: UDPHeaderSize - 1},
		{name: "beyond the buffer", field: uint16(len(valid) + 1)},
		{name: "protocol maximum", field: 0xffff},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			datagram := make([]byte, len(valid))
			copy(datagram, valid)
			putChecksum(datagram[4:6], tc.field)

			if _, err := ParseUDP(DefaultLimits(), datagram); !apperr.IsKind(err, apperr.KindInvalidArgument) {
				t.Fatalf("ParseUDP() = %v, want invalid_argument", err)
			}
		})
	}
}

func TestParseUDPKeepsTrailingBytesPadding(t *testing.T) {
	valid := decodeVector(t, udpFrame)[EthernetHeaderSize+MinIPv4HeaderSize:]
	withPadding := append(append([]byte(nil), valid...), make([]byte, 6)...)

	datagram, err := ParseUDP(DefaultLimits(), withPadding)
	if err != nil {
		t.Fatalf("ParseUDP(padded) = %v, want nil", err)
	}
	if got, want := string(datagram.Payload), "netforge"; got != want {
		t.Fatalf("Payload = %q, want %q", got, want)
	}
	if datagram.Payload[len(datagram.Payload)-1] != 'e' {
		t.Fatal("the padding leaked into the payload")
	}
}

func TestParseUDPDoesNotVerifyTheChecksum(t *testing.T) {
	corrupt := decodeVector(t, udpFrameBadChecksum)[EthernetHeaderSize+MinIPv4HeaderSize:]

	datagram, err := ParseUDP(DefaultLimits(), corrupt)
	if err != nil {
		t.Fatalf("ParseUDP(corrupt checksum) = %v, want nil", err)
	}
	if err := datagram.Validate(netip.MustParseAddr("10.42.0.1"), netip.MustParseAddr("10.42.0.2")); !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("Validate(corrupt) = %v, want invalid_argument", err)
	}
}

func TestParseFrameRejectsBadUDPChecksum(t *testing.T) {
	_, err := ParseFrame(DefaultLimits(), decodeVector(t, udpFrameBadChecksum))
	if !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("ParseFrame(bad UDP checksum) = %v, want invalid_argument", err)
	}

	limits := DefaultLimits()
	limits.VerifyChecksums = false
	frame, err := ParseFrame(limits, decodeVector(t, udpFrameBadChecksum))
	if err != nil {
		t.Fatalf("ParseFrame(unchecked) = %v, want nil", err)
	}
	if frame.UDP == nil {
		t.Fatal("ParseFrame(unchecked) produced no UDP datagram")
	}
	if got, want := frame.UDP.Checksum, uint16(0x1cb6); got != want {
		t.Fatalf("Checksum = 0x%04x, want the received 0x%04x", got, want)
	}
	if got, want := string(frame.UDP.Payload), "netforg\x9a"; got != want {
		t.Fatalf("Payload = %q, want the corrupted %q", got, want)
	}
}

func TestParseFrameRejectsCorruptIPChecksum(t *testing.T) {
	_, err := ParseFrame(DefaultLimits(), decodeVector(t, frameCorruptIPChecksum))
	if !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("ParseFrame(bad IP checksum) = %v, want invalid_argument", err)
	}
}

func TestParseFrameRejectsTruncatedVector(t *testing.T) {
	_, err := ParseFrame(DefaultLimits(), decodeVector(t, udpFrameTruncated))
	if !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("ParseFrame(truncated) = %v, want invalid_argument", err)
	}
}

func TestUDPSerializeRejectsOversizePayload(t *testing.T) {
	datagram := &UDP{SourcePort: 1, DestinationPort: 2, Payload: make([]byte, MaxUDPPayloadSize+1)}

	_, err := datagram.Serialize(netip.MustParseAddr("10.42.0.1"), netip.MustParseAddr("10.42.0.2"))
	if !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Fatalf("Serialize(oversized) = %v, want limit_exceeded", err)
	}
}

func TestUDPMaximumSizeRoundTrip(t *testing.T) {
	payload := make([]byte, MaxUDPPayloadSize)
	for i := range payload {
		payload[i] = byte(i)
	}
	source := netip.MustParseAddr("10.42.0.1")
	destination := netip.MustParseAddr("10.42.0.2")
	datagram := &UDP{SourcePort: 53, DestinationPort: 5353, Payload: payload}

	serialized, err := datagram.Serialize(source, destination)
	if err != nil {
		t.Fatalf("Serialize(maximum) = %v, want nil", err)
	}
	if got, want := len(serialized), MaxUDPPayloadSize+UDPHeaderSize; got != want {
		t.Fatalf("serialized %d bytes, want %d", got, want)
	}
	if got, want := int(binaryUint16(serialized[4:6])), len(serialized); got != want {
		t.Fatalf("length field = %d, want %d", got, want)
	}

	parsed, err := ParseUDP(DefaultLimits(), serialized)
	if err != nil {
		t.Fatalf("ParseUDP(maximum) = %v, want nil", err)
	}
	if err := parsed.Validate(source, destination); err != nil {
		t.Fatalf("Validate(maximum) = %v, want nil", err)
	}
	if !bytes.Equal(parsed.Payload, payload) {
		t.Fatal("the maximum size payload did not survive the round trip")
	}
}

func TestUDPEmptyPayload(t *testing.T) {
	source := netip.MustParseAddr("10.42.0.1")
	destination := netip.MustParseAddr("10.42.0.2")
	datagram := &UDP{SourcePort: 53, DestinationPort: 5353}

	serialized, err := datagram.Serialize(source, destination)
	if err != nil {
		t.Fatalf("Serialize() = %v, want nil", err)
	}
	if got, want := len(serialized), UDPHeaderSize; got != want {
		t.Fatalf("serialized %d bytes, want %d", got, want)
	}

	parsed, err := ParseUDP(DefaultLimits(), serialized)
	if err != nil {
		t.Fatalf("ParseUDP() = %v, want nil", err)
	}
	if len(parsed.Payload) != 0 {
		t.Fatalf("Payload is %d bytes, want none", len(parsed.Payload))
	}
	if err := parsed.Validate(source, destination); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestUDPValidateRejectsWrongEndpoints(t *testing.T) {
	source := netip.MustParseAddr("10.42.0.1")
	destination := netip.MustParseAddr("10.42.0.2")
	datagram := &UDP{SourcePort: 53, DestinationPort: 5353, Payload: []byte("netforge")}
	if _, err := datagram.Serialize(source, destination); err != nil {
		t.Fatalf("Serialize() = %v, want nil", err)
	}

	parsed, err := ParseUDP(DefaultLimits(), mustSerialize(t, datagram, source, destination))
	if err != nil {
		t.Fatalf("ParseUDP() = %v, want nil", err)
	}
	wrong := []netip.Addr{
		netip.MustParseAddr("10.42.0.3"),
		netip.MustParseAddr("10.42.0.4"),
	}
	for _, candidate := range wrong {
		if err := parsed.Validate(candidate, destination); err == nil {
			t.Fatalf("Validate(%s) = nil, want an error", candidate)
		}
	}
}

func TestDatagramMatchesVector(t *testing.T) {
	built, err := Datagram(DefaultLimits(),
		net.HardwareAddr{2, 0, 0, 0, 0, 2},
		net.HardwareAddr{2, 0, 0, 0, 0, 1},
		netip.MustParseAddr("10.42.0.1"),
		netip.MustParseAddr("10.42.0.2"),
		53, 5353, 64, []byte("netforge"))
	if err != nil {
		t.Fatalf("Datagram() = %v, want nil", err)
	}
	if want := decodeVector(t, udpFrameZeroID); !bytes.Equal(built, want) {
		t.Fatalf("Datagram = %x, want %x", built, want)
	}
}

func TestDatagramRejectsOversizePayload(t *testing.T) {
	_, err := Datagram(DefaultLimits(),
		net.HardwareAddr{2, 0, 0, 0, 0, 2},
		net.HardwareAddr{2, 0, 0, 0, 0, 1},
		netip.MustParseAddr("10.42.0.1"),
		netip.MustParseAddr("10.42.0.2"),
		53, 5353, 64, make([]byte, MaxUDPPayloadSize+1))
	if !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Fatalf("Datagram(oversized) = %v, want limit_exceeded", err)
	}
}

func mustSerialize(t *testing.T, datagram *UDP, source, destination netip.Addr) []byte {
	t.Helper()
	serialized, err := datagram.Serialize(source, destination)
	if err != nil {
		t.Fatalf("Serialize() = %v, want nil", err)
	}
	return serialized
}

func binaryUint16(data []byte) uint16 {
	return uint16(data[0])<<8 | uint16(data[1])
}
