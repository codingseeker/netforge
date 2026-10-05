package packet

import (
	"encoding/binary"
	"net/netip"
	"strings"
	"testing"
)

func mustQuery(t *testing.T, id uint16, name string) []byte {
	t.Helper()
	query, err := MarshalQuery(id, name)
	if err != nil {
		t.Fatalf("MarshalQuery(%d, %q) = %v, want nil", id, name, err)
	}
	return query
}

func TestAMarshalledQueryRoundTrips(t *testing.T) {
	t.Parallel()
	query := mustQuery(t, 0x1234, "service.lab")
	// 12 byte header, then the labels, the root label and the question fields.
	if len(query) != DNSHeaderSize+len("service")+1+len("lab")+1+1+4 {
		t.Errorf("the query is %d bytes, want a single question for service.lab", len(query))
	}
	if got := binary.BigEndian.Uint16(query[0:2]); got != 0x1234 {
		t.Errorf("the transaction id = %x, want 0x1234", got)
	}
	if got := binary.BigEndian.Uint16(query[4:6]); got != 1 {
		t.Errorf("the query reports %d questions, want 1", got)
	}

	message, err := ParseDNS(query)
	if err != nil {
		t.Fatalf("ParseDNS() = %v, want nil", err)
	}
	if len(message.Questions) != 1 {
		t.Fatalf("Questions = %d, want 1", len(message.Questions))
	}
	question := message.Questions[0]
	if question.Name != "service.lab" {
		t.Errorf("the name = %q, want service.lab", question.Name)
	}
	if question.Type != DNSTypeA {
		t.Errorf("the question type = %d, want an A record", question.Type)
	}
	if message.Response {
		t.Error("a query decoded as a response")
	}
}

func TestAMarshalledQueryIsBounded(t *testing.T) {
	t.Parallel()
	if _, err := MarshalQuery(1, strings.Repeat("a", MaxDNSQuestionName+1)+".lab"); err == nil {
		t.Error("MarshalQuery() with an oversized name = nil error, want a refusal")
	}
	if _, err := MarshalQuery(1, strings.Repeat("a", 200)); err == nil {
		t.Error("MarshalQuery() with a label past the limit = nil error, want a refusal")
	}
}

func TestAResponseRoundTrips(t *testing.T) {
	t.Parallel()
	query := mustQuery(t, 0x1234, "service.lab")
	answer, err := MarshalResponse(query, netip.MustParseAddr("10.60.0.2"), 30)
	if err != nil {
		t.Fatalf("MarshalResponse() = %v, want nil", err)
	}
	message, err := ParseDNS(answer)
	if err != nil {
		t.Fatalf("ParseDNS() = %v, want nil", err)
	}
	if message.ID != 0x1234 {
		t.Errorf("ID = %x, want the 0x1234 that was asked for", message.ID)
	}
	if !message.Response {
		t.Error("an answer decoded as a query")
	}
	if message.RCode != DNSRCodeSuccess {
		t.Errorf("RCode = %s, want noerror", RCodeName(message.RCode))
	}
	if len(message.Answers) != 1 {
		t.Fatalf("Answers = %d, want 1", len(message.Answers))
	}
	record := message.Answers[0]
	if record.Type != DNSTypeA {
		t.Errorf("Type = %d, want an A record", record.Type)
	}
	if record.Address.String() != "10.60.0.2" {
		t.Errorf("Address = %v, want 10.60.0.2", record.Address)
	}
	if record.TTL != 30 {
		t.Errorf("TTL = %d, want the 30 that was asked for", record.TTL)
	}
	if record.Name != "service.lab" {
		t.Errorf("Name = %q, want the name that was asked about", record.Name)
	}
}

// A name that resolves to nothing is a successful message with no answer,
// which is different from a resolver failure and has to decode differently.
func TestAResponseWithNoAddressCarriesNoAnswer(t *testing.T) {
	t.Parallel()
	answer, err := MarshalResponse(mustQuery(t, 7, "missing.lab"), netip.Addr{}, 30)
	if err != nil {
		t.Fatalf("MarshalResponse() = %v, want nil", err)
	}
	message, err := ParseDNS(answer)
	if err != nil {
		t.Fatalf("ParseDNS() = %v, want nil", err)
	}
	if message.RCode != DNSRCodeSuccess {
		t.Errorf("RCode = %s, want noerror", RCodeName(message.RCode))
	}
	if len(message.Answers) != 0 {
		t.Errorf("Answers = %d, want none", len(message.Answers))
	}
}

func TestARefusalCarriesItsResponseCode(t *testing.T) {
	t.Parallel()
	refusal, err := MarshalRefusal(mustQuery(t, 9, "service.lab"))
	if err != nil {
		t.Fatalf("MarshalRefusal() = %v, want nil", err)
	}
	message, err := ParseDNS(refusal)
	if err != nil {
		t.Fatalf("ParseDNS() = %v, want nil", err)
	}
	if message.RCode != DNSRCodeRefused {
		t.Errorf("RCode = %s, want refused", RCodeName(message.RCode))
	}
	if !message.Response {
		t.Error("a refusal decoded as a query")
	}
	if got := RCodeName(DNSRCodeNotFound); got != "nxdomain" {
		t.Errorf("RCodeName(%d) = %q, want nxdomain", DNSRCodeNotFound, got)
	}
	if got := RCodeName(9); got != "rcode9" {
		t.Errorf("RCodeName(9) = %q, want rcode9 rather than a guess", got)
	}
}

// A malformed answer from a peer must be refused rather than guessed at.
func TestAMalformedAnswerIsRefused(t *testing.T) {
	t.Parallel()
	complete := mustQuery(t, 1, "service.lab")
	answer, err := MarshalResponse(complete, netip.MustParseAddr("10.60.0.2"), 30)
	if err != nil {
		t.Fatalf("MarshalResponse() = %v, want nil", err)
	}
	cases := []struct {
		name  string
		input []byte
	}{
		{"an empty answer", nil},
		{"a truncated header", []byte{0x12, 0x34}},
		{"a truncated question", complete[:len(complete)-3]},
		{"a truncated answer", answer[:len(answer)-2]},
		{"a record claiming a huge address", func() []byte {
			corrupt := append([]byte(nil), answer...)
			binary.BigEndian.PutUint16(corrupt[len(corrupt)-6:len(corrupt)-4], 0xffff)
			return corrupt
		}()},
		{"an answer count that is not there", func() []byte {
			corrupt := append([]byte(nil), answer...)
			binary.BigEndian.PutUint16(corrupt[6:8], 0xffff)
			return corrupt
		}()},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseDNS(test.input); err == nil {
				t.Error("ParseDNS() = nil error, want a refusal")
			}
		})
	}
}

// A message that is structurally complete but says nothing is decoded as
// empty rather than refused: the parser checks that the bytes are a message,
// and deciding whether an empty message is a useful answer belongs to whoever
// asked the question.
func TestAnEmptyMessageDecodesAsEmpty(t *testing.T) {
	t.Parallel()
	message, err := ParseDNS(make([]byte, DNSHeaderSize))
	if err != nil {
		t.Fatalf("ParseDNS() = %v, want nil", err)
	}
	if len(message.Questions) != 0 || len(message.Answers) != 0 {
		t.Errorf("the message carries %d questions and %d answers, want none",
			len(message.Questions), len(message.Answers))
	}
}

// A message that points at itself must not make the decoder loop.
func TestASelfReferencingPointerIsRefused(t *testing.T) {
	t.Parallel()
	query := mustQuery(t, 1, "service.lab")
	// Replace the first label length with a pointer to itself.
	query[DNSHeaderSize] = 0xc0
	query[DNSHeaderSize+1] = 0x0c
	if _, err := ParseDNS(query); err == nil {
		t.Error("ParseDNS() = nil error, want a refusal for a name that points at itself")
	}
}

// Answering something that is not a question is refused rather than guessed at.
func TestAnsweringSomethingThatIsNotAQueryIsRefused(t *testing.T) {
	t.Parallel()
	header := make([]byte, DNSHeaderSize)
	binary.BigEndian.PutUint16(header[2:4], dnsResponse)
	if _, err := MarshalResponse(header, netip.MustParseAddr("10.60.0.2"), 30); err == nil {
		t.Error("MarshalResponse() for a message with no question = nil error, want a refusal")
	}
	if _, err := MarshalRefusal(header); err == nil {
		t.Error("MarshalRefusal() for a message with no question = nil error, want a refusal")
	}
}
