package packet

import (
	"encoding/binary"
	"net/netip"
	"strconv"
	"strings"

	"github.com/netforge/netforge/internal/apperr"
)

type IPv4FragmentField uint16

type IPv4 struct {
	TypeOfService     uint8
	Identification    uint16
	Fragment          IPv4FragmentField
	TimeToLive        uint8
	Protocol          Protocol
	HeaderChecksum    uint16
	Source            netip.Addr
	Destination       netip.Addr
	Options           []byte
	Payload           []byte
	Padding           []byte
	ChecksumUnchecked bool
}

func ParseIPv4(limits Limits, data []byte) (*IPv4, error) {
	const op = "packet.ParseIPv4"
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if len(data) < MinIPv4HeaderSize {
		return nil, apperr.InvalidArgument(op,
			"packet is "+strconv.Itoa(len(data))+" bytes, an IPv4 header needs "+strconv.Itoa(MinIPv4HeaderSize), nil)
	}
	if len(data) > limits.MaxPacketSize {
		return nil, apperr.LimitExceeded(op,
			"packet is "+strconv.Itoa(len(data))+" bytes, the limit is "+strconv.Itoa(limits.MaxPacketSize), nil)
	}
	if data[0]>>4 != 4 {
		return nil, apperr.InvalidArgument(op,
			"version field is "+strconv.Itoa(int(data[0]>>4))+", only IPv4 is supported", nil)
	}
	if data[0]&0x0f < 5 {
		return nil, apperr.InvalidArgument(op,
			"header length is "+strconv.Itoa(int(data[0]&0x0f))+" words, the minimum is 5", nil)
	}

	headerLength := int(data[0]&0x0f) * 4
	if headerLength > MaxIPv4HeaderSize {
		return nil, apperr.InvalidArgument(op,
			"header length is "+strconv.Itoa(headerLength)+" bytes, the maximum is "+strconv.Itoa(MaxIPv4HeaderSize), nil)
	}
	if headerLength > len(data) {
		return nil, apperr.InvalidArgument(op,
			"header length is "+strconv.Itoa(headerLength)+" bytes but only "+strconv.Itoa(len(data))+" bytes are present", nil)
	}

	totalLength := int(binary.BigEndian.Uint16(data[2:4]))
	if totalLength < headerLength {
		return nil, apperr.InvalidArgument(op,
			"total length is "+strconv.Itoa(totalLength)+" bytes, smaller than the "+strconv.Itoa(headerLength)+" byte header", nil)
	}
	if totalLength > MaxIPv4TotalLength {
		return nil, apperr.LimitExceeded(op,
			"total length is "+strconv.Itoa(totalLength)+" bytes, the protocol maximum is "+strconv.Itoa(MaxIPv4TotalLength), nil)
	}
	if totalLength > len(data) {
		return nil, apperr.InvalidArgument(op,
			"total length is "+strconv.Itoa(totalLength)+" bytes but only "+strconv.Itoa(len(data))+" bytes are present", nil)
	}
	if totalLength > limits.MaxPacketSize {
		return nil, apperr.LimitExceeded(op,
			"total length is "+strconv.Itoa(totalLength)+" bytes, the limit is "+strconv.Itoa(limits.MaxPacketSize), nil)
	}

	packet := &IPv4{
		TypeOfService:  data[1],
		Identification: binary.BigEndian.Uint16(data[4:6]),
		Fragment:       IPv4FragmentField(binary.BigEndian.Uint16(data[6:8])),
		TimeToLive:     data[8],
		Protocol:       Protocol(data[9]),
		HeaderChecksum: binary.BigEndian.Uint16(data[10:12]),
		Source:         netip.AddrFrom4([4]byte(data[12:16])),
		Destination:    netip.AddrFrom4([4]byte(data[16:20])),
	}
	if headerLength > MinIPv4HeaderSize {
		packet.Options = data[MinIPv4HeaderSize:headerLength]
	}
	packet.Payload = data[headerLength:totalLength]
	if totalLength < len(data) {
		packet.Padding = data[totalLength:]
	}

	if limits.VerifyChecksums && !packet.ChecksumValid() {
		return nil, apperr.InvalidArgument(op,
			"header checksum is "+hex16(packet.HeaderChecksum)+", it does not verify", nil)
	}
	packet.ChecksumUnchecked = !limits.VerifyChecksums
	return packet, nil
}

func (p *IPv4) HeaderLength() int {
	return MinIPv4HeaderSize + len(p.Options)
}

func (p *IPv4) TotalLength() int {
	return p.HeaderLength() + len(p.Payload)
}

func (p *IPv4) DontFragment() bool {
	return p.Fragment&0x4000 != 0
}

func (p *IPv4) MoreFragments() bool {
	return p.Fragment&0x2000 != 0
}

func (p *IPv4) FragmentOffset() int {
	return int(p.Fragment&0x1fff) * 8
}

func (p *IPv4) IsFragment() bool {
	return p.FragmentOffset() != 0
}

func (p *IPv4) ChecksumValid() bool {
	header, err := p.headerBytes()
	if err != nil {
		return false
	}
	return ChecksumValid(header)
}

func (p *IPv4) SetTimeToLive(ttl uint8) {
	p.HeaderChecksum = updateSum16(p.HeaderChecksum, uint16(p.TimeToLive), uint16(ttl))
	p.TimeToLive = ttl
}

func (p *IPv4) Serialize(limits Limits) ([]byte, error) {
	const op = "packet.IPv4.Serialize"
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if len(p.Options)%4 != 0 {
		return nil, apperr.InvalidArgument(op,
			"options are "+strconv.Itoa(len(p.Options))+" bytes, which is not a multiple of 4", nil)
	}
	headerLength := MinIPv4HeaderSize + len(p.Options)
	if headerLength > MaxIPv4HeaderSize {
		return nil, apperr.InvalidArgument(op,
			"header would be "+strconv.Itoa(headerLength)+" bytes, the maximum is "+strconv.Itoa(MaxIPv4HeaderSize), nil)
	}
	if !p.Source.Is4() || !p.Destination.Is4() {
		return nil, apperr.InvalidArgument(op,
			"source "+p.Source.String()+" and destination "+p.Destination.String()+" must both be IPv4", nil)
	}

	totalLength := headerLength + len(p.Payload)
	if totalLength < MinIPv4TotalLength || totalLength > MaxIPv4TotalLength {
		return nil, apperr.LimitExceeded(op,
			"total length would be "+strconv.Itoa(totalLength)+" bytes, the protocol range is "+
				strconv.Itoa(MinIPv4TotalLength)+" to "+strconv.Itoa(MaxIPv4TotalLength), nil)
	}
	if totalLength > limits.MaxPacketSize {
		return nil, apperr.LimitExceeded(op,
			"total length would be "+strconv.Itoa(totalLength)+" bytes, the limit is "+strconv.Itoa(limits.MaxPacketSize), nil)
	}
	if len(p.Padding) > MaxPaddingSize {
		return nil, apperr.LimitExceeded(op,
			"padding is "+strconv.Itoa(len(p.Padding))+" bytes, the limit is "+strconv.Itoa(MaxPaddingSize), nil)
	}

	packet := make([]byte, totalLength+len(p.Padding))
	header, err := p.headerBytes()
	if err != nil {
		return nil, err
	}
	copy(packet, header)
	copy(packet[headerLength:totalLength], p.Payload)
	copy(packet[totalLength:], p.Padding)
	putChecksum(packet[10:12], 0)
	p.HeaderChecksum = checksumField(rawSum(packet[:headerLength]))
	putChecksum(packet[10:12], p.HeaderChecksum)
	return packet, nil
}

func (p *IPv4) headerBytes() ([]byte, error) {
	const op = "packet.IPv4.header"
	if len(p.Options) > MaxIPv4HeaderSize-MinIPv4HeaderSize {
		return nil, apperr.LimitExceeded(op,
			"options are "+strconv.Itoa(len(p.Options))+" bytes, a header leaves "+
				strconv.Itoa(MaxIPv4HeaderSize-MinIPv4HeaderSize), nil)
	}
	header := make([]byte, MinIPv4HeaderSize+len(p.Options))
	header[0] = 4<<4 | byte((MinIPv4HeaderSize+len(p.Options))/4)
	header[1] = p.TypeOfService
	binary.BigEndian.PutUint16(header[2:4], uint16(MinIPv4HeaderSize+len(p.Options)+len(p.Payload)))
	binary.BigEndian.PutUint16(header[4:6], p.Identification)
	binary.BigEndian.PutUint16(header[6:8], uint16(p.Fragment))
	header[8] = p.TimeToLive
	header[9] = byte(p.Protocol)
	putChecksum(header[10:12], p.HeaderChecksum)
	copy(header[12:16], p.Source.AsSlice())
	copy(header[16:20], p.Destination.AsSlice())
	copy(header[MinIPv4HeaderSize:], p.Options)
	return header, nil
}

func hex16(value uint16) string {
	digits := strconv.FormatUint(uint64(value), 16)
	return "0x" + strings.Repeat("0", 4-len(digits)) + digits
}
