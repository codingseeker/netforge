package probe

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/codingseeker/netforge/internal/apperr"
	"github.com/codingseeker/netforge/internal/packet"
	"github.com/codingseeker/netforge/internal/transport"
)

type AgentPayload struct {
	Request

	MaxResponseSize int `json:"max_response_size"`

	MaxPayload int `json:"max_payload"`
}

func Dispatch(args []string) (bool, int) {
	if len(args) == 0 || args[0] != probeMode {
		return false, 0
	}
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, probeMode+": expected one request argument")
		return true, 2
	}
	var payload AgentPayload
	if err := json.Unmarshal([]byte(args[1]), &payload); err != nil {
		fmt.Fprintln(os.Stderr, probeMode+": "+err.Error())
		return true, 2
	}
	if payload.MaxResponseSize <= 0 {
		payload.MaxResponseSize = 65_507
	}
	if payload.MaxPayload <= 0 {
		payload.MaxPayload = 1400
	}
	result := Probe(context.Background(), payload)

	encoded, err := json.Marshal(result)
	if err != nil {
		fmt.Fprintln(os.Stderr, probeMode+": "+err.Error())
		return true, 1
	}
	fmt.Println(string(encoded))
	if !result.Succeeded() {
		return true, 1
	}
	return true, 0
}

func Probe(ctx context.Context, payload AgentPayload) Result {
	started := time.Now()
	result := Result{
		Experiment: payload.Experiment,
		Namespace:  payload.Namespace,
		Target:     payload.Target,
		Protocol:   payload.Protocol,
		At:         time.Now(),
	}
	request := payload.Request
	if request.Timeout <= 0 {
		request.Timeout = defaultRequestTimeout
	}

	runCtx, cancel := context.WithTimeout(ctx, request.Timeout)
	defer cancel()

	var err error
	switch request.Protocol {
	case ProtocolTCP:
		err = probeTCP(runCtx, request, payload, &result)
	case ProtocolUDP:
		err = probeUDP(runCtx, request, payload, &result)
	case ProtocolDNS:
		err = probeDNS(runCtx, request, payload, &result)
	case ProtocolTLS:
		err = probeTLS(runCtx, request, payload, &result)
	case ProtocolHTTP:
		err = probeHTTP(runCtx, request, payload, &result)
	case ProtocolRUDP:
		err = probeRUDP(runCtx, request, payload, &result)
	case ProtocolICMP:
		err = apperr.Unsupported("probe.Probe",
			"an echo probe is measured by the kernel's own tool, not by this agent", nil)
	default:
		err = apperr.InvalidArgument("probe.Probe", "unknown protocol "+string(request.Protocol), nil)
	}

	result.Duration = time.Since(started)
	if err != nil {
		result.OK = false
		result.Failure = Classify(err)
		if result.Error == "" {
			result.Error = bounded(err.Error(), 400)
		}
		return result
	}

	result.OK = true
	return result
}

func address(request Request) string {
	if request.Port == 0 {
		return request.Target
	}
	return net.JoinHostPort(request.Target, strconv.Itoa(int(request.Port)))
}

func probeTCP(ctx context.Context, request Request, payload AgentPayload, result *Result) error {
	start := time.Now()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address(request))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	result.RTT = time.Since(start)

	if request.Payload > 0 {
		sent, received, err := roundTrip(ctx, conn, request.Payload, payload.MaxResponseSize)
		if err != nil {
			return err
		}
		result.ResponseSize = received
		result.Detail = strconv.Itoa(sent) + " bytes echoed"
		return nil
	}
	result.Detail = "connected"
	return nil
}

func probeUDP(ctx context.Context, request Request, payload AgentPayload, result *Result) error {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "udp", address(request))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(request.Timeout))
	}
	size := request.Payload
	if size <= 0 {
		size = 32
	}
	if size > payload.MaxPayload {
		size = payload.MaxPayload
	}
	start := time.Now()
	if _, err := conn.Write(bytes.Repeat([]byte{0x5a}, size)); err != nil {
		return err
	}
	buffer := make([]byte, payload.MaxResponseSize+1)
	read, err := conn.Read(buffer)
	if err != nil {
		return err
	}
	result.RTT = time.Since(start)
	if read > payload.MaxResponseSize {
		result.ResponseSize = payload.MaxResponseSize
		result.Truncated = true
	} else {
		result.ResponseSize = read
	}
	result.Detail = strconv.Itoa(read) + " bytes"
	return nil
}

func probeDNS(ctx context.Context, request Request, payload AgentPayload, result *Result) error {
	query, err := packet.MarshalQuery(1, request.Name)
	if err != nil {
		return err
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "udp", address(request))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(request.Timeout))
	}

	start := time.Now()
	if _, err := conn.Write(query); err != nil {
		return err
	}
	buffer := make([]byte, payload.MaxResponseSize+1)
	read, err := conn.Read(buffer)
	if err != nil {
		return err
	}
	result.RTT = time.Since(start)
	if read > payload.MaxResponseSize {
		result.ResponseSize = payload.MaxResponseSize
		result.Truncated = true
		buffer = buffer[:payload.MaxResponseSize]
	} else {
		result.ResponseSize = read
	}

	message, err := packet.ParseDNS(buffer[:read])
	if err != nil {
		return err
	}
	if message.RCode != 0 {
		return &resolverError{code: message.RCode}
	}
	for _, answer := range message.Answers {
		if answer.Type == packet.DNSTypeA && answer.Address.IsValid() {
			result.Detail = request.Name + " = " + answer.Address.String()
			return nil
		}
	}
	return &resolverError{code: packet.DNSRCodeNotFound}
}

func probeTLS(ctx context.Context, request Request, payload AgentPayload, result *Result) error {
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{},
		Config: &tls.Config{
			ServerName:         request.Name,
			InsecureSkipVerify: !request.VerifyCertificate,
			MinVersion:         tls.VersionTLS12,
		},
	}
	start := time.Now()
	raw, err := dialer.DialContext(ctx, "tcp", address(request))
	if err != nil {
		return classifyTLS(err)
	}
	defer func() { _ = raw.Close() }()
	conn, ok := raw.(*tls.Conn)
	if !ok {
		return &tlsError{err: fmt.Errorf("the handshake returned a %T", raw)}
	}
	result.RTT = time.Since(start)
	state := conn.ConnectionState()
	result.Detail = "tls" + versionName(state.Version)

	if request.Payload > 0 {
		sent, received, err := roundTrip(ctx, conn, request.Payload, payload.MaxResponseSize)
		if err != nil {
			return err
		}
		result.ResponseSize = received
		result.Detail += ", " + strconv.Itoa(sent) + " bytes echoed"
	}
	return nil
}

func probeHTTP(ctx context.Context, request Request, payload AgentPayload, result *Result) error {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext:         (&net.Dialer{}).DialContext,
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: !request.VerifyCertificate, MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout: request.Timeout,
			DisableKeepAlives:   true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	target := url("http", request)
	start := time.Now()
	request2, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request2)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	body, truncated, err := readBounded(response.Body, payload.MaxResponseSize)
	if err != nil {
		return err
	}
	result.RTT = time.Since(start)
	result.ResponseSize = len(body)
	result.Truncated = truncated
	result.Detail = "HTTP " + strconv.Itoa(response.StatusCode)

	want := request.ExpectStatus
	if want == 0 {
		if response.StatusCode < 400 {
			return nil
		}
	} else if response.StatusCode == want {
		return nil
	}
	return &statusError{status: response.StatusCode, want: want}
}

func roundTrip(ctx context.Context, conn net.Conn, size, maxResponse int) (int, int, error) {
	if size > maxResponse {
		size = maxResponse
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(defaultRequestTimeout))
	}
	if _, err := conn.Write(bytes.Repeat([]byte{0x5a}, size)); err != nil {
		return 0, 0, err
	}
	buffer := make([]byte, size)
	read := 0
	for read < size {
		n, err := conn.Read(buffer[read:])
		if err != nil {
			return size, read, err
		}
		read += n
	}
	return size, read, nil
}

func readBounded(reader io.Reader, limit int) ([]byte, bool, error) {
	if limit <= 0 {
		limit = 65_507
	}
	body, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err != nil {
		return nil, false, err
	}
	if len(body) > limit {
		return body[:limit], true, nil
	}
	return body, false, nil
}

func url(scheme string, request Request) string {
	return scheme + "://" + address(request) + path(request)
}

func path(request Request) string {
	if request.Path == "" {
		return "/"
	}
	if request.Path[0] != '/' {
		return "/" + request.Path
	}
	return request.Path
}

func versionName(version uint16) string {
	switch version {
	case tls.VersionTLS13:
		return "1.3"
	case tls.VersionTLS12:
		return "1.2"
	default:
		return strconv.Itoa(int(version))
	}
}

type resolverError struct{ code uint16 }

func (e *resolverError) Error() string { return "the resolver answered " + packet.RCodeName(e.code) }

type statusError struct{ status, want int }

func (e *statusError) Error() string {
	if e.want == 0 {
		return "the server answered HTTP " + strconv.Itoa(e.status)
	}
	return "the server answered HTTP " + strconv.Itoa(e.status) + ", the probe wanted " + strconv.Itoa(e.want)
}

func classifyTLS(err error) error {
	var record tls.RecordHeaderError
	if errors.As(err, &record) {
		return &tlsError{err}
	}
	var alert tls.AlertError
	if errors.As(err, &alert) {
		return &tlsError{err}
	}
	var cert *tls.CertificateVerificationError
	if errors.As(err, &cert) {
		return &tlsError{err}
	}
	if isTLSHandshakeText(err.Error()) {
		return &tlsError{err}
	}
	return err
}

func isTLSHandshakeText(text string) bool {
	for _, marker := range []string{
		"tls: handshake failure", "tls: first record does not look like a TLS handshake",
		"tls: protocol version not supported", "x509:", "remote error: tls",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

type tlsError struct{ err error }

func (e *tlsError) Error() string { return e.err.Error() }
func (e *tlsError) Unwrap() error { return e.err }

func systemError(err error) string {
	var sysErr *os.SyscallError
	if errors.As(err, &sysErr) {
		return sysErr.Err.Error()
	}
	return ""
}

func Classify(err error) Failure {
	if err == nil {
		return FailureNone
	}
	var resolver *resolverError
	if errors.As(err, &resolver) {
		return FailureDNS
	}
	var status *statusError
	if errors.As(err, &status) {
		return FailureHTTP
	}
	var handshake *tlsError
	if errors.As(err, &handshake) {
		return FailureTLS
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return FailureDNS
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return FailureTimeout
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.Is(err, context.Canceled) {

		return FailureTimeout
	}
	switch systemError(err) {
	case "connection refused":
		return FailureRefused
	case "no route to host", "network is unreachable", "host is unreachable":
		return FailureUnreachable
	case "connection reset by peer", "broken pipe":
		return FailureReset
	}
	if kind, ok := apperr.KindOf(err); ok {
		switch kind {
		case apperr.KindInvalidArgument, apperr.KindValidation, apperr.KindUnsupported:
			return FailureMalformed
		case apperr.KindPermission:
			return FailureScope
		case apperr.KindLimitExceeded:
			return FailureLimit
		case apperr.KindTimeout, apperr.KindCanceled:
			return FailureTimeout
		case apperr.KindNotFound:
			return FailureDNS
		}
	}

	return FailureUnknown
}

func probeRUDP(ctx context.Context, request Request, payload AgentPayload, result *Result) error {
	size := request.Payload
	if size <= 0 {
		size = 32
	}
	if size > payload.MaxPayload {
		size = payload.MaxPayload
	}
	limits := RUDPLimits(payload.MaxPayload)
	conn, err := transport.Dial(ctx, "", address(request), limits)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	start := time.Now()
	_, writeErr := conn.Write(ctx, bytes.Repeat([]byte{0x5a}, size))
	var answer []byte
	var readErr error
	if writeErr == nil {
		answer, readErr = conn.Read(ctx)
	}
	result.RTT = time.Since(start)

	counters := conn.Counters()
	result.Sent = counters.Sent
	result.Received = counters.Received
	result.Retransmitted = counters.Retransmitted
	if counters.Sent > 0 {
		unacknowledged := counters.Sent - counters.Acknowledged
		if unacknowledged < 0 {
			unacknowledged = 0
		}
		result.Loss = float64(unacknowledged) * 100 / float64(counters.Sent)
	}
	if writeErr != nil {
		return writeErr
	}
	if readErr != nil {
		result.Detail = "the transport gave up after " +
			strconv.Itoa(counters.Retransmitted) + " retransmissions: " + readErr.Error()
		return readErr
	}
	if len(answer) > payload.MaxResponseSize {
		result.ResponseSize = payload.MaxResponseSize
		result.Truncated = true
	} else {
		result.ResponseSize = len(answer)
	}
	result.Detail = strconv.Itoa(len(answer)) + " bytes"
	return nil
}

func (r Result) Diagnostics() (int, bool) { return r.Retransmitted, true }

func RUDPLimits(maxPayload int) transport.Limits {
	return transport.Limits{
		MaxConnections: 2,
		MaxPayload:     min(max(maxPayload, 32), int(transport.MaxPayloadSize)),
		MaxWindow:      transport.DefaultLimits().MaxWindow,
		MaxDatagram:    transport.MaxFrameSize,
		IdleTimeout:    10 * time.Second,
	}
}
