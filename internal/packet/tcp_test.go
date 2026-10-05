package packet

import (
	"bytes"
	"net/netip"
	"testing"

	"github.com/netforge/netforge/internal/apperr"
)

func TestParseTCPFromVector(t *testing.T) {
	frame, err := ParseFrame(DefaultLimits(), decodeVector(t, tcpSynFrame))
	if err != nil {
		t.Fatalf("ParseFrame() = %v, want nil", err)
	}
	if frame.TCP == nil {
		t.Fatal("ParseFrame() produced no TCP segment")
	}
	segment := frame.TCP

	if got, want := segment.SourcePort, uint16(40000); got != want {
		t.Fatalf("SourcePort = %d, want %d", got, want)
	}
	if got, want := segment.DestinationPort, uint16(80); got != want {
		t.Fatalf("DestinationPort = %d, want %d", got, want)
	}
	if got, want := segment.SequenceNumber, uint32(0x11223344); got != want {
		t.Fatalf("SequenceNumber = 0x%08x, want 0x%08x", got, want)
	}
	if got, want := segment.Acknowledgment, uint32(0); got != want {
		t.Fatalf("Acknowledgment = 0x%08x, want 0x%08x", got, want)
	}
	if !segment.Has(TCPFlagSYN) {
		t.Fatal("Has(SYN) = false for a SYN segment")
	}
	if segment.Has(TCPFlagACK) || segment.Has(TCPFlagFIN) || segment.Has(TCPFlagRST) {
		t.Fatal("a SYN segment must not carry ACK, FIN or RST")
	}
	if got, want := segment.Window, uint16(65535); got != want {
		t.Fatalf("Window = %d, want %d", got, want)
	}
	if got, want := segment.Checksum, uint16(0x8dd2); got != want {
		t.Fatalf("Checksum = 0x%04x, want 0x%04x", got, want)
	}
	if got, want := segment.UrgentPointer, uint16(0); got != want {
		t.Fatalf("UrgentPointer = %d, want %d", got, want)
	}
	if got, want := segment.HeaderLength(), 28; got != want {
		t.Fatalf("HeaderLength() = %d, want %d", got, want)
	}

	want := []byte{0x02, 0x04, 0x05, 0xb4, 0x01, 0x01, 0x04, 0x02}
	if !bytes.Equal(segment.Options, want) {
		t.Fatalf("Options = %x, want %x", segment.Options, want)
	}
	if len(segment.Payload) != 0 {
		t.Fatalf("Payload is %d bytes, want none", len(segment.Payload))
	}
	if segment.ChecksumUnchecked {
		t.Fatal("ChecksumUnchecked = true although verification was requested")
	}
}

func TestTCPRoundTrip(t *testing.T) {
	source := netip.MustParseAddr("10.42.0.1")
	destination := netip.MustParseAddr("10.42.0.2")
	segment := &TCP{
		SourcePort:      40000,
		DestinationPort: 80,
		SequenceNumber:  0x11223344,
		Flags:           byte(TCPFlagSYN),
		Window:          65535,
		Options:         []byte{0x02, 0x04, 0x05, 0xb4, 0x01, 0x01, 0x04, 0x02},
	}

	serialized, err := segment.Serialize(source, destination)
	if err != nil {
		t.Fatalf("Serialize() = %v, want nil", err)
	}
	if want := decodeVector(t, tcpSynFrame)[EthernetHeaderSize+MinIPv4HeaderSize:]; !bytes.Equal(serialized, want) {
		t.Fatalf("Serialize() = %x, want %x", serialized, want)
	}

	parsed, err := ParseTCP(DefaultLimits(), serialized)
	if err != nil {
		t.Fatalf("ParseTCP() = %v, want nil", err)
	}
	if err := parsed.Validate(source, destination); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	reserialized, err := parsed.Serialize(source, destination)
	if err != nil {
		t.Fatalf("Serialize(parsed) = %v, want nil", err)
	}
	if !bytes.Equal(reserialized, serialized) {
		t.Fatalf("Serialize(ParseTCP(Serialize(x))) = %x, want %x", reserialized, serialized)
	}
}

func TestParseTCPRejectsTruncatedInput(t *testing.T) {
	valid := decodeVector(t, tcpSynFrame)[EthernetHeaderSize+MinIPv4HeaderSize:]

	for length := 0; length < len(valid); length++ {
		_, err := ParseTCP(DefaultLimits(), valid[:length])
		if err == nil {
			t.Fatalf("ParseTCP(%d of %d bytes) = nil, want an error", length, len(valid))
		}
	}
}

func TestParseTCPRejectsBadDataOffset(t *testing.T) {
	valid := decodeVector(t, tcpSynFrame)[EthernetHeaderSize+MinIPv4HeaderSize:]

	tests := []struct {
		name  string
		octet byte
	}{
		{name: "zero", octet: 0x00},
		{name: "below the minimum", octet: 0x41},
		{name: "maximum", octet: 0xf0},
		{name: "beyond the buffer", octet: 0x80},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			segment := make([]byte, len(valid))
			copy(segment, valid)
			segment[12] = tc.octet

			if _, err := ParseTCP(DefaultLimits(), segment); !apperr.IsKind(err, apperr.KindInvalidArgument) {
				t.Fatalf("ParseTCP() = %v, want invalid_argument", err)
			}
		})
	}
}

func TestParseTCPKeepsPayloadSeparate(t *testing.T) {
	segment := &TCP{
		SourcePort:      1,
		DestinationPort: 2,
		Flags:           byte(TCPFlagACK) | byte(TCPFlagPSH),
		Options:         []byte{0x01, 0x01, 0x08, 0x0a},
		Payload:         []byte("payload"),
	}
	serialized, err := segment.Serialize(
		netip.MustParseAddr("10.42.0.1"), netip.MustParseAddr("10.42.0.2"))
	if err != nil {
		t.Fatalf("Serialize() = %v, want nil", err)
	}
	if got, want := len(serialized), 24+len("payload"); got != want {
		t.Fatalf("serialized %d bytes, want %d", got, want)
	}

	parsed, err := ParseTCP(DefaultLimits(), serialized)
	if err != nil {
		t.Fatalf("ParseTCP() = %v, want nil", err)
	}
	if got, want := len(parsed.Options), 4; got != want {
		t.Fatalf("Options are %d bytes, want %d", got, want)
	}
	if got, want := string(parsed.Payload), "payload"; got != want {
		t.Fatalf("Payload = %q, want %q", got, want)
	}
	if err := parsed.Validate(netip.MustParseAddr("10.42.0.1"), netip.MustParseAddr("10.42.0.2")); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestTCPMaximumHeaderRoundTrip(t *testing.T) {
	options := make([]byte, MaxTCPHeaderSize-MinTCPHeaderSize)
	for i := range options {
		options[i] = byte(i)
	}
	source := netip.MustParseAddr("10.42.0.1")
	destination := netip.MustParseAddr("10.42.0.2")
	segment := &TCP{SourcePort: 1, DestinationPort: 2, Options: options, Window: 1}

	serialized, err := segment.Serialize(source, destination)
	if err != nil {
		t.Fatalf("Serialize(maximum header) = %v, want nil", err)
	}
	if got, want := int(serialized[12]>>4)*4, MaxTCPHeaderSize; got != want {
		t.Fatalf("data offset = %d, want %d", got, want)
	}
	parsed, err := ParseTCP(DefaultLimits(), serialized)
	if err != nil {
		t.Fatalf("ParseTCP(maximum header) = %v, want nil", err)
	}
	if !bytes.Equal(parsed.Options, options) {
		t.Fatal("the maximum size option block did not survive the round trip")
	}
	if err := parsed.Validate(source, destination); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestTCPSerializeRejectsInvalidOptions(t *testing.T) {
	source := netip.MustParseAddr("10.42.0.1")
	destination := netip.MustParseAddr("10.42.0.2")

	unaligned := &TCP{Options: []byte{1, 2, 3}}
	if _, err := unaligned.Serialize(source, destination); !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("Serialize(unaligned options) = %v, want invalid_argument", err)
	}

	tooLong := &TCP{Options: make([]byte, MaxTCPHeaderSize-MinTCPHeaderSize+4)}
	if _, err := tooLong.Serialize(source, destination); !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("Serialize(oversized options) = %v, want invalid_argument", err)
	}
}

func TestTCPSerializeRejectsOversizeSegment(t *testing.T) {
	segment := &TCP{Payload: make([]byte, MaxIPv4TotalLength)}

	_, err := segment.Serialize(netip.MustParseAddr("10.42.0.1"), netip.MustParseAddr("10.42.0.2"))
	if !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Fatalf("Serialize(oversized) = %v, want limit_exceeded", err)
	}
}

func TestParseFrameRejectsBadTCPChecksum(t *testing.T) {
	corrupt := decodeVector(t, tcpSynFrame)
	corrupt[len(corrupt)-1] ^= 0x01

	if _, err := ParseFrame(DefaultLimits(), corrupt); !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("ParseFrame(bad TCP checksum) = %v, want invalid_argument", err)
	}

	limits := DefaultLimits()
	limits.VerifyChecksums = false
	frame, err := ParseFrame(limits, corrupt)
	if err != nil {
		t.Fatalf("ParseFrame(unchecked) = %v, want nil", err)
	}
	if frame.TCP == nil {
		t.Fatal("ParseFrame(unchecked) produced no TCP segment")
	}

	if err := frame.TCP.Validate(frame.IP.Source, frame.IP.Destination); !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("Validate(corrupt) = %v, want invalid_argument", err)
	}
}

func TestTCPValidateRejectsWrongEndpoints(t *testing.T) {
	source := netip.MustParseAddr("10.42.0.1")
	destination := netip.MustParseAddr("10.42.0.2")
	segment := &TCP{SourcePort: 1, DestinationPort: 2, Flags: byte(TCPFlagSYN)}
	serialized, err := segment.Serialize(source, destination)
	if err != nil {
		t.Fatalf("Serialize() = %v, want nil", err)
	}

	parsed, err := ParseTCP(DefaultLimits(), serialized)
	if err != nil {
		t.Fatalf("ParseTCP() = %v, want nil", err)
	}
	if err := parsed.Validate(netip.MustParseAddr("10.42.0.9"), destination); !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("Validate(wrong source) = %v, want invalid_argument", err)
	}
	if err := parsed.Validate(source, netip.MustParseAddr("10.42.0.9")); !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("Validate(wrong destination) = %v, want invalid_argument", err)
	}
	if err := parsed.Validate(source, destination); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestTCPFatFlags(t *testing.T) {
	segment := &TCP{SourcePort: 1, DestinationPort: 2, Window: 1}
	segment.Set(TCPFlagSYN, true)
	segment.Set(TCPFlagACK, true)
	segment.Set(TCPFlagPSH, true)
	segment.Set(TCPFlagFIN, true)

	all := []TCPFlag{TCPFlagSYN, TCPFlagACK, TCPFlagPSH, TCPFlagFIN}
	for _, flag := range all {
		if !segment.Has(flag) {
			t.Fatalf("Has(%#02x) = false after Set", byte(flag))
		}
	}
	segment.Set(TCPFlagFIN, false)
	if segment.Has(TCPFlagFIN) {
		t.Fatal("Has(FIN) = true after clearing it")
	}
	if !segment.Has(TCPFlagSYN) || !segment.Has(TCPFlagACK) || !segment.Has(TCPFlagPSH) {
		t.Fatal("clearing one flag disturbed another")
	}
}

func TestTCPPreservesReservedFlagBits(t *testing.T) {
	source := netip.MustParseAddr("10.42.0.1")
	destination := netip.MustParseAddr("10.42.0.2")
	original := &TCP{SourcePort: 1, DestinationPort: 2, Window: 1, Flags: 0xc1}

	serialized, err := original.Serialize(source, destination)
	if err != nil {
		t.Fatalf("Serialize() = %v, want nil", err)
	}
	if got, want := serialized[13], byte(0xc1); got != want {
		t.Fatalf("flag octet = 0x%02x, want 0x%02x", got, want)
	}
	parsed, err := ParseTCP(DefaultLimits(), serialized)
	if err != nil {
		t.Fatalf("ParseTCP() = %v, want nil", err)
	}
	if parsed.Flags != original.Flags {
		t.Fatalf("Flags = 0x%02x, want 0x%02x", parsed.Flags, original.Flags)
	}
}
