package packet

import (
	"encoding/binary"
	"strconv"

	"github.com/netforge/netforge/internal/apperr"
)

type ICMPType uint8

const (
	ICMPTypeEchoReply              ICMPType = 0
	ICMPTypeDestinationUnreachable ICMPType = 3
	ICMPTypeEchoRequest            ICMPType = 8
	ICMPTypeTimeExceeded           ICMPType = 11
)

const (
	ICMPCodeNetworkUnreachable      uint8 = 0
	ICMPCodeHostUnreachable         uint8 = 1
	ICMPCodeProtocolUnreachable     uint8 = 2
	ICMPCodePortUnreachable         uint8 = 3
	ICMPCodeFragmentationNeeded     uint8 = 4
	ICMPCodeSourceRouteFailed       uint8 = 5
	ICMPCodeDestinationNetUnknown   uint8 = 6
	ICMPCodeDestinationHostUnknown  uint8 = 7
	ICMPCodeSourceHostIsolated      uint8 = 8
	ICMPCodeNetworkProhibited       uint8 = 9
	ICMPCodeHostProhibited          uint8 = 10
	ICMPCodeNetworkUnreachableTOS   uint8 = 11
	ICMPCodeHostUnreachableTOS      uint8 = 12
	ICMPCodeCommunicationProhibited uint8 = 13
	ICMPCodeHostPrecedenceViolated  uint8 = 14
	ICMPCodePrecedenceCutoff        uint8 = 15

	ICMPCodeTimeExceededInTransit uint8 = 0

	ICMPCodeTimeExceededReassembly uint8 = 1
)

type ICMP struct {
	Type     ICMPType
	Code     uint8
	Checksum uint16

	Identifier uint16
	Sequence   uint16

	RestOfHeader      [4]byte
	Payload           []byte
	ChecksumUnchecked bool
}

func (m *ICMP) IsEcho() bool {
	return m.Type == ICMPTypeEchoRequest || m.Type == ICMPTypeEchoReply
}

func (m *ICMP) IsError() bool {
	return m.Type == ICMPTypeDestinationUnreachable || m.Type == ICMPTypeTimeExceeded
}

func (t ICMPType) supported() bool {
	switch t {
	case ICMPTypeEchoReply, ICMPTypeEchoRequest, ICMPTypeDestinationUnreachable, ICMPTypeTimeExceeded:
		return true
	default:
		return false
	}
}

func (t ICMPType) headerSize() int {
	if t == ICMPTypeEchoRequest || t == ICMPTypeEchoReply {
		return ICMPEchoHeaderSize
	}
	return ICMPErrorHeaderSize
}

func (m *ICMP) TotalLength() int {
	return m.Type.headerSize() + len(m.Payload)
}

func ParseICMP(limits Limits, data []byte) (*ICMP, error) {
	const op = "packet.ParseICMP"
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if len(data) > limits.MaxPacketSize-MinIPv4HeaderSize {
		return nil, apperr.LimitExceeded(op,
			"message is "+strconv.Itoa(len(data))+" bytes, the limit leaves "+
				strconv.Itoa(limits.MaxPacketSize-MinIPv4HeaderSize), nil)
	}
	if len(data) > MaxICMPLength {
		return nil, apperr.LimitExceeded(op,
			"message is "+strconv.Itoa(len(data))+" bytes, the protocol maximum is "+strconv.Itoa(MaxICMPLength), nil)
	}
	if len(data) < MinICMPHeaderSize {
		return nil, apperr.InvalidArgument(op,
			"message is "+strconv.Itoa(len(data))+" bytes, an ICMP header needs "+strconv.Itoa(MinICMPHeaderSize), nil)
	}

	messageType := ICMPType(data[0])
	if !messageType.supported() {
		return nil, apperr.Unsupported(op,
			"ICMP type "+strconv.Itoa(int(messageType))+" is not handled by this phase", nil)
	}
	minimum := messageType.headerSize()
	if len(data) < minimum {
		return nil, apperr.InvalidArgument(op,
			"type "+strconv.Itoa(int(messageType))+" needs "+strconv.Itoa(minimum)+" bytes, got "+strconv.Itoa(len(data)), nil)
	}

	message := &ICMP{
		Type:     messageType,
		Code:     data[1],
		Checksum: binary.BigEndian.Uint16(data[2:4]),
		Payload:  data[minimum:],
	}
	if message.IsEcho() {
		message.Identifier = binary.BigEndian.Uint16(data[4:6])
		message.Sequence = binary.BigEndian.Uint16(data[6:8])
	} else {
		copy(message.RestOfHeader[:], data[4:8])
	}

	if limits.VerifyChecksums && !message.ChecksumValid() {
		return nil, apperr.InvalidArgument(op,
			"checksum is "+hex16(message.Checksum)+", it does not verify", nil)
	}
	message.ChecksumUnchecked = !limits.VerifyChecksums
	return message, nil
}

func (m *ICMP) ChecksumValid() bool {
	message, err := m.messageBytes()
	if err != nil {
		return false
	}
	return ChecksumValid(message)
}

func (m *ICMP) Serialize() ([]byte, error) {
	const op = "packet.ICMP.Serialize"
	if !m.Type.supported() {
		return nil, apperr.InvalidArgument(op,
			"ICMP type "+strconv.Itoa(int(m.Type))+" is not handled by this phase", nil)
	}
	length := m.Type.headerSize() + len(m.Payload)
	if length > MaxICMPLength {
		return nil, apperr.LimitExceeded(op,
			"message would be "+strconv.Itoa(length)+" bytes, the maximum is "+strconv.Itoa(MaxICMPLength), nil)
	}

	message, err := m.messageBytes()
	if err != nil {
		return nil, err
	}
	putChecksum(message[2:4], 0)
	m.Checksum = checksumField(rawSum(message))
	putChecksum(message[2:4], m.Checksum)
	return message, nil
}

func (m *ICMP) messageBytes() ([]byte, error) {
	const op = "packet.ICMP.message"
	fixed := m.Type.headerSize()
	if fixed+len(m.Payload) > MaxICMPLength {
		return nil, apperr.LimitExceeded(op,
			"message would be "+strconv.Itoa(fixed+len(m.Payload))+" bytes, the maximum is "+
				strconv.Itoa(MaxICMPLength), nil)
	}
	message := make([]byte, fixed+len(m.Payload))
	message[0] = byte(m.Type)
	message[1] = m.Code
	putChecksum(message[2:4], m.Checksum)
	if m.IsEcho() {
		binary.BigEndian.PutUint16(message[4:6], m.Identifier)
		binary.BigEndian.PutUint16(message[6:8], m.Sequence)
	} else {
		copy(message[4:8], m.RestOfHeader[:])
	}
	copy(message[fixed:], m.Payload)
	return message, nil
}
