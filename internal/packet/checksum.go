package packet

import (
	"encoding/binary"
	"net/netip"
	"strconv"

	"github.com/codingseeker/netforge/internal/apperr"
)

func Checksum(data []byte) uint16 {
	return ^foldSum(rawSum(data))
}

func ChecksumValid(data []byte) bool {
	return foldSum(rawSum(data)) == 0xffff
}

func PseudoSum(source, destination netip.Addr, protocol Protocol, length int) uint16 {
	return ^foldSum(pseudoSum(source, destination, protocol, length))
}

func WriteAddr(dst []byte, address netip.Addr) error {
	if len(dst) < 4 {
		return apperr.InvalidArgument("packet.WriteAddr",
			"destination buffer holds "+strconv.Itoa(len(dst))+" bytes, need 4", nil)
	}
	if !address.Is4() {
		return apperr.InvalidArgument("packet.WriteAddr", "address "+address.String()+" is not IPv4", nil)
	}
	value := address.As4()
	copy(dst[:4], value[:])
	return nil
}

func TransportSum(source, destination netip.Addr, protocol Protocol, segment []byte) uint16 {
	return checksumField(rawSum(segment) + pseudoSum(source, destination, protocol, len(segment)))
}

func TransportValid(source, destination netip.Addr, protocol Protocol, segment []byte) bool {
	if protocol == ProtocolUDP && isZeroChecksum(segment, UDPChecksumOffset) {
		return true
	}
	sum := rawSum(segment) + pseudoSum(source, destination, protocol, len(segment))
	return foldSum(sum) == 0xffff
}

func pseudoSum(source, destination netip.Addr, protocol Protocol, length int) uint32 {
	var sum uint32
	var address [4]byte
	_ = WriteAddr(address[0:4], source)
	sum += rawSum(address[:])
	_ = WriteAddr(address[0:4], destination)
	sum += rawSum(address[:])
	sum += uint32(protocol)
	sum += uint32(uint16(length))
	return sum
}

func checksumField(sum uint32) uint16 {
	value := ^foldSum(sum)
	if value == 0 {
		return 0xffff
	}
	return value
}

func updateSum16(current, old, replacement uint16) uint16 {
	sum := uint32(^current)
	sum += uint32(^old) + uint32(^replacement)
	sum = (sum & 0xffff) + (sum >> 16)
	sum = (sum & 0xffff) + (sum >> 16)
	return checksumField(sum)
}

func isZeroChecksum(segment []byte, offset int) bool {
	if len(segment) < offset+2 {
		return false
	}
	return segment[offset] == 0 && segment[offset+1] == 0
}

func rawSum(data []byte) uint32 {
	var sum uint32
	length := len(data)
	for i := 0; i+1 < length; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(data[i : i+2]))
	}
	if length%2 == 1 {
		sum += uint32(data[length-1]) << 8
	}
	return sum
}

func foldSum(sum uint32) uint16 {
	for sum > 0xffff {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return uint16(sum)
}

func putChecksum(dst []byte, value uint16) {
	binary.BigEndian.PutUint16(dst, value)
}
