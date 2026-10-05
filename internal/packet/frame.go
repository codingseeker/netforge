package packet

import (
	"net"
	"net/netip"
	"strconv"

	"github.com/netforge/netforge/internal/apperr"
)

type Frame struct {
	Link *Ethernet
	IP   *IPv4
	ICMP *ICMP
	UDP  *UDP
	TCP  *TCP

	Padding []byte

	PayloadTail []byte
}

func ParseFrame(limits Limits, data []byte) (*Frame, error) {
	link, err := ParseEthernet(limits, data)
	if err != nil {
		return nil, err
	}
	switch link.EtherType {
	case EtherTypeIPv4:
	case EtherTypeVLAN:
		return nil, apperr.Unsupported("packet.ParseFrame",
			"VLAN tagged frames are not decoded, ethertype 0x"+hex16(uint16(link.EtherType)), nil)
	default:
		return nil, apperr.Unsupported("packet.ParseFrame",
			"ethertype 0x"+hex16(uint16(link.EtherType))+" is not IPv4", nil)
	}

	ip, err := ParseIPv4(limits, link.Payload)
	if err != nil {
		return nil, err
	}
	frame := &Frame{Link: link, IP: ip}

	switch ip.Protocol {
	case ProtocolICMP:
		message, err := ParseICMP(limits, ip.Payload)
		if err != nil {
			return nil, err
		}
		frame.ICMP = message
	case ProtocolUDP:
		datagram, err := ParseUDP(limits, ip.Payload)
		if err != nil {
			return nil, err
		}
		if limits.VerifyChecksums {
			if err := datagram.Validate(ip.Source, ip.Destination); err != nil {
				return nil, err
			}
		}
		frame.UDP = datagram
		if tail := ip.Payload[UDPHeaderSize+len(datagram.Payload):]; len(tail) > 0 {
			frame.PayloadTail = tail
		}
	case ProtocolTCP:
		segment, err := ParseTCP(limits, ip.Payload)
		if err != nil {
			return nil, err
		}
		if limits.VerifyChecksums {
			if err := segment.Validate(ip.Source, ip.Destination); err != nil {
				return nil, err
			}
		}
		frame.TCP = segment
	}
	frame.Padding = ip.Padding
	return frame, nil
}

func (f *Frame) Serialize(limits Limits) ([]byte, error) {
	if f == nil || f.Link == nil || f.IP == nil {
		return nil, apperr.InvalidArgument("packet.Frame.Serialize", "frame is incomplete", nil)
	}

	var transport []byte
	var err error
	switch {
	case f.ICMP != nil:
		transport, err = f.ICMP.Serialize()
	case f.UDP != nil:
		if f.UDP.ChecksumOmitted {
			transport, err = f.UDP.SerializeNoSum()
		} else {
			transport, err = f.UDP.Serialize(f.IP.Source, f.IP.Destination)
		}
	case f.TCP != nil:
		transport, err = f.TCP.Serialize(f.IP.Source, f.IP.Destination)
	}
	if err != nil {
		return nil, err
	}
	if transport != nil {
		if len(f.PayloadTail) > 0 {
			transport = append(transport, f.PayloadTail...)
		}
		f.IP.Payload = transport
	}
	f.Link.EtherType = EtherTypeIPv4
	f.Link.Payload = nil

	packet, err := f.IP.Serialize(limits)
	if err != nil {
		return nil, err
	}
	f.Link.Payload = packet
	return f.Link.Serialize(limits)
}

func (f *Frame) Size() int {
	if f == nil || f.IP == nil {
		return 0
	}
	return EthernetHeaderSize + f.IP.TotalLength()
}

func EchoRequest(limits Limits, sourceMAC, destinationMAC net.HardwareAddr, source, destination netip.Addr, identifier, sequence uint16, ttl uint8, payload []byte) ([]byte, error) {
	const op = "packet.EchoRequest"
	message := &ICMP{
		Type:       ICMPTypeEchoRequest,
		Identifier: identifier,
		Sequence:   sequence,
		Payload:    payload,
	}
	segment, err := message.Serialize()
	if err != nil {
		return nil, err
	}
	return buildIPv4Frame(limits, op, sourceMAC, destinationMAC, source, destination, ProtocolICMP, ttl, segment)
}

func EchoReply(limits Limits, request *Frame, sourceMAC, destinationMAC net.HardwareAddr) ([]byte, error) {
	const op = "packet.EchoReply"
	if request == nil || request.ICMP == nil || request.IP == nil {
		return nil, apperr.InvalidArgument(op, "the request carries no ICMP echo message", nil)
	}
	if request.ICMP.Type != ICMPTypeEchoRequest {
		return nil, apperr.InvalidArgument(op,
			"ICMP type "+strconv.Itoa(int(request.ICMP.Type))+" is not an echo request", nil)
	}

	message := &ICMP{
		Type:       ICMPTypeEchoReply,
		Identifier: request.ICMP.Identifier,
		Sequence:   request.ICMP.Sequence,
		Payload:    request.ICMP.Payload,
	}
	segment, err := message.Serialize()
	if err != nil {
		return nil, err
	}
	return buildIPv4Frame(limits, op, sourceMAC, destinationMAC,
		request.IP.Destination, request.IP.Source, ProtocolICMP, request.IP.TimeToLive, segment)
}

func Datagram(limits Limits, sourceMAC, destinationMAC net.HardwareAddr, source, destination netip.Addr, sourcePort, destinationPort uint16, ttl uint8, payload []byte) ([]byte, error) {
	const op = "packet.Datagram"
	datagram := &UDP{
		SourcePort:      sourcePort,
		DestinationPort: destinationPort,
		Payload:         payload,
	}
	segment, err := datagram.Serialize(source, destination)
	if err != nil {
		return nil, err
	}
	return buildIPv4Frame(limits, op, sourceMAC, destinationMAC, source, destination, ProtocolUDP, ttl, segment)
}

func buildIPv4Frame(limits Limits, op string, sourceMAC, destinationMAC net.HardwareAddr, source, destination netip.Addr, protocol Protocol, ttl uint8, segment []byte) ([]byte, error) {
	link := &Ethernet{
		Destination: destinationMAC,
		Source:      sourceMAC,
		EtherType:   EtherTypeIPv4,
	}
	ip := &IPv4{
		TimeToLive:  ttl,
		Protocol:    protocol,
		Source:      source,
		Destination: destination,
		Payload:     segment,
	}
	frame := &Frame{Link: link, IP: ip}
	frame.Link.Payload = segment
	packet, err := ip.Serialize(limits)
	if err != nil {
		return nil, apperr.Wrap(err, kindOf(err), op, "cannot serialize the IPv4 packet")
	}
	link.Payload = packet
	return link.Serialize(limits)
}

func kindOf(err error) apperr.Kind {
	kind, _ := apperr.KindOf(err)
	return kind
}
