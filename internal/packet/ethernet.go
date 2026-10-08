package packet

import (
	"encoding/binary"
	"net"
	"strconv"

	"github.com/codingseeker/netforge/internal/apperr"
)

type Ethernet struct {
	Destination net.HardwareAddr
	Source      net.HardwareAddr
	EtherType   EtherType
	Payload     []byte
}

func ParseEthernet(limits Limits, frame []byte) (*Ethernet, error) {
	const op = "packet.ParseEthernet"
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if len(frame) < EthernetHeaderSize {
		return nil, apperr.InvalidArgument(op,
			"frame is "+strconv.Itoa(len(frame))+" bytes, an Ethernet header needs "+strconv.Itoa(EthernetHeaderSize), nil)
	}
	if len(frame) > limits.MaxFrameSize {
		return nil, apperr.LimitExceeded(op,
			"frame is "+strconv.Itoa(len(frame))+" bytes, the limit is "+strconv.Itoa(limits.MaxFrameSize), nil)
	}

	result := &Ethernet{
		Destination: net.HardwareAddr(frame[0:6]),
		Source:      net.HardwareAddr(frame[6:12]),
		EtherType:   EtherType(binary.BigEndian.Uint16(frame[12:14])),
		Payload:     frame[EthernetHeaderSize:],
	}
	if err := result.validate(op); err != nil {
		return nil, err
	}
	return result, nil
}

func (e *Ethernet) validate(op string) error {
	if len(e.Source) != EthernetAddressSize || len(e.Destination) != EthernetAddressSize {
		return apperr.InvalidArgument(op,
			"hardware addresses must be "+strconv.Itoa(EthernetAddressSize)+" bytes", nil)
	}
	if isMulticast(e.Source) {
		return apperr.InvalidArgument(op,
			"source address "+e.Source.String()+" is a group address, an Ethernet source must be unicast", nil)
	}
	return nil
}

func (e *Ethernet) IsBroadcast() bool {
	return e.Destination.String() == "ff:ff:ff:ff:ff:ff"
}

func (e *Ethernet) IsMulticast() bool {
	return isMulticast(e.Destination)
}

func (e *Ethernet) Serialize(limits Limits) ([]byte, error) {
	const op = "packet.Ethernet.Serialize"
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if err := e.validate(op); err != nil {
		return nil, err
	}
	if len(e.Payload) > limits.MaxFrameSize-EthernetHeaderSize {
		return nil, apperr.LimitExceeded(op,
			"payload is "+strconv.Itoa(len(e.Payload))+" bytes, the frame limit leaves "+
				strconv.Itoa(limits.MaxFrameSize-EthernetHeaderSize), nil)
	}

	frame := make([]byte, EthernetHeaderSize+len(e.Payload))
	copy(frame[0:6], e.Destination)
	copy(frame[6:12], e.Source)
	binary.BigEndian.PutUint16(frame[12:14], uint16(e.EtherType))
	copy(frame[EthernetHeaderSize:], e.Payload)
	return frame, nil
}

func isMulticast(address net.HardwareAddr) bool {
	return len(address) > 0 && address[0]&0x01 == 0x01
}
