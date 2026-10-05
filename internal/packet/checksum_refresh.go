package packet

import (
	"strconv"

	"github.com/netforge/netforge/internal/apperr"
)

// RefreshTransportChecksum recomputes the transport checksum of a packet that is
// about to be forwarded, in place.
//
// The bytes of a received packet may hold only the seed of a checksum its
// sender never finished. A sender whose network stack offloads the transport
// sum writes the seed into the checksum field and leaves the rest to the
// receiving kernel, and a router that reads frames from a packet socket sees
// those bytes rather than the finished checksum. Forwarding them unchanged hands
// the far end a packet it will reject as corrupt, and nothing on the wire says
// why: a refused connection that never answers looks exactly like a path that is
// down. Recomputing the sum here, over the packet as it will actually be sent,
// is what makes a forwarded packet a packet.
//
// The IPv4 header checksum is not affected. That one is always computed by the
// sending stack, and a router rewrites it for itself when it changes the time to
// live.
//
// A protocol this layer cannot checksum is left exactly as it arrived rather
// than guessed at. The bytes go out unchanged, which is what happened before,
// and the router's own decision to forward an unknown protocol is recorded
// separately.
func RefreshTransportChecksum(ip *IPv4) error {
	const op = "packet.RefreshTransportChecksum"
	if ip == nil {
		return apperr.InvalidArgument(op, "there is no packet to refresh", nil)
	}
	if len(ip.Payload) == 0 {
		return nil
	}
	switch ip.Protocol {
	case ProtocolICMP:
		if len(ip.Payload) < MinICMPHeaderSize {
			return apperr.InvalidArgument(op,
				"ICMP message is "+strconv.Itoa(len(ip.Payload))+" bytes, the header needs "+
					strconv.Itoa(MinICMPHeaderSize), nil)
		}
		putChecksum(ip.Payload[2:4], 0)
		putChecksum(ip.Payload[2:4], checksumField(rawSum(ip.Payload)))
		return nil
	case ProtocolUDP:
		if len(ip.Payload) < UDPHeaderSize {
			return apperr.InvalidArgument(op,
				"datagram is "+strconv.Itoa(len(ip.Payload))+" bytes, the header needs "+
					strconv.Itoa(UDPHeaderSize), nil)
		}
		// RFC 768 makes a zero checksum mean that the sender omitted it. Turning
		// an omitted checksum into a computed one would change the datagram
		// rather than repair it, and the far end accepts both.
		if isZeroChecksum(ip.Payload, UDPChecksumOffset) {
			return nil
		}
		putChecksum(ip.Payload[UDPChecksumOffset:UDPChecksumOffset+2], 0)
		putChecksum(ip.Payload[UDPChecksumOffset:UDPChecksumOffset+2],
			TransportSum(ip.Source, ip.Destination, ProtocolUDP, ip.Payload))
		return nil
	case ProtocolTCP:
		if len(ip.Payload) < MinTCPHeaderSize {
			return apperr.InvalidArgument(op,
				"segment is "+strconv.Itoa(len(ip.Payload))+" bytes, the header needs "+
					strconv.Itoa(MinTCPHeaderSize), nil)
		}
		putChecksum(ip.Payload[TCPChecksumOffset:TCPChecksumOffset+2], 0)
		putChecksum(ip.Payload[TCPChecksumOffset:TCPChecksumOffset+2],
			TransportSum(ip.Source, ip.Destination, ProtocolTCP, ip.Payload))
		return nil
	default:
		return nil
	}
}
