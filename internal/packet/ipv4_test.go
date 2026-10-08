package packet

import (
	"bytes"
	"net/netip"
	"strconv"
	"testing"

	"github.com/codingseeker/netforge/internal/apperr"
)

func TestParseIPv4FromVector(t *testing.T) {
	link, err := ParseEthernet(DefaultLimits(), decodeVector(t, udpFrame))
	if err != nil {
		t.Fatalf("ParseEthernet() = %v, want nil", err)
	}

	packet, err := ParseIPv4(DefaultLimits(), link.Payload)
	if err != nil {
		t.Fatalf("ParseIPv4() = %v, want nil", err)
	}
	if got, want := packet.Source.String(), "10.42.0.1"; got != want {
		t.Fatalf("Source = %s, want %s", got, want)
	}
	if got, want := packet.Destination.String(), "10.42.0.2"; got != want {
		t.Fatalf("Destination = %s, want %s", got, want)
	}
	if packet.Protocol != ProtocolUDP {
		t.Fatalf("Protocol = %d, want %d", packet.Protocol, ProtocolUDP)
	}
	if packet.TimeToLive != 64 {
		t.Fatalf("TimeToLive = %d, want 64", packet.TimeToLive)
	}
	if packet.Identification != 0x1234 {
		t.Fatalf("Identification = 0x%04x, want 0x1234", packet.Identification)
	}
	if packet.DontFragment() {
		t.Fatal("DontFragment() = true, the vector leaves the flag clear")
	}
	if packet.MoreFragments() || packet.IsFragment() || packet.FragmentOffset() != 0 {
		t.Fatal("the vector is not fragmented")
	}
	if got, want := packet.HeaderLength(), MinIPv4HeaderSize; got != want {
		t.Fatalf("HeaderLength() = %d, want %d", got, want)
	}
	if got, want := packet.TotalLength(), 36; got != want {
		t.Fatalf("TotalLength() = %d, want %d", got, want)
	}
	if !packet.ChecksumValid() {
		t.Fatal("ChecksumValid() = false for a correct header")
	}
	if packet.ChecksumUnchecked {
		t.Fatal("ChecksumUnchecked = true although verification was requested")
	}
}

func TestIPv4RoundTrip(t *testing.T) {
	link, err := ParseEthernet(DefaultLimits(), decodeVector(t, udpFrame))
	if err != nil {
		t.Fatalf("ParseEthernet() = %v, want nil", err)
	}
	original := link.Payload

	packet, err := ParseIPv4(DefaultLimits(), original)
	if err != nil {
		t.Fatalf("ParseIPv4() = %v, want nil", err)
	}
	serialized, err := packet.Serialize(DefaultLimits())
	if err != nil {
		t.Fatalf("Serialize() = %v, want nil", err)
	}
	if !bytes.Equal(serialized, original) {
		t.Fatalf("Serialize(ParseIPv4(x)) = %x, want %x", serialized, original)
	}
}

func TestIPv4OptionsRoundTrip(t *testing.T) {
	link, err := ParseEthernet(DefaultLimits(), decodeVector(t, ipOptionsFrame))
	if err != nil {
		t.Fatalf("ParseEthernet() = %v, want nil", err)
	}
	original := link.Payload

	packet, err := ParseIPv4(DefaultLimits(), original)
	if err != nil {
		t.Fatalf("ParseIPv4(options) = %v, want nil", err)
	}
	if got, want := packet.HeaderLength(), 24; got != want {
		t.Fatalf("HeaderLength() = %d, want %d", got, want)
	}
	if got, want := len(packet.Options), 4; got != want {
		t.Fatalf("Options are %d bytes, want %d", got, want)
	}
	if !packet.DontFragment() {
		t.Fatal("DontFragment() = false, the vector sets the flag")
	}

	serialized, err := packet.Serialize(DefaultLimits())
	if err != nil {
		t.Fatalf("Serialize(options) = %v, want nil", err)
	}
	if !bytes.Equal(serialized, original) {
		t.Fatalf("Serialize(ParseIPv4(x)) = %x, want %x", serialized, original)
	}
}

func TestParseIPv4RejectsMalformedHeaders(t *testing.T) {
	link, err := ParseEthernet(DefaultLimits(), decodeVector(t, udpFrame))
	if err != nil {
		t.Fatalf("ParseEthernet() = %v, want nil", err)
	}
	valid := link.Payload

	tests := []struct {
		name    string
		mutate  func(packet []byte)
		wantErr apperr.Kind
	}{
		{
			name:    "truncated below the minimum header",
			mutate:  func(packet []byte) {},
			wantErr: apperr.KindInvalidArgument,
		},
		{
			name:    "version six",
			mutate:  func(packet []byte) { packet[0] = 0x65 },
			wantErr: apperr.KindInvalidArgument,
		},
		{
			name:    "header length below five words",
			mutate:  func(packet []byte) { packet[0] = 0x44 },
			wantErr: apperr.KindInvalidArgument,
		},
		{
			name:    "total length below the header",
			mutate:  func(packet []byte) { packet[2], packet[3] = 0x00, 0x0a },
			wantErr: apperr.KindInvalidArgument,
		},
		{
			name:    "total length beyond the buffer",
			mutate:  func(packet []byte) { packet[2], packet[3] = 0xff, 0xff },
			wantErr: apperr.KindInvalidArgument,
		},
		{
			name:    "header length beyond the buffer",
			mutate:  func(packet []byte) { packet[0] = 0x4f },
			wantErr: apperr.KindInvalidArgument,
		},
		{
			name:    "corrupt header checksum",
			mutate:  func(packet []byte) { packet[10] ^= 0x0f },
			wantErr: apperr.KindInvalidArgument,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			packet := make([]byte, len(valid))
			copy(packet, valid)
			tc.mutate(packet)
			if tc.name == "truncated below the minimum header" {
				packet = packet[:len(packet)-8]
			}

			_, err := ParseIPv4(DefaultLimits(), packet)
			if !apperr.IsKind(err, tc.wantErr) {
				t.Fatalf("ParseIPv4() = %v, want %s", err, tc.wantErr)
			}
		})
	}
}

func TestParseIPv4RejectsEveryTruncation(t *testing.T) {
	link, err := ParseEthernet(DefaultLimits(), decodeVector(t, udpFrame))
	if err != nil {
		t.Fatalf("ParseEthernet() = %v, want nil", err)
	}

	for length := 0; length < len(link.Payload); length++ {
		if _, err := ParseIPv4(DefaultLimits(), link.Payload[:length]); err == nil {
			t.Fatalf("ParseIPv4(%d of %d bytes) = nil, want an error", length, len(link.Payload))
		}
	}
}

func TestParseIPv4WithoutChecksumVerification(t *testing.T) {
	frame := decodeVector(t, frameCorruptIPChecksum)
	link, err := ParseEthernet(DefaultLimits(), frame)
	if err != nil {
		t.Fatalf("ParseEthernet() = %v, want nil", err)
	}
	if _, err := ParseIPv4(DefaultLimits(), link.Payload); !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("ParseIPv4(corrupt checksum) = %v, want invalid_argument", err)
	}

	limits := DefaultLimits()
	limits.VerifyChecksums = false
	packet, err := ParseIPv4(limits, link.Payload)
	if err != nil {
		t.Fatalf("ParseIPv4(unchecked) = %v, want nil", err)
	}
	if !packet.ChecksumUnchecked {
		t.Fatal("ChecksumUnchecked = false although verification was disabled")
	}
	if packet.ChecksumValid() {
		t.Fatal("ChecksumValid() = true for a corrupt checksum")
	}
}

func TestParseIPv4HonoursPacketLimit(t *testing.T) {
	link, err := ParseEthernet(DefaultLimits(), decodeVector(t, udpFrame))
	if err != nil {
		t.Fatalf("ParseEthernet() = %v, want nil", err)
	}
	limits := DefaultLimits()
	limits.MaxPacketSize = 30

	if _, err := ParseIPv4(limits, link.Payload); !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Fatalf("ParseIPv4(tight limit) = %v, want limit_exceeded", err)
	}
}

func TestParseIPv4RejectsOversizeInput(t *testing.T) {
	oversized := make([]byte, MaxIPv4TotalLength+1)
	oversized[0] = 0x45

	if _, err := ParseIPv4(DefaultLimits(), oversized); !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Fatalf("ParseIPv4(oversized) = %v, want limit_exceeded", err)
	}
}

func TestIPv4PaddingIsPreserved(t *testing.T) {
	link, err := ParseEthernet(DefaultLimits(), decodeVector(t, udpFrame))
	if err != nil {
		t.Fatalf("ParseEthernet() = %v, want nil", err)
	}
	padded := make([]byte, 0, len(link.Payload)+8)
	padded = append(padded, link.Payload...)
	padded = append(padded, make([]byte, 8)...)

	packet, err := ParseIPv4(DefaultLimits(), padded)
	if err != nil {
		t.Fatalf("ParseIPv4(padded) = %v, want nil", err)
	}
	if got, want := len(packet.Padding), 8; got != want {
		t.Fatalf("Padding is %d bytes, want %d", got, want)
	}
	if got, want := packet.TotalLength(), 36; got != want {
		t.Fatalf("TotalLength() = %d, want %d", got, want)
	}

	serialized, err := packet.Serialize(DefaultLimits())
	if err != nil {
		t.Fatalf("Serialize(padded) = %v, want nil", err)
	}
	if !bytes.Equal(serialized, padded) {
		t.Fatalf("Serialize(ParseIPv4(padded)) = %x, want %x", serialized, padded)
	}
}

func TestIPv4SetTimeToLiveRepairsChecksum(t *testing.T) {
	link, err := ParseEthernet(DefaultLimits(), decodeVector(t, udpFrame))
	if err != nil {
		t.Fatalf("ParseEthernet() = %v, want nil", err)
	}
	packet, err := ParseIPv4(DefaultLimits(), link.Payload)
	if err != nil {
		t.Fatalf("ParseIPv4() = %v, want nil", err)
	}

	packet.SetTimeToLive(63)
	serialized, err := packet.Serialize(DefaultLimits())
	if err != nil {
		t.Fatalf("Serialize() = %v, want nil", err)
	}
	if serialized[8] != 63 {
		t.Fatalf("serialized TTL = %d, want 63", serialized[8])
	}
	if !ChecksumValid(serialized[:20]) {
		t.Fatal("the header checksum was not recomputed after the TTL change")
	}

	reparsed, err := ParseIPv4(DefaultLimits(), serialized)
	if err != nil {
		t.Fatalf("ParseIPv4(after TTL change) = %v, want nil", err)
	}
	if reparsed.TimeToLive != 63 {
		t.Fatalf("TimeToLive = %d, want 63", reparsed.TimeToLive)
	}
}

func TestIPv4FragmentAccessors(t *testing.T) {
	packet := &IPv4{
		Source:      netip.MustParseAddr("10.42.0.1"),
		Destination: netip.MustParseAddr("10.42.0.2"),
		TimeToLive:  64,
		Fragment:    IPv4FragmentField(0x2000 | 185),
	}
	if !packet.MoreFragments() {
		t.Fatal("MoreFragments() = false")
	}
	if got, want := packet.FragmentOffset(), 185*8; got != want {
		t.Fatalf("FragmentOffset() = %d, want %d", got, want)
	}
	if !packet.IsFragment() {
		t.Fatal("IsFragment() = false")
	}
	if packet.DontFragment() {
		t.Fatal("DontFragment() = true")
	}
}

func TestIPv4SerializeRejectsInvalidInput(t *testing.T) {
	valid := &IPv4{
		Source:      netip.MustParseAddr("10.42.0.1"),
		Destination: netip.MustParseAddr("10.42.0.2"),
		TimeToLive:  64,
		Protocol:    ProtocolUDP,
		Payload:     []byte{0, 8, 0, 0, 0, 0, 0, 0},
	}

	badOptions := *valid
	badOptions.Options = []byte{1, 2, 3}
	if _, err := badOptions.Serialize(DefaultLimits()); !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("Serialize(unaligned options) = %v, want invalid_argument", err)
	}

	longOptions := *valid
	longOptions.Options = make([]byte, MaxIPv4HeaderSize-MinIPv4HeaderSize+4)
	if _, err := longOptions.Serialize(DefaultLimits()); !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("Serialize(oversized options) = %v, want invalid_argument", err)
	}

	nonIPv4 := *valid
	nonIPv4.Source = netip.MustParseAddr("fd00::1")
	if _, err := nonIPv4.Serialize(DefaultLimits()); !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("Serialize(IPv6 source) = %v, want invalid_argument", err)
	}

	oversized := *valid
	oversized.Payload = make([]byte, MaxIPv4TotalLength)
	if _, err := oversized.Serialize(DefaultLimits()); !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Fatalf("Serialize(oversized payload) = %v, want limit_exceeded", err)
	}

	tight := *valid
	limits := DefaultLimits()
	limits.MaxPacketSize = 24
	if _, err := tight.Serialize(limits); !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Fatalf("Serialize(tight limit) = %v, want limit_exceeded", err)
	}
}

func TestIPv4LengthFieldsDoNotTruncate(t *testing.T) {

	oversized := &IPv4{
		Source:      netip.MustParseAddr("10.42.0.1"),
		Destination: netip.MustParseAddr("10.42.0.2"),
		TimeToLive:  64,
		Payload:     make([]byte, MaxIPv4TotalLength-MinIPv4HeaderSize+1),
	}
	serialized, err := oversized.Serialize(DefaultLimits())
	if err == nil {
		t.Fatalf("Serialize(oversized) = %x, want an error", serialized)
	}
	if !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Fatalf("Serialize(oversized) = %v, want limit_exceeded", err)
	}
	if len(serialized) != 0 {
		t.Fatalf("Serialize(oversized) returned %d bytes, want none", len(serialized))
	}
}

func TestIPv4MinimumPacketRoundTrip(t *testing.T) {
	packet := &IPv4{
		Source:      netip.MustParseAddr("10.42.0.1"),
		Destination: netip.MustParseAddr("10.42.0.2"),
		TimeToLive:  64,
		Protocol:    ProtocolICMP,
	}

	serialized, err := packet.Serialize(DefaultLimits())
	if err != nil {
		t.Fatalf("Serialize() = %v, want nil", err)
	}
	if got, want := len(serialized), MinIPv4TotalLength; got != want {
		t.Fatalf("serialized %d bytes, want %d", got, want)
	}
	if got, want := serialized[2:4], []byte{0x00, 0x14}; !bytes.Equal(got, want) {
		t.Fatalf("total length field = %x, want %x", got, want)
	}

	reparsed, err := ParseIPv4(DefaultLimits(), serialized)
	if err != nil {
		t.Fatalf("ParseIPv4(minimum) = %v, want nil", err)
	}
	if len(reparsed.Payload) != 0 {
		t.Fatalf("Payload is %d bytes, want none", len(reparsed.Payload))
	}
}

func TestIPv4MaximumPacketRoundTrip(t *testing.T) {
	payload := make([]byte, MaxIPv4TotalLength-MinIPv4HeaderSize)
	for i := range payload {
		payload[i] = byte(i)
	}
	packet := &IPv4{
		Source:      netip.MustParseAddr("10.42.0.1"),
		Destination: netip.MustParseAddr("10.42.0.2"),
		TimeToLive:  64,
		Protocol:    ProtocolUDP,
		Payload:     payload,
	}

	serialized, err := packet.Serialize(DefaultLimits())
	if err != nil {
		t.Fatalf("Serialize(maximum) = %v, want nil", err)
	}
	if got, want := len(serialized), MaxIPv4TotalLength; got != want {
		t.Fatalf("serialized %d bytes, want %d", got, want)
	}
	if got, want := int(serialized[2])<<8|int(serialized[3]), MaxIPv4TotalLength; got != want {
		t.Fatalf("total length field = %d, want %d", got, want)
	}
	if _, err := ParseIPv4(DefaultLimits(), serialized); err != nil {
		t.Fatalf("ParseIPv4(maximum) = %v, want nil", err)
	}
}

func TestIPv4SetTimeToLiveAcceptsBoundary(t *testing.T) {
	packet := &IPv4{
		Source:      netip.MustParseAddr("10.42.0.1"),
		Destination: netip.MustParseAddr("10.42.0.2"),
		TimeToLive:  255,
	}
	serialized, err := packet.Serialize(DefaultLimits())
	if err != nil {
		t.Fatalf("Serialize(TTL 255) = %v, want nil", err)
	}
	if got, want := serialized[8], byte(255); got != want {
		t.Fatalf("serialized TTL = %d, want %d", got, want)
	}
}

func TestParseIPv4ReportsDeclaredLength(t *testing.T) {
	frame := decodeVector(t, udpFrameTruncated)
	link, err := ParseEthernet(DefaultLimits(), frame)
	if err != nil {
		t.Fatalf("ParseEthernet() = %v, want nil", err)
	}

	_, err = ParseIPv4(DefaultLimits(), link.Payload)
	if err == nil {
		t.Fatal("ParseIPv4(truncated) = nil, want an error")
	}
	declared := int(binary16(link.Payload[2:4]))
	if !containsMessage(err.Error(), strconv.Itoa(declared)) {
		t.Fatalf("error %q does not mention the declared length %d", err, declared)
	}
}

func binary16(data []byte) uint16 {
	return uint16(data[0])<<8 | uint16(data[1])
}

func containsMessage(message, want string) bool {
	return len(want) > 0 && len(message) >= len(want) && indexOf(message, want) >= 0
}

func indexOf(message, want string) int {
	for i := 0; i+len(want) <= len(message); i++ {
		if message[i:i+len(want)] == want {
			return i
		}
	}
	return -1
}
