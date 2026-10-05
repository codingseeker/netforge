package packet

import (
	"bytes"
	"net"
	"net/netip"
	"testing"

	"github.com/netforge/netforge/internal/apperr"
)

func TestParseICMPFromVector(t *testing.T) {
	link, err := ParseEthernet(DefaultLimits(), decodeVector(t, icmpEchoFrame))
	if err != nil {
		t.Fatalf("ParseEthernet() = %v, want nil", err)
	}
	packet, err := ParseIPv4(DefaultLimits(), link.Payload)
	if err != nil {
		t.Fatalf("ParseIPv4() = %v, want nil", err)
	}

	message, err := ParseICMP(DefaultLimits(), packet.Payload)
	if err != nil {
		t.Fatalf("ParseICMP() = %v, want nil", err)
	}
	if message.Type != ICMPTypeEchoRequest {
		t.Fatalf("Type = %d, want %d", message.Type, ICMPTypeEchoRequest)
	}
	if message.Code != 0 {
		t.Fatalf("Code = %d, want 0", message.Code)
	}
	if message.Checksum != 0xaa3c {
		t.Fatalf("ICMPChecksum = 0x%04x, want 0xaa3c", message.Checksum)
	}
	if message.Identifier != 0x1234 {
		t.Fatalf("Identifier = 0x%04x, want 0x1234", message.Identifier)
	}
	if message.Sequence != 1 {
		t.Fatalf("Sequence = %d, want 1", message.Sequence)
	}
	if got, want := string(message.Payload), "ping-payload"; got != want {
		t.Fatalf("Payload = %q, want %q", got, want)
	}
	if got, want := message.TotalLength(), 20; got != want {
		t.Fatalf("TotalLength() = %d, want %d", got, want)
	}
	if !message.ChecksumValid() {
		t.Fatal("ChecksumValid() = false for a correct message")
	}
	if message.ChecksumUnchecked {
		t.Fatal("ChecksumUnchecked = true although verification was requested")
	}
}

func TestParseICMPDestinationUnreachable(t *testing.T) {
	link, err := ParseEthernet(DefaultLimits(), decodeVector(t, icmpUnreachableFrame))
	if err != nil {
		t.Fatalf("ParseEthernet() = %v, want nil", err)
	}
	packet, err := ParseIPv4(DefaultLimits(), link.Payload)
	if err != nil {
		t.Fatalf("ParseIPv4() = %v, want nil", err)
	}

	message, err := ParseICMP(DefaultLimits(), packet.Payload)
	if err != nil {
		t.Fatalf("ParseICMP() = %v, want nil", err)
	}
	if message.Type != ICMPTypeDestinationUnreachable {
		t.Fatalf("Type = %d, want %d", message.Type, ICMPTypeDestinationUnreachable)
	}
	if got, want := message.Code, ICMPCodeHostUnreachable; got != want {
		t.Fatalf("Code = %d, want %d", got, want)
	}
	if got, want := len(message.RestOfHeader), 4; got != want {
		t.Fatalf("RestOfHeader is %d bytes, want %d", got, want)
	}

	if got, want := len(message.Payload), 20; got != want {
		t.Fatalf("Payload is %d bytes, want %d", got, want)
	}

	if got, want := message.RestOfHeader[0]>>4, byte(4); got != want {
		t.Fatalf("quoted datagram has version %d, want 4", got)
	}
	if got, want := message.RestOfHeader[1], byte(0); got != want {
		t.Fatalf("quoted datagram has type of service %d, want 0", got)
	}
	if message.ChecksumValid() != true {
		t.Fatal("ChecksumValid() = false for a correct message")
	}
}

func TestICMPRoundTrip(t *testing.T) {
	for name, encoded := range map[string]string{
		"echo":           icmpEchoFrame,
		"unreachable":    icmpUnreachableFrame,
		"echo reply":     icmpEchoFrame,
		"echo oversized": icmpEchoFrame,
	} {
		t.Run(name, func(t *testing.T) {
			link, err := ParseEthernet(DefaultLimits(), decodeVector(t, encoded))
			if err != nil {
				t.Fatalf("ParseEthernet() = %v, want nil", err)
			}
			packet, err := ParseIPv4(DefaultLimits(), link.Payload)
			if err != nil {
				t.Fatalf("ParseIPv4() = %v, want nil", err)
			}
			original := packet.Payload

			message, err := ParseICMP(DefaultLimits(), original)
			if err != nil {
				t.Fatalf("ParseICMP() = %v, want nil", err)
			}
			serialized, err := message.Serialize()
			if err != nil {
				t.Fatalf("Serialize() = %v, want nil", err)
			}
			if !bytes.Equal(serialized, original) {
				t.Fatalf("Serialize(ParseICMP(x)) = %x, want %x", serialized, original)
			}
		})
	}
}

func TestParseICMPRejectsTruncatedInput(t *testing.T) {
	link, err := ParseEthernet(DefaultLimits(), decodeVector(t, icmpEchoFrame))
	if err != nil {
		t.Fatalf("ParseEthernet() = %v, want nil", err)
	}
	packet, err := ParseIPv4(DefaultLimits(), link.Payload)
	if err != nil {
		t.Fatalf("ParseIPv4() = %v, want nil", err)
	}
	valid := packet.Payload

	for length := 0; length < len(valid); length++ {
		if _, err := ParseICMP(DefaultLimits(), valid[:length]); err == nil {
			t.Fatalf("ParseICMP(%d of %d bytes) = nil, want an error", length, len(valid))
		}
	}
}

func TestParseICMPRejectsBadChecksum(t *testing.T) {
	message := decodeVector(t, icmpEchoFrame)[EthernetHeaderSize+MinIPv4HeaderSize:]
	message[len(message)-1] ^= 0x01

	if _, err := ParseICMP(DefaultLimits(), message); !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("ParseICMP(bad checksum) = %v, want invalid_argument", err)
	}

	limits := DefaultLimits()
	limits.VerifyChecksums = false
	parsed, err := ParseICMP(limits, message)
	if err != nil {
		t.Fatalf("ParseICMP(unchecked) = %v, want nil", err)
	}
	if !parsed.ChecksumUnchecked {
		t.Fatal("ChecksumUnchecked = false although verification was disabled")
	}
}

func TestParseICMPHonoursPacketLimit(t *testing.T) {
	message := decodeVector(t, icmpEchoFrame)[EthernetHeaderSize+MinIPv4HeaderSize:]
	limits := DefaultLimits()
	limits.MaxPacketSize = MinIPv4HeaderSize + 8

	if _, err := ParseICMP(limits, message); !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Fatalf("ParseICMP(tight limit) = %v, want limit_exceeded", err)
	}
}

func TestParseICMPOversizeMessage(t *testing.T) {
	oversized := make([]byte, MaxICMPLength+1)
	oversized[0] = byte(ICMPTypeEchoRequest)

	if _, err := ParseICMP(DefaultLimits(), oversized); !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Fatalf("ParseICMP(oversized) = %v, want limit_exceeded", err)
	}
}

func TestICMPSerializeRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name    string
		message ICMP
	}{
		{
			name: "reserved type",
			message: ICMP{
				Type:       ICMPType(2),
				Code:       0,
				Identifier: 1,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.message.Serialize(); !apperr.IsKind(err, apperr.KindInvalidArgument) {
				t.Fatalf("Serialize() = %v, want invalid_argument", err)
			}
		})
	}
}

func TestICMPSerializeMaximumMessage(t *testing.T) {
	payload := make([]byte, MaxICMPLength-ICMPEchoHeaderSize)
	for i := range payload {
		payload[i] = byte(i)
	}
	message := ICMP{
		Type:       ICMPTypeEchoRequest,
		Identifier: 0x1234,
		Sequence:   7,
		Payload:    payload,
	}

	serialized, err := message.Serialize()
	if err != nil {
		t.Fatalf("Serialize(maximum) = %v, want nil", err)
	}
	if got, want := len(serialized), MaxICMPLength; got != want {
		t.Fatalf("serialized %d bytes, want %d", got, want)
	}

	parsed, err := ParseICMP(DefaultLimits(), serialized)
	if err != nil {
		t.Fatalf("ParseICMP(maximum) = %v, want nil", err)
	}
	if got, want := string(parsed.Payload), string(payload); got != want {
		t.Fatal("the maximum size payload did not survive the round trip")
	}
}

func TestICMPSerializeCoversTheWholeMessage(t *testing.T) {

	quoted := decodeVector(t, udpFrame)[EthernetHeaderSize:]
	quoted = append([]byte(nil), quoted[:28]...)

	var rest [4]byte
	copy(rest[:], quoted[:4])
	message := ICMP{
		Type:         ICMPTypeDestinationUnreachable,
		Code:         ICMPCodeHostUnreachable,
		RestOfHeader: rest,
		Payload:      quoted[4:],
	}

	serialized, err := message.Serialize()
	if err != nil {
		t.Fatalf("Serialize() = %v, want nil", err)
	}
	if got, want := len(serialized), MinICMPHeaderSize+len(quoted); got != want {
		t.Fatalf("serialized %d bytes, want %d", got, want)
	}
	if !ChecksumValid(serialized) {
		t.Fatal("the message checksum does not cover the quoted datagram")
	}
}

func TestEchoRequestMatchesVector(t *testing.T) {
	built, err := EchoRequest(DefaultLimits(),
		net.HardwareAddr{2, 0, 0, 0, 0, 2},
		net.HardwareAddr{2, 0, 0, 0, 0, 1},
		netip.MustParseAddr("10.42.0.2"),
		netip.MustParseAddr("10.42.0.1"),
		0x1234, 1, 64, []byte("ping-payload"))
	if err != nil {
		t.Fatalf("EchoRequest() = %v, want nil", err)
	}
	if want := decodeVector(t, icmpEchoRequestZeroIDFrame); !bytes.Equal(built, want) {
		t.Fatalf("EchoRequest = %x, want %x", built, want)
	}

	frame, err := ParseFrame(DefaultLimits(), built)
	if err != nil {
		t.Fatalf("ParseFrame() = %v, want nil", err)
	}
	if frame.ICMP == nil {
		t.Fatal("the built frame has no ICMP message")
	}
	if frame.ICMP.Type != ICMPTypeEchoRequest {
		t.Fatalf("Type = %d, want echo request", frame.ICMP.Type)
	}
	if got, want := string(frame.ICMP.Payload), "ping-payload"; got != want {
		t.Fatalf("Payload = %q, want %q", got, want)
	}
}

func TestEchoReplySwapsEverything(t *testing.T) {
	requestBytes := decodeVector(t, icmpEchoFrame)
	request, err := ParseFrame(DefaultLimits(), requestBytes)
	if err != nil {
		t.Fatalf("ParseFrame() = %v, want nil", err)
	}

	reply, err := EchoReply(DefaultLimits(), request,
		net.HardwareAddr{2, 0, 0, 0, 0, 1},
		net.HardwareAddr{2, 0, 0, 0, 0, 2})
	if err != nil {
		t.Fatalf("EchoReply() = %v, want nil", err)
	}

	replyFrame, err := ParseFrame(DefaultLimits(), reply)
	if err != nil {
		t.Fatalf("ParseFrame(reply) = %v, want nil", err)
	}
	if replyFrame.ICMP == nil {
		t.Fatal("the reply carries no ICMP message")
	}
	if replyFrame.ICMP.Type != ICMPTypeEchoReply {
		t.Fatalf("reply Type = %d, want echo reply", replyFrame.ICMP.Type)
	}
	if replyFrame.ICMP.Identifier != request.ICMP.Identifier ||
		replyFrame.ICMP.Sequence != request.ICMP.Sequence ||
		!bytes.Equal(replyFrame.ICMP.Payload, request.ICMP.Payload) {
		t.Fatal("the reply does not quote the request identifier, sequence and data")
	}
	if got, want := replyFrame.IP.Source.String(), "10.42.0.1"; got != want {
		t.Fatalf("reply source = %s, want %s", got, want)
	}
	if got, want := replyFrame.IP.Destination.String(), "10.42.0.2"; got != want {
		t.Fatalf("reply destination = %s, want %s", got, want)
	}
	if got, want := replyFrame.Link.Source.String(), "02:00:00:00:00:01"; got != want {
		t.Fatalf("reply link source = %s, want %s", got, want)
	}
	if got, want := replyFrame.Link.Destination.String(), "02:00:00:00:00:02"; got != want {
		t.Fatalf("reply link destination = %s, want %s", got, want)
	}
	if !bytes.Equal(replyFrame.ICMP.Payload, request.ICMP.Payload) {
		t.Fatal("the reply payload does not match the request payload")
	}

	reserialized, err := replyFrame.Serialize(DefaultLimits())
	if err != nil {
		t.Fatalf("Serialize(reply) = %v, want nil", err)
	}
	if !bytes.Equal(reserialized, reply) {
		t.Fatalf("Serialize(ParseFrame(EchoReply)) = %x, want %x", reserialized, reply)
	}
}

func TestEchoReplyRejectsOtherTypes(t *testing.T) {
	link, err := ParseEthernet(DefaultLimits(), decodeVector(t, icmpUnreachableFrame))
	if err != nil {
		t.Fatalf("ParseEthernet() = %v, want nil", err)
	}
	packet, err := ParseIPv4(DefaultLimits(), link.Payload)
	if err != nil {
		t.Fatalf("ParseIPv4() = %v, want nil", err)
	}
	frame := &Frame{Link: link, IP: packet}

	if _, err := EchoReply(DefaultLimits(), frame,
		net.HardwareAddr{2, 0, 0, 0, 0, 2},
		net.HardwareAddr{2, 0, 0, 0, 0, 1}); !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("EchoReply(no echo request) = %v, want invalid_argument", err)
	}
	if _, err := EchoReply(DefaultLimits(), nil,
		net.HardwareAddr{2, 0, 0, 0, 0, 2},
		net.HardwareAddr{2, 0, 0, 0, 0, 1}); !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("EchoReply(nil) = %v, want invalid_argument", err)
	}
}

func TestBuildICMPRejectsIPv6Endpoints(t *testing.T) {
	_, err := EchoRequest(DefaultLimits(),
		net.HardwareAddr{2, 0, 0, 0, 0, 2},
		net.HardwareAddr{2, 0, 0, 0, 0, 1},
		netip.MustParseAddr("fd00::2"),
		netip.MustParseAddr("fd00::1"),
		1, 1, 64, nil)
	if !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("EchoRequest(IPv6) = %v, want invalid_argument", err)
	}
}

func TestBuildUDPRejectsIPv6Endpoints(t *testing.T) {
	_, err := Datagram(DefaultLimits(),
		net.HardwareAddr{2, 0, 0, 0, 0, 2},
		net.HardwareAddr{2, 0, 0, 0, 0, 1},
		netip.MustParseAddr("fd00::2"),
		netip.MustParseAddr("fd00::1"),
		1, 2, 64, nil)
	if !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("Datagram(IPv6) = %v, want invalid_argument", err)
	}
}

func TestICMPSerializeRejectsOversizeMessage(t *testing.T) {
	message := ICMP{
		Type:    ICMPTypeEchoRequest,
		Payload: make([]byte, MaxICMPLength),
	}
	if _, err := message.Serialize(); !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Fatalf("Serialize(oversized) = %v, want limit_exceeded", err)
	}

	atLimit := ICMP{
		Type:    ICMPTypeEchoRequest,
		Payload: make([]byte, MaxICMPLength-ICMPEchoHeaderSize),
	}
	if _, err := atLimit.Serialize(); err != nil {
		t.Fatalf("Serialize(at the limit) = %v, want nil", err)
	}
}

func TestICMPSerializePreservesAShortQuote(t *testing.T) {

	message := ICMP{
		Type:    ICMPTypeDestinationUnreachable,
		Code:    ICMPCodeHostUnreachable,
		Payload: nil,
	}
	serialized, err := message.Serialize()
	if err != nil {
		t.Fatalf("Serialize() = %v, want nil", err)
	}
	if got, want := len(serialized), ICMPErrorHeaderSize; got != want {
		t.Fatalf("serialized %d bytes, want %d", got, want)
	}
	if !ChecksumValid(serialized) {
		t.Fatal("the serialized message does not verify against its own checksum")
	}
	parsed, err := ParseICMP(DefaultLimits(), serialized)
	if err != nil {
		t.Fatalf("ParseICMP() = %v, want nil", err)
	}
	roundTripped, err := parsed.Serialize()
	if err != nil {
		t.Fatalf("Serialize(parsed) = %v, want nil", err)
	}
	if !bytes.Equal(roundTripped, serialized) {
		t.Fatalf("Serialize(ParseICMP(Serialize(x))) = %x, want %x", roundTripped, serialized)
	}
}

func TestICMPSerializeReproducesMalformed(t *testing.T) {
	message := []byte{0x08, 0x2a, 0x00, 0x00, 0x12, 0x34, 0x00, 0x01}
	putChecksum(message[2:4], checksumField(rawSum(message)))

	parsed, err := ParseICMP(DefaultLimits(), message)
	if err != nil {
		t.Fatalf("ParseICMP(nonzero echo code) = %v, want nil", err)
	}
	if parsed.Code != 0x2a {
		t.Fatalf("Code = %d, want the received 42", parsed.Code)
	}
	serialized, err := parsed.Serialize()
	if err != nil {
		t.Fatalf("Serialize() = %v, want nil", err)
	}
	if !bytes.Equal(serialized, message) {
		t.Fatalf("Serialize(ParseICMP(x)) = %x, want %x", serialized, message)
	}
}
