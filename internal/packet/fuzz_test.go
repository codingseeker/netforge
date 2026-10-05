package packet

import (
	"bytes"
	"net/netip"
	"testing"
)

func FuzzParseEthernet(f *testing.F) {
	seeds := []string{
		udpFrame, udpFrameZeroID, icmpEchoFrame, icmpUnreachableFrame, tcpSynFrame,
		ipOptionsFrame, broadcastARPFrame, vlanFrame, udpFrameBadChecksum,
		udpFrameTruncated, "", "0200", "020000000001",
	}
	for _, seed := range seeds {
		f.Add(decodeFuzzSeed(f, seed))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		link, err := ParseEthernet(DefaultLimits(), data)
		if err != nil {
			return
		}
		if link.EtherType == EtherTypeIPv4 {

			serialized, err := link.Serialize(DefaultLimits())
			if err != nil {
				t.Fatalf("Serialize() = %v, want nil for a parsed frame", err)
			}
			assertSameBytes(t, serialized, data)
		}
	})
}

func FuzzParseIPv4(f *testing.F) {
	seeds := []string{
		udpFrame, udpFrameZeroID, icmpEchoFrame, icmpUnreachableFrame, tcpSynFrame,
		ipOptionsFrame, udpFrameTruncated, frameBadHeaderLength, frameBadTotalLength,
		frameCorruptIPChecksum, "4500", "45000014", "",
	}
	for _, seed := range seeds {
		f.Add(ipPayloadOf(decodeFuzzSeed(f, seed)))
	}
	for _, seed := range [][]byte{
		{},
		{0x45},
		{0x45, 0x00, 0x00, 0x00},
		{0x60, 0x00, 0x00, 0x00, 0, 0, 0, 0, 64, 17, 0, 0, 10, 0, 0, 1, 10, 0, 0, 1},
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		packet, err := ParseIPv4(DefaultLimits(), data)
		if err != nil {
			return
		}
		serialized, err := packet.Serialize(DefaultLimits())
		if err != nil {
			t.Fatalf("Serialize() = %v, want nil for a parsed packet", err)
		}
		assertSameBytes(t, serialized, data, 10)
		reparsed, err := ParseIPv4(DefaultLimits(), serialized)
		if err != nil {
			t.Fatalf("ParseIPv4(Serialize(ParseIPv4(x))) = %v, want nil", err)
		}
		if reparsed.HeaderChecksum != packet.HeaderChecksum {
			t.Fatalf("the header checksum changed across a round trip")
		}
	})
}

func FuzzParseICMP(f *testing.F) {
	seeds := []string{
		icmpEchoFrame, icmpUnreachableFrame, "", "0800", "08000000", "03010000",
	}
	for _, seed := range seeds {
		f.Add(transportOf(decodeFuzzSeed(f, seed)))
	}
	for _, seed := range [][]byte{
		{},
		{byte(ICMPTypeEchoRequest)},
		{byte(ICMPTypeEchoRequest), 0, 0, 0},
		{byte(ICMPTypeDestinationUnreachable), 0, 0, 0, 0, 0, 0, 0},
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		message, err := ParseICMP(DefaultLimits(), data)
		if err != nil {
			return
		}
		serialized, err := message.Serialize()
		if err != nil {
			t.Fatalf("Serialize() = %v, want nil for a parsed message", err)
		}
		assertSameBytes(t, serialized, data, 2)
		if !ChecksumValid(serialized) {
			t.Fatal("the serialized message does not verify against its own checksum")
		}
	})
}

func FuzzParseUDP(f *testing.F) {
	seeds := []string{
		udpFrame, udpZeroChecksumFrame, "", "0035", "003514e9",
	}
	for _, seed := range seeds {
		f.Add(transportOf(decodeFuzzSeed(f, seed)))
	}
	for _, seed := range [][]byte{
		{},
		{0, 53, 0x14, 0xe9, 0, 8, 0, 0},
		{0, 53, 0x14, 0xe9, 0, 9, 0, 0, 0},
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		datagram, err := ParseUDP(DefaultLimits(), data)
		if err != nil {
			return
		}
		serialized, err := datagram.SerializeRaw()
		if err != nil {
			t.Fatalf("SerializeRaw() = %v, want nil for a parsed datagram", err)
		}

		want := make([]byte, UDPHeaderSize+len(datagram.Payload))
		copy(want, data[:UDPHeaderSize+len(datagram.Payload)])
		putChecksum(want[UDPChecksumOffset:UDPChecksumOffset+2], 0)
		if !bytes.Equal(serialized, want) {
			t.Fatalf("SerializeRaw() = %x, want %x", serialized, want)
		}
		if datagram.ChecksumOmitted != (datagram.Checksum == 0) {
			t.Fatal("ChecksumOmitted disagrees with the checksum field")
		}
	})
}

func FuzzParseTCP(f *testing.F) {
	f.Add(transportOf(decodeFuzzSeed(f, tcpSynFrame)))
	f.Add([]byte{})
	f.Add([]byte{0x9c, 0x40, 0x00, 0x50, 0x11, 0x22, 0x33, 0x44, 0, 0, 0, 0, 0x50, 0x02, 0xff, 0xff})
	f.Add([]byte{0x9c, 0x40, 0x00, 0x50, 0x11, 0x22, 0x33, 0x44, 0, 0, 0, 0, 0x50, 0x02, 0xff, 0xff,
		0x8d, 0xd2, 0, 0, 0x02, 0x04, 0x05, 0xb4})
	f.Add([]byte{0x50, 0x02, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x50, 0x02, 0xff, 0xff})

	f.Fuzz(func(t *testing.T, data []byte) {
		segment, err := ParseTCP(DefaultLimits(), data)
		if err != nil {
			return
		}
		serialized, err := segment.SerializeRaw()
		if err != nil {
			t.Fatalf("SerializeRaw() = %v, want nil for a parsed segment", err)
		}

		want := make([]byte, len(data))
		copy(want, data)
		putChecksum(want[TCPChecksumOffset:TCPChecksumOffset+2], 0)
		if !bytes.Equal(serialized, want) {
			t.Fatalf("SerializeRaw() = %x, want %x", serialized, want)
		}
		if got, want := int(serialized[12]>>4)*4, segment.HeaderLength(); got != want {
			t.Fatalf("data offset = %d, want the header length %d", got, want)
		}
		if got, want := segment.HeaderLength(), len(segment.Options)+MinTCPHeaderSize; got != want {
			t.Fatalf("HeaderLength() = %d, want %d", got, want)
		}
	})
}

func FuzzParseFrame(f *testing.F) {
	seeds := []string{
		udpFrame, udpFrameZeroID, udpFrameZeroID, icmpEchoFrame, icmpEchoRequestZeroIDFrame,
		icmpUnreachableFrame, tcpSynFrame, ipOptionsFrame, broadcastARPFrame, vlanFrame,
		udpFrameBadChecksum, udpFrameTruncated, frameBadHeaderLength, frameBadTotalLength,
		frameCorruptIPChecksum, "", "0200000000010200000000020800",
	}
	for _, seed := range seeds {
		f.Add(decodeFuzzSeed(f, seed))
	}
	for _, seed := range [][]byte{
		{},
		make([]byte, EthernetHeaderSize),
		make([]byte, EthernetHeaderSize+MinIPv4HeaderSize),
		append(make([]byte, EthernetHeaderSize), ipPayloadOf(decodeFuzzSeed(f, ipOptionsFrame))...),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		frame, err := ParseFrame(DefaultLimits(), data)
		if err != nil {
			return
		}
		serialized, err := frame.Serialize(DefaultLimits())
		if err != nil {
			t.Fatalf("Serialize() = %v, want nil for a parsed frame", err)
		}
		assertSameBytes(t, serialized, data, frameChecksumOffsets(frame)...)
		if frame.Size() != EthernetHeaderSize+frame.IP.TotalLength() {
			t.Fatalf("Size() = %d, want %d", frame.Size(), EthernetHeaderSize+frame.IP.TotalLength())
		}
	})
}

func FuzzChecksum(f *testing.F) {
	for _, seed := range []string{"", "00", "0001", "45000024123400004011543f", "ffffffff"} {
		f.Add(decodeFuzzSeed(f, seed))
	}
	for _, seed := range [][]byte{
		{}, {0}, {0, 0}, {0xff, 0xff}, {0x01, 0x02, 0x03, 0x04, 0x05},
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		sum := Checksum(data)
		if sum != Checksum(data) {
			t.Fatal("Checksum() is not deterministic")
		}
		if len(data) >= 2 {

			withField := make([]byte, len(data))
			copy(withField, data)
			putChecksum(withField[0:2], 0)
			putChecksum(withField[0:2], Checksum(withField))
			if !ChecksumValid(withField) {
				t.Fatalf("ChecksumValid() = false for a buffer carrying the checksum of %x", data)
			}
		}

		if len(data) > 2 {
			whole := rawSum(data)
			halves := rawSum(data[:2]) + rawSum(data[2:])
			if whole != halves {
				t.Fatalf("the sum depends on the split: %d and %d", whole, halves)
			}
		}
	})
}

// FuzzRefreshTransportChecksum drives the repair a forwarder performs over
// arbitrary bytes. The property is the one a router depends on: whatever goes
// in, the packet that comes out either verifies or is refused, and its length is
// never anything but what it was.
func FuzzRefreshTransportChecksum(f *testing.F) {
	for _, encoded := range []string{udpFrame, tcpSynFrame, icmpEchoFrame, udpZeroChecksumFrame} {
		f.Add(decodeFuzzSeed(f, encoded))
	}
	for _, seed := range [][]byte{
		{}, {0x45}, {0x45, 0x00}, {0x08, 0x00, 0x00, 0x00},
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < MinIPv4HeaderSize {
			return
		}
		ip := &IPv4{
			TimeToLive:  64,
			Source:      netip.MustParseAddr("10.42.0.2"),
			Destination: netip.MustParseAddr("10.42.0.6"),
			Payload:     append([]byte(nil), data[MinIPv4HeaderSize:]...),
		}
		ip.Protocol = Protocol(data[9])
		before := len(ip.Payload)

		if err := RefreshTransportChecksum(ip); err != nil {
			if len(ip.Payload) != before {
				t.Fatalf("a refused payload of %d bytes was changed to %d bytes", before, len(ip.Payload))
			}
			return
		}
		if len(ip.Payload) != before {
			t.Fatalf("the payload grew from %d to %d bytes", before, len(ip.Payload))
		}
		switch ip.Protocol {
		case ProtocolICMP:
			if len(ip.Payload) < MinICMPHeaderSize {
				return
			}
			if !ChecksumValid(ip.Payload) {
				t.Fatalf("a refreshed ICMP message of %d bytes does not verify", len(ip.Payload))
			}
		case ProtocolUDP:
			if len(ip.Payload) < UDPHeaderSize || isZeroChecksum(ip.Payload, UDPChecksumOffset) {
				return
			}
			if !TransportValid(ip.Source, ip.Destination, ProtocolUDP, ip.Payload) {
				t.Fatalf("a refreshed datagram of %d bytes does not verify", len(ip.Payload))
			}
		case ProtocolTCP:
			if len(ip.Payload) < MinTCPHeaderSize {
				return
			}
			if !TransportValid(ip.Source, ip.Destination, ProtocolTCP, ip.Payload) {
				t.Fatalf("a refreshed segment of %d bytes does not verify", len(ip.Payload))
			}
		}
	})
}

func decodeFuzzSeed(f *testing.F, encoded string) []byte {
	f.Helper()
	data, err := decodeHexString(encoded)
	if err != nil {
		f.Fatalf("fuzz seed is not valid hex: %v", err)
	}
	return data
}

func ipPayloadOf(frame []byte) []byte {
	if len(frame) <= EthernetHeaderSize {
		return nil
	}
	return frame[EthernetHeaderSize:]
}

func transportOf(frame []byte) []byte {
	packet := ipPayloadOf(frame)
	if len(packet) < MinIPv4HeaderSize {
		return nil
	}
	return packet[MinIPv4HeaderSize:]
}

func frameChecksumOffsets(frame *Frame) []int {
	offsets := []int{EthernetHeaderSize + 10}
	headerLength := EthernetHeaderSize + frame.IP.HeaderLength()
	switch {
	case frame.UDP != nil:
		offsets = append(offsets, headerLength+UDPChecksumOffset)
	case frame.TCP != nil:
		offsets = append(offsets, headerLength+TCPChecksumOffset)
	case frame.ICMP != nil:
		offsets = append(offsets, headerLength+2)
	}
	return offsets
}

func assertSameBytes(t *testing.T, got, want []byte, checksumOffsets ...int) {
	t.Helper()
	if bytes.Equal(got, want) {
		return
	}
	expected := make([]byte, len(want))
	copy(expected, want)
	for _, offset := range checksumOffsets {
		if offset < 0 || offset+2 > len(want) || offset+2 > len(got) {
			continue
		}
		if binaryUint16(want[offset:offset+2]) == 0 && binaryUint16(got[offset:offset+2]) == 0xffff {
			putChecksum(expected[offset:offset+2], 0xffff)
		}
	}
	if bytes.Equal(got, expected) {
		return
	}
	t.Fatalf("re-serialized buffer = %x, want %x", got, want)
}
