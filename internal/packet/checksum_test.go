package packet

import (
	"math/rand"
	"net/netip"
	"testing"

	"github.com/netforge/netforge/internal/apperr"
)

func referenceChecksum(data []byte) uint16 {
	sum := 0
	for i := 0; i+1 < len(data); i += 2 {
		sum += int(data[i])<<8 | int(data[i+1])
	}
	if len(data)%2 == 1 {
		sum += int(data[len(data)-1]) << 8
	}
	for sum > 0xffff {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return uint16(^sum)
}

func TestChecksumMatchesReference(t *testing.T) {
	random := rand.New(rand.NewSource(20260101))
	lengths := []int{0, 1, 2, 3, 4, 5, 7, 8, 15, 16, 17, 31, 32, 33, 63, 64, 127, 1024, 4095}

	for _, length := range lengths {
		data := make([]byte, length)
		for i := range data {
			data[i] = byte(random.Intn(256))
		}
		if got, want := Checksum(data), referenceChecksum(data); got != want {
			t.Fatalf("Checksum(%d bytes) = 0x%04x, reference gives 0x%04x", length, got, want)
		}
	}
}

func TestChecksumFillsFieldToZero(t *testing.T) {
	data := []byte{0x45, 0x00, 0x00, 0x73, 0x00, 0x00, 0x40, 0x00,
		0x40, 0x11, 0x00, 0x00, 0x0a, 0x2a, 0x00, 0x01, 0x0a, 0x2a, 0x00, 0x02}
	withField := make([]byte, len(data))
	copy(withField, data)
	putChecksum(withField[10:12], 0)

	sum := Checksum(withField)
	putChecksum(withField[10:12], sum)
	if !ChecksumValid(withField) {
		t.Fatal("ChecksumValid() = false for a packet carrying the computed checksum")
	}
	if ChecksumValid(withField) != true {
		t.Fatal("checksum verification is not repeatable")
	}

	putChecksum(withField[10:12], sum+1)
	if ChecksumValid(withField) {
		t.Fatal("ChecksumValid() = true after the checksum was corrupted")
	}
}

func TestChecksumOfOddLengthData(t *testing.T) {
	data := []byte{0x01, 0x02, 0x03}
	if got, want := Checksum(data), referenceChecksum(data); got != want {
		t.Fatalf("Checksum(odd length) = 0x%04x, want 0x%04x", got, want)
	}
}

func TestPseudoSum(t *testing.T) {
	source := netip.MustParseAddr("10.42.0.1")
	destination := netip.MustParseAddr("10.42.0.2")

	expected := referenceChecksum([]byte{
		10, 42, 0, 1,
		10, 42, 0, 2,
		0, 17, 0, 16,
	})
	if got := PseudoSum(source, destination, ProtocolUDP, 16); got != expected {
		t.Fatalf("PseudoSum() = 0x%04x, want 0x%04x", got, expected)
	}
	if got := PseudoSum(source, destination, ProtocolTCP, 16); got == expected {
		t.Fatal("PseudoSum() ignored the protocol number")
	}
	if got := PseudoSum(source, netip.MustParseAddr("10.42.0.3"), ProtocolUDP, 16); got == expected {
		t.Fatal("PseudoSum() ignored the destination")
	}
}

func TestTransportSumRoundTrip(t *testing.T) {
	source := netip.MustParseAddr("10.42.0.1")
	destination := netip.MustParseAddr("10.42.0.2")
	segment := []byte{0x00, 0x35, 0x14, 0xe9, 0x00, 0x10, 0x00, 0x00, 0x6e, 0x65, 0x74, 0x66}

	sum := TransportSum(source, destination, ProtocolUDP, segment)
	putChecksum(segment[6:8], sum)
	if !TransportValid(source, destination, ProtocolUDP, segment) {
		t.Fatal("TransportValid() = false for a correctly checksummed segment")
	}

	segment[9] ^= 0x01
	if TransportValid(source, destination, ProtocolUDP, segment) {
		t.Fatal("TransportValid() = true after the payload was corrupted")
	}
	if TransportValid(source, netip.MustParseAddr("10.42.0.9"), ProtocolUDP, segment) {
		t.Fatal("TransportValid() = true for the wrong destination")
	}
}

func TestZeroUDPChecksumMeansOmitted(t *testing.T) {
	source := netip.MustParseAddr("10.42.0.1")
	destination := netip.MustParseAddr("10.42.0.2")
	segment := []byte{0x00, 0x35, 0x14, 0xe9, 0x00, 0x10, 0x00, 0x00, 0x6e, 0x65, 0x74, 0x66}

	if !TransportValid(source, destination, ProtocolUDP, segment) {
		t.Fatal("a zero UDP checksum must be accepted, IPv4 permits omitting it")
	}
	if TransportValid(source, destination, ProtocolTCP, segment) {
		t.Fatal("a zero TCP checksum must be rejected, TCP has no omitted form")
	}
}

func TestWriteAddrRejectsNonIPv4(t *testing.T) {
	dst := make([]byte, 4)
	if err := WriteAddr(dst, netip.MustParseAddr("10.42.0.7")); err != nil {
		t.Fatalf("WriteAddr() = %v, want nil", err)
	}
	if got := netip.AddrFrom4([4]byte(dst)); got.String() != "10.42.0.7" {
		t.Fatalf("WriteAddr() wrote %s, want 10.42.0.7", got)
	}

	if err := WriteAddr(dst, netip.MustParseAddr("fd00::1")); !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("WriteAddr(IPv6) = %v, want invalid_argument", err)
	}
	if err := WriteAddr(dst[:2], netip.MustParseAddr("10.42.0.7")); !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("WriteAddr(short buffer) = %v, want invalid_argument", err)
	}
	if err := WriteAddr(dst, netip.Addr{}); !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("WriteAddr(invalid) = %v, want invalid_argument", err)
	}
}
