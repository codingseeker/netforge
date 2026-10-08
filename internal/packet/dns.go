package packet

import (
	"encoding/binary"
	"net/netip"
	"strconv"
	"strings"

	"github.com/codingseeker/netforge/internal/apperr"
)

const (
	DNSHeaderSize = 12

	MaxDNSNameLength = 255

	MaxDNSLabelLength = 63

	MaxDNSQuestionName = 128

	MaxDNSMessageSize = 512

	DNSTypeA uint16 = 1

	DNSClassIN uint16 = 1
)

const (
	dnsResponse  uint16 = 1 << 15
	dnsRecursion uint16 = 1 << 8

	DNSRCodeSuccess uint16 = 0

	DNSRCodeFail uint16 = 2

	DNSRCodeNotFound uint16 = 3

	DNSRCodeRefused uint16 = 5
)

type DNSQuestion struct {
	Name  string
	Type  uint16
	Class uint16
}

type DNSRecord struct {
	Name string
	Type uint16
	TTL  uint32

	Address netip.Addr
}

type DNSMessage struct {
	ID        uint16
	Response  bool
	Recursion bool
	RCode     uint16
	Questions []DNSQuestion
	Answers   []DNSRecord

	Truncated bool
}

func rcodeName(code uint16) string {
	switch code {
	case DNSRCodeSuccess:
		return "noerror"
	case DNSRCodeFail:
		return "servfail"
	case DNSRCodeNotFound:
		return "nxdomain"
	case DNSRCodeRefused:
		return "refused"
	default:
		return "rcode" + strconv.Itoa(int(code))
	}
}

func RCodeName(code uint16) string { return rcodeName(code) }

func ParseDNS(data []byte) (*DNSMessage, error) {
	const op = "packet.ParseDNS"
	if len(data) < DNSHeaderSize {
		return nil, apperr.InvalidArgument(op, "message is "+strconv.Itoa(len(data))+
			" bytes, a DNS header needs "+strconv.Itoa(DNSHeaderSize), nil)
	}
	message := &DNSMessage{
		ID:        binary.BigEndian.Uint16(data[0:2]),
		Recursion: binary.BigEndian.Uint16(data[2:4])&dnsRecursion != 0,
	}
	flags := binary.BigEndian.Uint16(data[2:4])
	if flags&dnsResponse != 0 {
		message.Response = true
		message.RCode = flags & 0x000f
	}
	counts := [4]int{
		int(binary.BigEndian.Uint16(data[4:6])),
		int(binary.BigEndian.Uint16(data[6:8])),
		int(binary.BigEndian.Uint16(data[8:10])),
		int(binary.BigEndian.Uint16(data[10:12])),
	}

	remaining := len(data) - DNSHeaderSize
	for i, count := range counts {
		if count > remaining {
			return nil, apperr.InvalidArgument(op, "the message claims "+strconv.Itoa(count)+
				" records in section "+strconv.Itoa(i)+" but only "+strconv.Itoa(remaining)+
				" bytes are present", nil)
		}
	}

	offset := DNSHeaderSize
	for i := 0; i < counts[0]; i++ {
		name, next, err := parseDNSName(data, offset, 0)
		if err != nil {
			return nil, err
		}
		if next+4 > len(data) {
			return nil, apperr.InvalidArgument(op, "the question is cut off", nil)
		}
		message.Questions = append(message.Questions, DNSQuestion{
			Name:  name,
			Type:  binary.BigEndian.Uint16(data[next : next+2]),
			Class: binary.BigEndian.Uint16(data[next+2 : next+4]),
		})
		offset = next + 4
	}
	for i := 0; i < counts[1]; i++ {
		name, next, err := parseDNSName(data, offset, 0)
		if err != nil {
			return nil, err
		}
		if next+10 > len(data) {
			return nil, apperr.InvalidArgument(op, "the answer record is cut off", nil)
		}
		record := DNSRecord{
			Name: name,
			Type: binary.BigEndian.Uint16(data[next : next+2]),
			TTL:  binary.BigEndian.Uint32(data[next+4 : next+8]),
		}
		rdLength := int(binary.BigEndian.Uint16(data[next+8 : next+10]))
		rdStart := next + 10
		if rdStart+rdLength > len(data) {
			return nil, apperr.InvalidArgument(op, "the answer data is cut off", nil)
		}
		if record.Type == DNSTypeA && rdLength == 4 {
			var octets [4]byte
			copy(octets[:], data[rdStart:rdStart+4])
			record.Address = netip.AddrFrom4(octets)
		}
		message.Answers = append(message.Answers, record)
		offset = rdStart + rdLength
	}
	return message, nil
}

func parseDNSName(data []byte, offset, depth int) (string, int, error) {
	const op = "packet.parseDNSName"
	if depth > 1 {
		return "", 0, apperr.InvalidArgument(op, "the name points at another name", nil)
	}
	var labels []string
	next := -1
	for {
		if offset >= len(data) {
			return "", 0, apperr.InvalidArgument(op, "the name runs past the end of the message", nil)
		}
		length := int(data[offset])
		switch {
		case length == 0:
			offset++
			if next < 0 {
				next = offset
			}
			return strings.Join(labels, "."), next, nil
		case length&0xc0 == 0xc0:
			if offset+1 >= len(data) {
				return "", 0, apperr.InvalidArgument(op, "the name pointer is cut off", nil)
			}
			pointer := int(binary.BigEndian.Uint16(data[offset:offset+2]) & 0x3fff)
			if pointer >= len(data) {
				return "", 0, apperr.InvalidArgument(op, "the name pointer points outside the message", nil)
			}
			if next < 0 {
				next = offset + 2
			}
			suffix, _, err := parseDNSName(data, pointer, depth+1)
			if err != nil {
				return "", 0, err
			}
			if suffix != "" {
				labels = append(labels, suffix)
			}
			return strings.Join(labels, "."), next, nil
		case length > MaxDNSLabelLength:
			return "", 0, apperr.InvalidArgument(op, "a label of "+strconv.Itoa(length)+
				" bytes is longer than "+strconv.Itoa(MaxDNSLabelLength), nil)
		default:
			if offset+1+length > len(data) {
				return "", 0, apperr.InvalidArgument(op, "a label runs past the end of the message", nil)
			}
			labels = append(labels, string(data[offset+1:offset+1+length]))
			offset += 1 + length
			if len(labels) > MaxDNSQuestionName/MaxDNSLabelLength+1 {
				return "", 0, apperr.InvalidArgument(op, "the name has too many labels", nil)
			}
		}
	}
}

func MarshalQuery(id uint16, name string) ([]byte, error) {
	const op = "packet.MarshalQuery"
	labels, err := dnsLabels(op, name)
	if err != nil {
		return nil, err
	}
	total := DNSHeaderSize + len(labels) + 4
	if total > MaxDNSMessageSize {
		return nil, messageTooLarge(op, total)
	}
	message := make([]byte, DNSHeaderSize, total)
	binary.BigEndian.PutUint16(message[0:2], id)
	binary.BigEndian.PutUint16(message[2:4], dnsRecursion)
	binary.BigEndian.PutUint16(message[4:6], 1)
	message = append(message, labels...)
	tail := make([]byte, 4)
	binary.BigEndian.PutUint16(tail[0:2], DNSTypeA)
	binary.BigEndian.PutUint16(tail[2:4], DNSClassIN)
	return append(message, tail...), nil
}

func MarshalResponse(request []byte, address netip.Addr, ttl uint32) ([]byte, error) {
	const op = "packet.MarshalResponse"
	question, err := ParseDNS(request)
	if err != nil {
		return nil, err
	}
	if len(question.Questions) == 0 {

		return nil, apperr.InvalidArgument(op, "the message to answer carries no question", nil)
	}
	labels, err := dnsLabels(op, firstOr(question.Questions, DNSQuestion{}).Name)
	if err != nil {
		return nil, err
	}
	answers := 0
	if address.IsValid() {
		answers = 1
	}
	total := DNSHeaderSize + len(labels) + 4 + answers*16
	if total > MaxDNSMessageSize {
		return nil, messageTooLarge(op, total)
	}
	message := make([]byte, DNSHeaderSize, total)
	binary.BigEndian.PutUint16(message[0:2], question.ID)
	binary.BigEndian.PutUint16(message[2:4], dnsResponse|dnsRecursion|DNSRCodeSuccess)
	binary.BigEndian.PutUint16(message[4:6], uint16(len(question.Questions)))
	binary.BigEndian.PutUint16(message[6:8], uint16(answers))
	message = append(message, labels...)
	tail := make([]byte, 4)
	binary.BigEndian.PutUint16(tail[0:2], DNSTypeA)
	binary.BigEndian.PutUint16(tail[2:4], DNSClassIN)
	message = append(message, tail...)

	if answers == 1 {
		record := make([]byte, 0, 16)

		pointer := make([]byte, 2)
		binary.BigEndian.PutUint16(pointer, 0xc00c)
		record = append(record, pointer...)
		fields := make([]byte, 10)
		binary.BigEndian.PutUint16(fields[0:2], DNSTypeA)
		binary.BigEndian.PutUint16(fields[2:4], DNSClassIN)
		binary.BigEndian.PutUint32(fields[4:8], ttl)
		binary.BigEndian.PutUint16(fields[8:10], 4)
		record = append(record, fields...)
		octets := address.As4()
		record = append(record, octets[:]...)
		message = append(message, record...)
	}
	return message, nil
}

func MarshalRefusal(request []byte) ([]byte, error) {
	const op = "packet.MarshalRefusal"
	question, err := ParseDNS(request)
	if err != nil {
		return nil, err
	}
	if len(question.Questions) == 0 {
		return nil, apperr.InvalidArgument(op, "the message to refuse carries no question", nil)
	}
	labels, err := dnsLabels(op, firstOr(question.Questions, DNSQuestion{}).Name)
	if err != nil {
		return nil, err
	}
	total := DNSHeaderSize + len(labels) + 4
	if total > MaxDNSMessageSize {
		return nil, messageTooLarge(op, total)
	}
	message := make([]byte, DNSHeaderSize, total)
	binary.BigEndian.PutUint16(message[0:2], question.ID)
	binary.BigEndian.PutUint16(message[2:4], dnsResponse|dnsRecursion|DNSRCodeRefused)
	binary.BigEndian.PutUint16(message[4:6], uint16(len(question.Questions)))
	message = append(message, labels...)
	tail := make([]byte, 4)
	binary.BigEndian.PutUint16(tail[0:2], DNSTypeA)
	binary.BigEndian.PutUint16(tail[2:4], DNSClassIN)
	return append(message, tail...), nil
}

func messageTooLarge(op string, total int) error {
	return apperr.LimitExceeded(op, strconv.Itoa(total)+
		" bytes, the maximum message is "+strconv.Itoa(MaxDNSMessageSize), nil)
}

func dnsLabels(op, name string) ([]byte, error) {
	name = strings.TrimSuffix(strings.TrimSpace(name), ".")
	if name == "" {
		return []byte{0}, nil
	}
	if len(name) > MaxDNSQuestionName {
		return nil, apperr.LimitExceeded(op, "the name is "+strconv.Itoa(len(name))+
			" bytes, the maximum is "+strconv.Itoa(MaxDNSQuestionName), nil)
	}
	encoded := make([]byte, 0, len(name)+2)
	for _, label := range strings.Split(name, ".") {
		if label == "" {
			return nil, apperr.InvalidArgument(op, "the name "+name+" has an empty label", nil)
		}
		if len(label) > MaxDNSLabelLength {
			return nil, apperr.InvalidArgument(op, "the label "+label+" is longer than "+
				strconv.Itoa(MaxDNSLabelLength)+" bytes", nil)
		}
		encoded = append(encoded, byte(len(label)))
		encoded = append(encoded, label...)
	}
	return append(encoded, 0), nil
}

func firstOr(questions []DNSQuestion, fallback DNSQuestion) DNSQuestion {
	if len(questions) == 0 {
		return fallback
	}
	return questions[0]
}
