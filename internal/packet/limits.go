package packet

import (
	"strconv"

	"github.com/netforge/netforge/internal/apperr"
)

const (
	EthernetHeaderSize = 14

	EthernetAddressSize = 6

	MinIPv4HeaderSize = 20

	MaxIPv4HeaderSize = 60

	MaxIPv4TotalLength = 65535

	MinIPv4TotalLength = MinIPv4HeaderSize

	MinICMPHeaderSize = 4

	ICMPErrorHeaderSize = 8

	ICMPEchoHeaderSize = 8

	MaxICMPLength = MaxIPv4TotalLength - MinIPv4HeaderSize

	MaxICMPPayloadSize = MaxICMPLength - ICMPEchoHeaderSize

	UDPHeaderSize = 8

	MaxUDPPayloadSize = MaxIPv4TotalLength - MinIPv4HeaderSize - UDPHeaderSize

	MinTCPHeaderSize = 20

	MaxTCPHeaderSize = 60

	MaxFrameSize = EthernetHeaderSize + MaxIPv4TotalLength

	MaxPaddingSize = MaxFrameSize - EthernetHeaderSize - MinIPv4TotalLength
)

type EtherType uint16

const (
	EtherTypeIPv4 EtherType = 0x0800
	EtherTypeARP  EtherType = 0x0806
	EtherTypeVLAN EtherType = 0x8100
)

type Protocol uint8

const (
	ProtocolICMP Protocol = 1
	ProtocolTCP  Protocol = 6
	ProtocolUDP  Protocol = 17
)

type Limits struct {
	MaxFrameSize int

	MaxPacketSize int

	VerifyChecksums bool
}

func DefaultLimits() Limits {
	return Limits{
		MaxFrameSize:    MaxFrameSize,
		MaxPacketSize:   MaxIPv4TotalLength,
		VerifyChecksums: true,
	}
}

func (l Limits) Validate() error {
	const op = "packet.Limits.Validate"
	if l.MaxFrameSize <= 0 {
		return apperr.InvalidArgument(op, "max frame size must be positive, got "+strconv.Itoa(l.MaxFrameSize), nil)
	}
	if l.MaxPacketSize <= 0 {
		return apperr.InvalidArgument(op, "max packet size must be positive, got "+strconv.Itoa(l.MaxPacketSize), nil)
	}
	if l.MaxPacketSize > MaxIPv4TotalLength {
		return apperr.InvalidArgument(op,
			"max packet size "+strconv.Itoa(l.MaxPacketSize)+" exceeds the protocol maximum of "+strconv.Itoa(MaxIPv4TotalLength), nil)
	}
	if l.MaxFrameSize > MaxFrameSize {
		return apperr.InvalidArgument(op,
			"max frame size "+strconv.Itoa(l.MaxFrameSize)+" exceeds the protocol maximum of "+strconv.Itoa(MaxFrameSize), nil)
	}
	if l.MaxFrameSize < EthernetHeaderSize+MinIPv4TotalLength {
		return apperr.InvalidArgument(op,
			"max frame size "+strconv.Itoa(l.MaxFrameSize)+" is smaller than an Ethernet header plus a minimal IPv4 packet", nil)
	}
	return nil
}
