package packet

import (
	"encoding/binary"
	"net/netip"
	"strconv"

	"github.com/codingseeker/netforge/internal/apperr"
)

const UDPChecksumOffset = 6

type UDP struct {
	SourcePort      uint16
	DestinationPort uint16
	Checksum        uint16
	Payload         []byte

	ChecksumOmitted bool
}

func ParseUDP(limits Limits, data []byte) (*UDP, error) {
	const op = "packet.ParseUDP"
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if len(data) > limits.MaxPacketSize-MinIPv4HeaderSize {
		return nil, apperr.LimitExceeded(op,
			"datagram is "+strconv.Itoa(len(data))+" bytes, the limit leaves "+
				strconv.Itoa(limits.MaxPacketSize-MinIPv4HeaderSize), nil)
	}
	if len(data) < UDPHeaderSize {
		return nil, apperr.InvalidArgument(op,
			"datagram is "+strconv.Itoa(len(data))+" bytes, a UDP header needs "+strconv.Itoa(UDPHeaderSize), nil)
	}

	length := int(binary.BigEndian.Uint16(data[4:6]))
	if length < UDPHeaderSize {
		return nil, apperr.InvalidArgument(op,
			"length field is "+strconv.Itoa(length)+", the minimum is "+strconv.Itoa(UDPHeaderSize), nil)
	}
	if length > len(data) {
		return nil, apperr.InvalidArgument(op,
			"length field is "+strconv.Itoa(length)+" bytes but only "+strconv.Itoa(len(data))+" bytes are present", nil)
	}

	datagram := &UDP{
		SourcePort:      binary.BigEndian.Uint16(data[0:2]),
		DestinationPort: binary.BigEndian.Uint16(data[2:4]),
		Checksum:        binary.BigEndian.Uint16(data[6:8]),
		Payload:         data[UDPHeaderSize:length],
	}
	if datagram.Checksum == 0 {
		datagram.ChecksumOmitted = true
	}
	return datagram, nil
}

func (d *UDP) Validate(source, destination netip.Addr) error {
	const op = "packet.UDP.Validate"
	if d.ChecksumOmitted {
		return nil
	}
	datagram, err := d.SerializeRaw()
	if err != nil {
		return err
	}
	putChecksum(datagram[UDPChecksumOffset:UDPChecksumOffset+2], d.Checksum)
	if !TransportValid(source, destination, ProtocolUDP, datagram) {
		return apperr.InvalidArgument(op,
			"checksum "+hex16(d.Checksum)+" does not verify for "+source.String()+" to "+destination.String(), nil)
	}
	return nil
}

func (d *UDP) Serialize(source, destination netip.Addr) ([]byte, error) {
	datagram, err := d.SerializeRaw()
	if err != nil {
		return nil, err
	}
	putChecksum(datagram[UDPChecksumOffset:UDPChecksumOffset+2], 0)
	d.Checksum = TransportSum(source, destination, ProtocolUDP, datagram)
	putChecksum(datagram[UDPChecksumOffset:UDPChecksumOffset+2], d.Checksum)
	d.ChecksumOmitted = false
	return datagram, nil
}

func (d *UDP) SerializeNoSum() ([]byte, error) {
	datagram, err := d.SerializeRaw()
	if err != nil {
		return nil, err
	}
	putChecksum(datagram[UDPChecksumOffset:UDPChecksumOffset+2], 0)
	d.Checksum = 0
	d.ChecksumOmitted = true
	return datagram, nil
}

func (d *UDP) SerializeRaw() ([]byte, error) {
	const op = "packet.UDP.Serialize"
	if len(d.Payload) > MaxUDPPayloadSize {
		return nil, apperr.LimitExceeded(op,
			"payload is "+strconv.Itoa(len(d.Payload))+" bytes, the maximum is "+strconv.Itoa(MaxUDPPayloadSize), nil)
	}

	length := UDPHeaderSize + len(d.Payload)
	if length > MaxIPv4TotalLength-MinIPv4HeaderSize {
		return nil, apperr.LimitExceeded(op,
			"datagram would be "+strconv.Itoa(length)+" bytes, it does not fit in an IPv4 packet", nil)
	}

	datagram := make([]byte, length)
	binary.BigEndian.PutUint16(datagram[0:2], d.SourcePort)
	binary.BigEndian.PutUint16(datagram[2:4], d.DestinationPort)
	binary.BigEndian.PutUint16(datagram[4:6], uint16(length))
	putChecksum(datagram[UDPChecksumOffset:UDPChecksumOffset+2], 0)
	copy(datagram[UDPHeaderSize:], d.Payload)
	return datagram, nil
}
