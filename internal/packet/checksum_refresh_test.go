package packet

import (
	"net/netip"
	"testing"

	"github.com/netforge/netforge/internal/apperr"
)

// These tests cover the checksum a forwarder has to repair rather than trust.
//
// A frame read from a packet socket shows the bytes as they are on the wire. A
// sender whose stack offloads the transport sum writes only the seed of the
// checksum into those bytes and leaves the rest to the receiving kernel, so a
// forwarder that copies them hands the far end a packet it rejects as corrupt.
// The audit that found this watched a refused connection that never answered
// and looked exactly like a path that was down.

func parsedFrame(t *testing.T, encoded string) *Frame {
	t.Helper()
	frame, err := ParseFrame(DefaultLimits(), decodeVector(t, encoded))
	if err != nil {
		t.Fatalf("ParseFrame() = %v, want nil", err)
	}
	return frame
}

// TestRefreshTransportChecksumRepairsAnUnfinishedSeed replaces the checksum of
// each protocol with the seed an offloading sender would have left, and requires
// the refresh to produce a checksum that verifies.
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
			// A seed is any value that does not verify: the sending stack had
			// not finished the sum, so whatever is in the field is wrong.
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

// TestRefreshTransportChecksumLeavesACorrectFrameAlone checks the other half: a
// frame that already verifies must come out of the refresh byte for byte as it
// went in. A repair that changed a correct frame would be a second defect.
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

// TestRefreshTransportChecksumKeepsAnOmittedDatagramChecksum covers RFC 768: a
// zero checksum says the sender omitted it, so a repair that computed one would
// be changing the datagram rather than fixing it.
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

// TestRefreshTransportChecksumIgnoresProtocolsItCannotSum leaves an unknown
// protocol exactly as it arrived. The bytes of a protocol this layer does not
// implement are not this layer's to change.
func TestRefreshTransportChecksumIgnoresProtocolsItCannotSum(t *testing.T) {
	frame := parsedFrame(t, icmpEchoFrame)
	frame.IP.Protocol = Protocol(47) // GRE
	frame.IP.Payload = []byte{0x00, 0x00, 0x00, 0x00, 0xde, 0xad, 0xbe, 0xef}
	before := append([]byte(nil), frame.IP.Payload...)

	if err := RefreshTransportChecksum(frame.IP); err != nil {
		t.Fatalf("RefreshTransportChecksum() = %v, want nil", err)
	}
	if string(frame.IP.Payload) != string(before) {
		t.Errorf("an unknown protocol's payload was changed:\n got %x\nwant %x", frame.IP.Payload, before)
	}
}

// TestRefreshTransportChecksumRefusesATruncatedPayload refuses a payload too
// short to hold the checksum field it is being asked to write, rather than
// writing outside the slice.
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

// TestRefreshTransportChecksumRefusesNothing checks the two cases that are not a
// refusal: an empty payload, which is nothing to checksum, and no packet at all,
// which is reported rather than dereferenced.
func TestRefreshTransportChecksumNeedsNothingToDo(t *testing.T) {
	ip := &IPv4{Protocol: ProtocolUDP, Payload: nil}
	if err := RefreshTransportChecksum(ip); err != nil {
		t.Errorf("RefreshTransportChecksum() on an empty payload = %v, want nil", err)
	}
	if err := RefreshTransportChecksum(nil); err == nil {
		t.Error("RefreshTransportChecksum(nil) was accepted")
	}
}
