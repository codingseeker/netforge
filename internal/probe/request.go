package probe

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/netforge/netforge/internal/apperr"
)

type Protocol string

const (
	ProtocolICMP Protocol = "icmp"

	ProtocolTCP Protocol = "tcp"

	ProtocolUDP Protocol = "udp"

	ProtocolDNS Protocol = "dns"

	ProtocolTLS Protocol = "tls"

	ProtocolHTTP Protocol = "http"
)

func Protocols() []Protocol {
	return []Protocol{ProtocolICMP, ProtocolTCP, ProtocolUDP, ProtocolDNS, ProtocolTLS, ProtocolHTTP}
}

func (p Protocol) Valid() bool { return slices.Contains(Protocols(), p) }

func (p Protocol) CarriesApplicationData() bool {
	switch p {
	case ProtocolDNS, ProtocolTLS, ProtocolHTTP:
		return true
	default:
		return false
	}
}

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

type Request struct {
	Protocol Protocol `json:"protocol"`

	Namespace string `json:"namespace"`

	Target string `json:"target"`

	Port uint16 `json:"port,omitempty"`

	Name string `json:"name,omitempty"`

	Path string `json:"path,omitempty"`

	Payload int `json:"payload,omitempty"`

	ExpectStatus int `json:"expect_status,omitempty"`

	Timeout time.Duration `json:"timeout,omitempty"`

	Experiment string `json:"experiment,omitempty"`

	VerifyCertificate bool `json:"verify_certificate,omitempty"`
}

type Result struct {
	Experiment string `json:"experiment"`

	Target string `json:"target"`

	Namespace string `json:"namespace"`

	Protocol Protocol `json:"protocol"`

	OK bool `json:"ok"`

	RTT time.Duration `json:"rtt"`

	Detail string `json:"detail,omitempty"`

	ResponseSize int `json:"response_size"`

	Sent     int     `json:"sent,omitempty"`
	Received int     `json:"received,omitempty"`
	Loss     float64 `json:"loss,omitempty"`

	Truncated bool `json:"truncated,omitempty"`

	Error string `json:"error,omitempty"`

	Failure Failure `json:"failure,omitempty"`

	At time.Time `json:"at"`

	Duration time.Duration `json:"duration"`
}

type Failure string

const (
	FailureNone Failure = ""

	FailureTimeout Failure = "timeout"

	FailureRefused Failure = "refused"

	FailureUnreachable Failure = "unreachable"

	FailureReset Failure = "reset"

	FailureDNS Failure = "dns_error"

	FailureTLS Failure = "tls_error"

	FailureHTTP Failure = "http_status"

	FailureMalformed Failure = "malformed_response"

	FailureScope Failure = "out_of_scope"

	FailureLimit Failure = "limit_exceeded"

	FailureInternal Failure = "internal"

	FailureUnknown Failure = "unknown"
)

func (r Result) Succeeded() bool { return r.OK && r.Failure == FailureNone }

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

func (p Protocol) UsesPort() bool {
	switch p {
	case ProtocolTCP, ProtocolUDP, ProtocolDNS, ProtocolTLS, ProtocolHTTP:
		return true
	default:
		return false
	}
}

func (p Protocol) NeedsListener() bool {
	return p.Valid() && p != ProtocolICMP
}
