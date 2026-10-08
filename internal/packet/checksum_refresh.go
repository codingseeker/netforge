package packet

import (
	"strconv"

	"github.com/codingseeker/netforge/internal/apperr"
)

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
