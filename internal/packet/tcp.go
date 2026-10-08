package packet

import (
	"encoding/binary"
	"net/netip"
	"strconv"

	"github.com/codingseeker/netforge/internal/apperr"
)

const TCPChecksumOffset = 16

type TCPFlag uint8

const (
	TCPFlagFIN TCPFlag = 0x01
	TCPFlagSYN TCPFlag = 0x02
	TCPFlagRST TCPFlag = 0x04
	TCPFlagPSH TCPFlag = 0x08
	TCPFlagACK TCPFlag = 0x10
	TCPFlagURG TCPFlag = 0x20
	TCPFlagECE TCPFlag = 0x40
	TCPFlagCWR TCPFlag = 0x80
)

type TCP struct {
	SourcePort      uint16
	DestinationPort uint16
	SequenceNumber  uint32
	Acknowledgment  uint32

	DataOffsetReserved uint8

	Flags         uint8
	Window        uint16
	Checksum      uint16
	UrgentPointer uint16
	Options       []byte
	Payload       []byte

	ChecksumUnchecked bool
}

func (t *TCP) Has(flag TCPFlag) bool {
	return t.Flags&byte(flag) != 0
}

func (t *TCP) Set(flag TCPFlag, on bool) {
	if on {
		t.Flags |= byte(flag)
		return
	}
	t.Flags &^= byte(flag)
}

func (t *TCP) HeaderLength() int {
	return MinTCPHeaderSize + len(t.Options)
}

func ParseTCP(limits Limits, data []byte) (*TCP, error) {
	const op = "packet.ParseTCP"
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if len(data) > limits.MaxPacketSize-MinIPv4HeaderSize {
		return nil, apperr.LimitExceeded(op,
			"segment is "+strconv.Itoa(len(data))+" bytes, the limit leaves "+
				strconv.Itoa(limits.MaxPacketSize-MinIPv4HeaderSize), nil)
	}
	if len(data) < MinTCPHeaderSize {
		return nil, apperr.InvalidArgument(op,
			"segment is "+strconv.Itoa(len(data))+" bytes, a TCP header needs "+strconv.Itoa(MinTCPHeaderSize), nil)
	}

	offset := int(data[12]>>4) * 4
	if offset < MinTCPHeaderSize {
		return nil, apperr.InvalidArgument(op,
			"data offset is "+strconv.Itoa(offset)+" bytes, the minimum is "+strconv.Itoa(MinTCPHeaderSize), nil)
	}
	if offset > MaxTCPHeaderSize {
		return nil, apperr.InvalidArgument(op,
			"data offset is "+strconv.Itoa(offset)+" bytes, the maximum is "+strconv.Itoa(MaxTCPHeaderSize), nil)
	}
	if offset > len(data) {
		return nil, apperr.InvalidArgument(op,
			"data offset is "+strconv.Itoa(offset)+" bytes but only "+strconv.Itoa(len(data))+" bytes are present", nil)
	}

	segment := &TCP{
		SourcePort:         binary.BigEndian.Uint16(data[0:2]),
		DestinationPort:    binary.BigEndian.Uint16(data[2:4]),
		SequenceNumber:     binary.BigEndian.Uint32(data[4:8]),
		Acknowledgment:     binary.BigEndian.Uint32(data[8:12]),
		DataOffsetReserved: data[12] & 0x0f,
		Flags:              data[13],
		Window:             binary.BigEndian.Uint16(data[14:16]),
		Checksum:           binary.BigEndian.Uint16(data[16:18]),
		UrgentPointer:      binary.BigEndian.Uint16(data[18:20]),
		Payload:            data[offset:],
	}
	if offset > MinTCPHeaderSize {
		segment.Options = data[MinTCPHeaderSize:offset]
	}
	segment.ChecksumUnchecked = !limits.VerifyChecksums
	return segment, nil
}

func (t *TCP) Validate(source, destination netip.Addr) error {
	segment, err := t.SerializeRaw()
	if err != nil {
		return err
	}
	putChecksum(segment[TCPChecksumOffset:TCPChecksumOffset+2], t.Checksum)
	if !TransportValid(source, destination, ProtocolTCP, segment) {
		return apperr.InvalidArgument("packet.TCP.Validate",
			"checksum "+hex16(t.Checksum)+" does not verify for "+source.String()+" to "+destination.String(), nil)
	}
	return nil
}

func (t *TCP) Serialize(source, destination netip.Addr) ([]byte, error) {
	segment, err := t.SerializeRaw()
	if err != nil {
		return nil, err
	}
	putChecksum(segment[TCPChecksumOffset:TCPChecksumOffset+2], 0)
	t.Checksum = TransportSum(source, destination, ProtocolTCP, segment)
	putChecksum(segment[TCPChecksumOffset:TCPChecksumOffset+2], t.Checksum)
	return segment, nil
}

func (t *TCP) SerializeRaw() ([]byte, error) {
	const op = "packet.TCP.Serialize"
	if len(t.Options)%4 != 0 {
		return nil, apperr.InvalidArgument(op,
			"options are "+strconv.Itoa(len(t.Options))+" bytes, which is not a multiple of 4", nil)
	}
	headerLength := MinTCPHeaderSize + len(t.Options)
	if headerLength > MaxTCPHeaderSize {
		return nil, apperr.InvalidArgument(op,
			"header would be "+strconv.Itoa(headerLength)+" bytes, the maximum is "+strconv.Itoa(MaxTCPHeaderSize), nil)
	}
	length := headerLength + len(t.Payload)
	if length > MaxIPv4TotalLength-MinIPv4HeaderSize {
		return nil, apperr.LimitExceeded(op,
			"segment would be "+strconv.Itoa(length)+" bytes, it does not fit in an IPv4 packet", nil)
	}

	segment := make([]byte, length)
	binary.BigEndian.PutUint16(segment[0:2], t.SourcePort)
	binary.BigEndian.PutUint16(segment[2:4], t.DestinationPort)
	binary.BigEndian.PutUint32(segment[4:8], t.SequenceNumber)
	binary.BigEndian.PutUint32(segment[8:12], t.Acknowledgment)
	segment[12] = byte(headerLength/4)<<4 | t.DataOffsetReserved&0x0f
	segment[13] = t.Flags
	binary.BigEndian.PutUint16(segment[14:16], t.Window)
	putChecksum(segment[TCPChecksumOffset:TCPChecksumOffset+2], 0)
	binary.BigEndian.PutUint16(segment[18:20], t.UrgentPointer)
	copy(segment[MinTCPHeaderSize:headerLength], t.Options)
	copy(segment[headerLength:], t.Payload)
	return segment, nil
}
