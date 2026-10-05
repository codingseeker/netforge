package packet

import (
	"net/netip"
	"testing"

	"github.com/netforge/netforge/internal/apperr"
)

func parsedFrame(t *testing.T, encoded string) *Frame {
	t.Helper()
	frame, err := ParseFrame(DefaultLimits(), decodeVector(t, encoded))
	if err != nil {
		t.Fatalf("ParseFrame() = %v, want nil", err)
	}
	return frame
}

func TestRefreshTransportChecksumRepairsAnUnfinishedSeed(t *testing.T) {
	for _, test := range []struct {
		name   string
		frame  string
		offset int
	}{
		{"ICMP", icmpEchoFrame, 2},
		{"UDP", udpFrame, UDPHeaderSize - 2},
		{"TCP", tcpSynFrame, TCPChecksumOffset},
	} {
		t.Run(test.name, func(t *testing.T) {
			frame := parsedFrame(t, test.frame)

			putChecksum(frame.IP.Payload[test.offset:test.offset+2], 0x1234)
			if err := RefreshTransportChecksum(frame.IP); err != nil {
				t.Fatalf("RefreshTransportChecksum() = %v, want nil", err)
			}
			if !frame.IP.ChecksumValid() {
				t.Error("the refreshed packet's own header does not verify")
			}
			switch frame.IP.Protocol {
			case ProtocolICMP:
				if !frame.ICMP.ChecksumValid() {
					t.Error("the refreshed ICMP checksum does not verify")
				}
			case ProtocolUDP:
				if !TransportValid(frame.IP.Source, frame.IP.Destination, ProtocolUDP, frame.IP.Payload) {
					t.Error("the refreshed datagram's checksum does not verify")
				}
			case ProtocolTCP:
				if !TransportValid(frame.IP.Source, frame.IP.Destination, ProtocolTCP, frame.IP.Payload) {
					t.Error("the refreshed segment's checksum does not verify")
				}
			}
		})
	}
}

func TestRefreshTransportChecksumLeavesACorrectFrameAlone(t *testing.T) {
	for _, test := range []struct {
		name    string
		encoded string
	}{
		{"ICMP", icmpEchoFrame},
		{"UDP", udpFrame},
		{"TCP", tcpSynFrame},
	} {
		t.Run(test.name, func(t *testing.T) {
			encoded := test.encoded
			frame := parsedFrame(t, encoded)
			before := append([]byte(nil), frame.IP.Payload...)
			if err := RefreshTransportChecksum(frame.IP); err != nil {
				t.Fatalf("RefreshTransportChecksum() = %v, want nil", err)
			}
			if string(frame.IP.Payload) != string(before) {
				t.Errorf("a correct frame was changed:\n got %x\nwant %x", frame.IP.Payload, before)
			}
		})
	}
}

func TestRefreshTransportChecksumKeepsAnOmittedDatagramChecksum(t *testing.T) {
	frame := parsedFrame(t, udpZeroChecksumFrame)
	if !frame.UDP.ChecksumOmitted {
		t.Fatal("the vector is not a datagram with an omitted checksum")
	}
	before := append([]byte(nil), frame.IP.Payload...)
	if err := RefreshTransportChecksum(frame.IP); err != nil {
		t.Fatalf("RefreshTransportChecksum() = %v, want nil", err)
	}
	if !frame.UDP.ChecksumOmitted {
		t.Error("an omitted checksum was replaced by a computed one")
	}
	if string(frame.IP.Payload) != string(before) {
		t.Errorf("the datagram changed while its omitted checksum was kept:\n got %x\nwant %x",
			frame.IP.Payload, before)
	}
}

func TestRefreshTransportChecksumIgnoresProtocolsItCannotSum(t *testing.T) {
	frame := parsedFrame(t, icmpEchoFrame)
	frame.IP.Protocol = Protocol(47)
	frame.IP.Payload = []byte{0x00, 0x00, 0x00, 0x00, 0xde, 0xad, 0xbe, 0xef}
	before := append([]byte(nil), frame.IP.Payload...)

	if err := RefreshTransportChecksum(frame.IP); err != nil {
		t.Fatalf("RefreshTransportChecksum() = %v, want nil", err)
	}
	if string(frame.IP.Payload) != string(before) {
		t.Errorf("an unknown protocol's payload was changed:\n got %x\nwant %x", frame.IP.Payload, before)
	}
}

func TestRefreshTransportChecksumRefusesATruncatedPayload(t *testing.T) {
	for _, test := range []struct {
		name     string
		protocol Protocol
		payload  []byte
	}{
		{"ICMP", ProtocolICMP, []byte{0x08, 0x00, 0x00}},
		{"UDP", ProtocolUDP, []byte{0x00, 0x35, 0x14, 0xe9}},
		{"TCP", ProtocolTCP, []byte{0x9c, 0x40, 0x00, 0x50, 0x11, 0x22, 0x33, 0x44}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ip := &IPv4{
				Protocol:    test.protocol,
				Source:      netip.MustParseAddr("10.42.0.2"),
				Destination: netip.MustParseAddr("10.42.0.6"),
				Payload:     append([]byte(nil), test.payload...),
			}
			err := RefreshTransportChecksum(ip)
			if err == nil {
				t.Fatalf("a %d byte %s payload was accepted", len(test.payload), test.name)
			}
			if kind, ok := apperr.KindOf(err); !ok || kind != apperr.KindInvalidArgument {
				t.Errorf("the refusal carries kind %q, want an invalid argument: %v", kind, err)
			}
			if string(ip.Payload) != string(test.payload) {
				t.Errorf("a refused payload was changed:\n got %x\nwant %x", ip.Payload, test.payload)
			}
		})
	}
}

func TestRefreshTransportChecksumNeedsNothingToDo(t *testing.T) {
	ip := &IPv4{Protocol: ProtocolUDP, Payload: nil}
	if err := RefreshTransportChecksum(ip); err != nil {
		t.Errorf("RefreshTransportChecksum() on an empty payload = %v, want nil", err)
	}
	if err := RefreshTransportChecksum(nil); err == nil {
		t.Error("RefreshTransportChecksum(nil) was accepted")
	}
}
