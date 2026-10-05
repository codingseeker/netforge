package probe

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/netforge/netforge/internal/apperr"
)

// Protocol is the layer a probe speaks.
type Protocol string

const (
	// ProtocolICMP is an echo request and its reply.
	ProtocolICMP Protocol = "icmp"
	// ProtocolTCP is a connection handshake and an optional byte round trip.
	ProtocolTCP Protocol = "tcp"
	// ProtocolUDP is a datagram and its answer.
	ProtocolUDP Protocol = "udp"
	// ProtocolDNS is a resolver query and its answer.
	ProtocolDNS Protocol = "dns"
	// ProtocolTLS is a handshake and an optional application byte.
	ProtocolTLS Protocol = "tls"
	// ProtocolHTTP is a request and a bounded response body.
	ProtocolHTTP Protocol = "http"
)

// Protocols lists every protocol a probe may speak, in the order the
// diagnostic pipeline walks them.
func Protocols() []Protocol {
	return []Protocol{ProtocolICMP, ProtocolTCP, ProtocolUDP, ProtocolDNS, ProtocolTLS, ProtocolHTTP}
}

// Valid reports whether the protocol is one NetForge knows.
func (p Protocol) Valid() bool { return slices.Contains(Protocols(), p) }

// CarriesApplicationData reports whether a successful probe of this protocol
// proves that the application above it works, rather than only that the network
// below it does. A resolver that answers and a server that returns a body are
// what a later layer depends on.
func (p Protocol) CarriesApplicationData() bool {
	switch p {
	case ProtocolDNS, ProtocolTLS, ProtocolHTTP:
		return true
	default:
		return false
	}
}

// Layer returns the diagnostic layer a protocol belongs to, which is the
// layer a failure is attributed to. TCP and UDP share the transport layer
// because a failure to reach a port is a transport failure whichever datagram
// type carried it.
func (p Protocol) Layer() string {
	switch p {
	case ProtocolDNS:
		return "dns"
	case ProtocolTCP, ProtocolUDP:
		return "transport"
	case ProtocolTLS:
		return "tls"
	case ProtocolHTTP:
		return "http"
	case ProtocolICMP:
		return "network"
	default:
		return "unknown"
	}
}

// parseProtocol validates a protocol name from a configuration or a command
// line.
func parseProtocol(op, name string) (Protocol, error) {
	protocol := Protocol(strings.ToLower(strings.TrimSpace(name)))
	if protocol == "" {
		return "", apperr.InvalidArgument(op, "a probe needs a protocol", nil)
	}
	if !protocol.Valid() {
		return "", apperr.InvalidArgument(op, fmt.Sprintf(
			"%q is not a protocol NetForge probes, expected one of %s",
			name, strings.Join(protocolNames(), ", ")), nil)
	}
	return protocol, nil
}

func protocolNames() []string {
	names := make([]string, 0, len(Protocols()))
	for _, protocol := range Protocols() {
		names = append(names, string(protocol))
	}
	return names
}

// Request describes one probe of one target.
type Request struct {
	// Protocol is the layer to speak.
	Protocol Protocol `json:"protocol"`
	// Namespace is the laboratory namespace the probe runs in. It has to be
	// named in the laboratory's scope, which is checked before anything is
	// sent.
	Namespace string `json:"namespace"`
	// Target is the address to probe. It has to be inside the laboratory's
	// scope, which is checked before anything is sent.
	Target string `json:"target"`
	// Port is the TCP or UDP port for the protocols that need one. DNS, TLS
	// and HTTP carry their own defaults and leave it zero.
	Port uint16 `json:"port,omitempty"`
	// Name is the name a DNS probe asks about, and the server name a TLS or
	// HTTP probe sends.
	Name string `json:"name,omitempty"`
	// Path is the request path an HTTP probe asks for.
	Path string `json:"path,omitempty"`
	// Payload is the number of payload bytes a TCP, UDP or TLS probe sends
	// once the connection or handshake is up. It is bounded by the probe
	// size limit.
	Payload int `json:"payload,omitempty"`
	// ExpectStatus is the HTTP status an HTTP probe treats as the answer it
	// was looking for. Zero accepts any status below 400.
	ExpectStatus int `json:"expect_status,omitempty"`
	// Timeout is how long this one probe may take. It is bounded by the probe
	// duration limit.
	Timeout time.Duration `json:"timeout,omitempty"`
	// Experiment is the experiment the probe belongs to. It is recorded with
	// the result so a measurement can always be traced back to the run that
	// asked for it.
	Experiment string `json:"experiment,omitempty"`
	// VerifyCertificate is whether a TLS or HTTP probe checks the server
	// certificate. A laboratory service presents a certificate this process
	// generated itself, so the default is to pin that certificate rather than
	// to trust a public authority.
	VerifyCertificate bool `json:"verify_certificate,omitempty"`
}

// Result is one structured measurement. Every field is something that was
// observed: a failure carries the reason it failed rather than a zero that
// could be mistaken for a fast answer.
type Result struct {
	// Experiment is the experiment the probe belonged to.
	Experiment string `json:"experiment"`
	// Target is the address that was probed.
	Target string `json:"target"`
	// Namespace is the laboratory namespace the probe ran in.
	Namespace string `json:"namespace"`
	// Protocol is the layer that was spoken.
	Protocol Protocol `json:"protocol"`
	// OK reports whether the probe got the answer it asked for.
	OK bool `json:"ok"`
	// RTT is the measured round trip time. It is zero when nothing came back,
	// which OK distinguishes from an instantaneous answer.
	RTT time.Duration `json:"rtt"`
	// Detail describes what came back: a status line, a resolved address, a
	// handshake version. It is bounded like every other response.
	Detail string `json:"detail,omitempty"`
	// ResponseSize is the number of bytes the answer occupied, counted as it
	// was read and never beyond the response limit. For an echo probe it is
	// the number of replies that came back, which Sent and Received also
	// carry.
	ResponseSize int `json:"response_size"`
	// Sent and Received are the echo request and reply counts, and Loss is the
	// measured loss as a percentage. They are zero for every other protocol,
	// which has one exchange rather than a series.
	Sent     int     `json:"sent,omitempty"`
	Received int     `json:"received,omitempty"`
	Loss     float64 `json:"loss,omitempty"`
	// Truncated reports whether the answer was longer than the response limit
	// and was cut short. A truncated answer is still an answer, and saying so
	// is more honest than reporting a partial size as if it were the whole.
	Truncated bool `json:"truncated,omitempty"`
	// Error is why the probe failed. It is empty when OK is true.
	Error string `json:"error,omitempty"`
	// Failure names the failure in a form a diagnosis can branch on, so the
	// investigator does not have to read prose to tell a timeout from a
	// refusal.
	Failure Failure `json:"failure,omitempty"`
	// At is when the probe finished.
	At time.Time `json:"at"`
	// Duration is the wall clock time the probe took.
	Duration time.Duration `json:"duration"`
}

// Failure is the reason a probe did not get its answer.
type Failure string

const (
	// FailureNone means the probe succeeded.
	FailureNone Failure = ""
	// FailureTimeout means the answer did not arrive in time.
	FailureTimeout Failure = "timeout"
	// FailureRefused means the far end actively refused the connection.
	FailureRefused Failure = "refused"
	// FailureUnreachable means the network said the target could not be
	// reached.
	FailureUnreachable Failure = "unreachable"
	// FailureReset means the connection was reset.
	FailureReset Failure = "reset"
	// FailureDNS means the resolver did not answer, or answered with an error.
	FailureDNS Failure = "dns_error"
	// FailureTLS means the handshake failed or the certificate did not match.
	FailureTLS Failure = "tls_error"
	// FailureHTTP means a response arrived but it was not the one wanted.
	FailureHTTP Failure = "http_status"
	// FailureMalformed means an answer arrived and could not be understood.
	FailureMalformed Failure = "malformed_response"
	// FailureScope means the probe was refused before anything was sent.
	FailureScope Failure = "out_of_scope"
	// FailureLimit means a limit refused the probe before anything was sent.
	FailureLimit Failure = "limit_exceeded"
	// FailureInternal means the probe could not be run at all.
	FailureInternal Failure = "internal"
	// FailureUnknown means the failure was none of the known kinds. It exists
	// so an unrecognised error is reported as unrecognised rather than being
	// filed under a category that would make a diagnosis more confident than
	// the evidence allows.
	FailureUnknown Failure = "unknown"
)

// Succeeded reports whether the probe got its answer.
func (r Result) Succeeded() bool { return r.OK && r.Failure == FailureNone }

// check validates a request against the scope and the limits. Nothing is sent
// until every bound has been checked.
func (r Request) check(op string, limits Limits, scope Scope) (Request, error) {
	protocol, err := parseProtocol(op, string(r.Protocol))
	if err != nil {
		return r, err
	}
	r.Protocol = protocol

	if strings.TrimSpace(r.Namespace) == "" {
		return r, apperr.InvalidArgument(op, "a probe needs a laboratory namespace", nil)
	}
	if err := checkAddress(op, r.Target); err != nil {
		return r, err
	}
	if err := scope.check(op, r.Namespace, r.Target); err != nil {
		return r, err
	}
	if strings.TrimSpace(r.Experiment) == "" {
		return r, apperr.InvalidArgument(op, "a probe needs an experiment to belong to", nil)
	}
	if len(r.Experiment) > limits.MaxNameLength {
		return r, apperr.Validation(op, fmt.Sprintf(
			"the experiment name is %d characters, the maximum is %d",
			len(r.Experiment), limits.MaxNameLength), nil)
	}

	switch protocol {
	case ProtocolICMP:
		// An echo probe carries no port and no payload of its own.
		r.Port, r.Name, r.Path, r.Payload, r.ExpectStatus = 0, "", "", 0, 0
	case ProtocolTCP, ProtocolUDP:
		if r.Port == 0 {
			return r, apperr.InvalidArgument(op, fmt.Sprintf(
				"a %s probe needs a port", protocol), nil)
		}
		r.Name, r.Path, r.ExpectStatus = "", "", 0
	case ProtocolDNS:
		if strings.TrimSpace(r.Name) == "" {
			return r, apperr.InvalidArgument(op, "a dns probe needs a name to ask about", nil)
		}
		if len(r.Name) > limits.MaxResponseSize {
			return r, apperr.LimitExceeded(op, fmt.Sprintf(
				"the queried name is %d bytes, the maximum is %d", len(r.Name), limits.MaxResponseSize), nil)
		}
		if r.Port == 0 {
			r.Port = 53
		}
		r.Path, r.Payload, r.ExpectStatus = "", 0, 0
	case ProtocolTLS:
		if r.Port == 0 {
			r.Port = 443
		}
		r.Path, r.Payload, r.ExpectStatus = "", 0, 0
	case ProtocolHTTP:
		if r.Port == 0 {
			r.Port = 80
		}
		if strings.TrimSpace(r.Path) == "" {
			r.Path = "/"
		}
		if len(r.Path) > limits.MaxResponseSize {
			return r, apperr.LimitExceeded(op, fmt.Sprintf(
				"the request path is %d bytes, the maximum is %d", len(r.Path), limits.MaxResponseSize), nil)
		}
		if r.ExpectStatus < 0 || r.ExpectStatus > 599 {
			return r, apperr.InvalidArgument(op, fmt.Sprintf(
				"%d is not an HTTP status code", r.ExpectStatus), nil)
		}
		r.Payload = 0
	}

	if r.Port > 0 && !protocol.UsesPort() {
		return r, apperr.InvalidArgument(op, fmt.Sprintf(
			"a %s probe does not take a port", protocol), nil)
	}
	if r.Payload < 0 {
		return r, apperr.InvalidArgument(op, "a payload size cannot be negative", nil)
	}
	if r.Payload > limits.MaxSize {
		return r, apperr.LimitExceeded(op, fmt.Sprintf(
			"the payload is %d bytes, the maximum is %d", r.Payload, limits.MaxSize), nil)
	}
	if r.Timeout < 0 {
		return r, apperr.InvalidArgument(op, "a timeout cannot be negative", nil)
	}
	if r.Timeout == 0 {
		r.Timeout = defaultRequestTimeout
	}
	if r.Timeout > limits.MaxDuration {
		return r, apperr.LimitExceeded(op, fmt.Sprintf(
			"the timeout of %s is past the %s limit", r.Timeout, limits.MaxDuration), nil)
	}
	if r.Path != "" && strings.ContainsAny(r.Path, "\r\n") {
		return r, apperr.InvalidArgument(op, "a request path cannot contain a line break", nil)
	}
	return r, nil
}

// UsesPort reports whether a protocol is addressed by a port.
func (p Protocol) UsesPort() bool {
	switch p {
	case ProtocolTCP, ProtocolUDP, ProtocolDNS, ProtocolTLS, ProtocolHTTP:
		return true
	default:
		return false
	}
}

// NeedsListener reports whether a probe of this protocol can only be answered
// by something that is listening.
//
// It is a different question from UsesPort, and the difference matters: a port
// says where to send a datagram, and a listener says whether anything will
// answer. An echo request needs neither, because the network itself answers it,
// so measuring one against a laboratory with no services in it is measuring a
// working path. Every other protocol is answered by a process, and a probe
// against a port nothing is bound to measures a closed port.
func (p Protocol) NeedsListener() bool {
	return p.Valid() && p != ProtocolICMP
}
