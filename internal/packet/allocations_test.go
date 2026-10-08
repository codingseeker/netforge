package packet

import (
	"bytes"
	"testing"

	"github.com/codingseeker/netforge/internal/apperr"
)

const allocationCeiling = 8

func TestParsingDoesNotAllocatePerByte(t *testing.T) {

	small := decodeVector(t, udpFrame)
	large := buildLargeFrame(t)

	smallAllocs := testing.AllocsPerRun(20, func() {
		if _, err := ParseFrame(DefaultLimits(), small); err != nil {
			t.Fatalf("ParseFrame(small) = %v, want nil", err)
		}
	})
	largeAllocs := testing.AllocsPerRun(20, func() {
		if _, err := ParseFrame(DefaultLimits(), large); err != nil {
			t.Fatalf("ParseFrame(large) = %v, want nil", err)
		}
	})
	if largeAllocs > smallAllocs+allocationCeiling {
		t.Fatalf("parsing a %d byte frame costs %v allocations, the small frame costs %v",
			len(large), largeAllocs, smallAllocs)
	}
	if largeAllocs > allocationCeiling*4 {
		t.Fatalf("parsing a %d byte frame costs %v allocations, the ceiling is %d",
			len(large), largeAllocs, allocationCeiling*4)
	}
}

func TestRejectedInputDoesNotAllocateByLength(t *testing.T) {

	frame := decodeVector(t, udpFrame)
	frame[16], frame[17] = 0xff, 0xff

	allocs := testing.AllocsPerRun(50, func() {
		if _, err := ParseFrame(DefaultLimits(), frame); err == nil {
			t.Fatal("ParseFrame() = nil, want an error")
		}
	})
	if allocs > allocationCeiling {
		t.Fatalf("rejecting a frame costs %v allocations, the ceiling is %d", allocs, allocationCeiling)
	}
}

func TestOversizeInputIsRejectedWithoutReadingIt(t *testing.T) {

	oversized := make([]byte, MaxFrameSize+1)

	allocs := testing.AllocsPerRun(20, func() {
		if _, err := ParseEthernet(DefaultLimits(), oversized); !apperr.IsKind(err, apperr.KindLimitExceeded) {
			t.Fatalf("ParseEthernet(oversized) = %v, want limit_exceeded", err)
		}
	})
	if allocs > allocationCeiling {
		t.Fatalf("rejecting an oversized frame costs %v allocations, the ceiling is %d", allocs, allocationCeiling)
	}
}

func TestTightLimitsAreAppliedBeforeParsing(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxFrameSize = 40
	limits.MaxPacketSize = 30

	frame := decodeVector(t, udpFrame)
	allocs := testing.AllocsPerRun(20, func() {
		if _, err := ParseFrame(limits, frame); !apperr.IsKind(err, apperr.KindLimitExceeded) {
			t.Fatalf("ParseFrame(tight limits) = %v, want limit_exceeded", err)
		}
	})
	if allocs > allocationCeiling {
		t.Fatalf("rejecting a frame over the limit costs %v allocations, the ceiling is %d", allocs, allocationCeiling)
	}
}

func TestParseAliasesTheCallerBuffer(t *testing.T) {

	original := decodeVector(t, udpFrame)
	frame, err := ParseFrame(DefaultLimits(), original)
	if err != nil {
		t.Fatalf("ParseFrame() = %v, want nil", err)
	}
	if &frame.UDP.Payload[0] != &original[EthernetHeaderSize+MinIPv4HeaderSize+UDPHeaderSize] {
		t.Fatal("the payload was copied instead of aliasing the input")
	}
	if !bytes.Equal(frame.UDP.Payload, original[EthernetHeaderSize+MinIPv4HeaderSize+UDPHeaderSize:]) {
		t.Fatal("the aliased payload does not match the input")
	}
}

func TestSerializingAllocatesOnlyTheOutput(t *testing.T) {
	source := decodeVector(t, udpFrame)
	link, err := ParseEthernet(DefaultLimits(), source)
	if err != nil {
		t.Fatalf("ParseEthernet() = %v, want nil", err)
	}
	ip, err := ParseIPv4(DefaultLimits(), link.Payload)
	if err != nil {
		t.Fatalf("ParseIPv4() = %v, want nil", err)
	}
	frame := &Frame{Link: link, IP: ip, UDP: &UDP{SourcePort: 53, DestinationPort: 5353, Payload: []byte("netforge")}}

	allocs := testing.AllocsPerRun(20, func() {
		if _, err := frame.Serialize(DefaultLimits()); err != nil {
			t.Fatalf("Serialize() = %v, want nil", err)
		}
	})
	if allocs > allocationCeiling*2 {
		t.Fatalf("serializing costs %v allocations, the ceiling is %d", allocs, allocationCeiling*2)
	}
}

func buildLargeFrame(t *testing.T) []byte {
	t.Helper()
	link := make([]byte, EthernetHeaderSize)
	copy(link, decodeVector(t, udpFrame))
	frame := make([]byte, 0, EthernetHeaderSize+MaxIPv4TotalLength)
	frame = append(frame, link...)

	ipLength := MaxIPv4TotalLength
	packet := make([]byte, ipLength)
	packet[0] = 0x45
	packet[2], packet[3] = 0xff, 0xff
	packet[8] = 64
	packet[9] = byte(ProtocolUDP)
	copy(packet[12:16], []byte{10, 42, 0, 1})
	copy(packet[16:20], []byte{10, 42, 0, 2})
	packet[20], packet[21] = 0x00, 0x35
	packet[22], packet[23] = 0x14, 0xe9
	packet[24], packet[25] = 0xff, 0xeb

	putChecksum(packet[10:12], checksumField(rawSum(packet[:MinIPv4HeaderSize])))
	return append(frame, packet...)
}
