// Package service provides the endpoints a laboratory probes.
//
// A measurement of a DNS resolver, a TLS server or an HTTP server needs
// something on the far end to answer. This package is that something: small,
// bounded, dependency-free listeners that a probe can measure, run inside a
// laboratory namespace and never on the host. It exists so the evidence in a
// diagnosis comes from a real exchange rather than from a closed port.
package service

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/packet"
	"github.com/netforge/netforge/internal/transport"
)

// serviceMode is the argument the service agent is started with.
const serviceMode = "netforge-service"

// controlPipeBytes bounds the read on the laboratory agent's control pipe. The
// agent never writes to it, so anything that arrives is discarded; the bound
// exists so that a peer which found the pipe cannot keep this process reading.
const controlPipeBytes = 1 << 16

// States a service reports in its status file.
const (
	// StateRunning means every configured listener is bound.
	StateRunning = "running"
	// StateFailed means the service could not start, with the reason.
	StateFailed = "failed"
)

// Config describes the endpoints one namespace serves.
type Config struct {
	// Namespace is the laboratory namespace the service runs in. It is
	// recorded so a status file can be traced back to the run that wrote it.
	Namespace string `json:"namespace"`
	// StatusPath is where the service writes its state. The laboratory polls
	// this file to know when the listeners are bound.
	StatusPath string `json:"status_path"`
	// TCP and UDP are the ports for the echo endpoints. Zero disables them.
	TCPPort uint16 `json:"tcp_port,omitempty"`
	UDPPort uint16 `json:"udp_port,omitempty"`
	// StreamPort is the port for the stream echo endpoint, which answers every
	// message a peer sends for as long as it keeps the connection open. The TCP
	// and TLS endpoints answer one request and hang up, which is what a probe
	// wants; a transfer wants a conversation. Zero disables it.
	StreamPort uint16 `json:"stream_port,omitempty"`
	// RUDPPort is the port for the reliable-UDP echo endpoint, which is the
	// transport a throughput comparison needs an answer from. Zero disables it.
	RUDPPort uint16 `json:"rudp_port,omitempty"`
	// DNSPort is the port for the resolver. Zero disables it.
	DNSPort uint16 `json:"dns_port,omitempty"`
	// DNSNames maps a name the resolver answers for to the address it answers
	// with. A name that is not in the map is refused, which is what lets a
	// diagnosis tell a resolver that works from one that does not know the
	// name.
	DNSNames map[string]string `json:"dns_names,omitempty"`
	// TLSPort is the port for the TLS endpoint. Zero disables it.
	TLSPort uint16 `json:"tls_port,omitempty"`
	// HTTPPort is the port for the HTTP endpoint. Zero disables it.
	HTTPPort uint16 `json:"http_port,omitempty"`
	// HTTPPrefix is the path prefix the HTTP endpoint serves. Anything else
	// answers 404, which is what lets a probe ask for a resource that is not
	// there and get a real answer.
	HTTPPrefix string `json:"http_prefix,omitempty"`
	// HTTPStatus is the status the HTTP endpoint answers with for its own
	// prefix.
	HTTPStatus int `json:"http_status,omitempty"`
	// HTTPBody is the body the HTTP endpoint answers with.
	HTTPBody string `json:"http_body,omitempty"`
	// MaxConnections bounds how many connections are served at once.
	MaxConnections int `json:"max_connections"`
	// MaxRequest bounds the largest request or datagram the service reads.
	MaxRequest int `json:"max_request"`
	// Timeout bounds how long one exchange may take.
	Timeout time.Duration `json:"timeout"`
}

// Status is what the service reports about itself.
type Status struct {
	State string `json:"state"`
	Error string `json:"error,omitempty"`
	// Bound lists the endpoints that are listening, each as "protocol/address"
	// exactly as it was bound.
	Bound []string `json:"bound,omitempty"`
	// PID is the process serving the endpoints, so a status names what is
	// answering rather than only that something is.
	PID int `json:"pid,omitempty"`
	// At is when the status was written.
	At time.Time `json:"at"`
}

// DefaultConfig returns a small service with safe bounds.
func DefaultConfig() Config {
	return Config{
		HTTPPrefix:     "/",
		HTTPStatus:     200,
		HTTPBody:       "netforge laboratory endpoint\n",
		MaxConnections: 32,
		MaxRequest:     65_507,
		Timeout:        10 * time.Second,
	}
}

// Validate checks a configuration before a listener is opened.
func (c Config) Validate() (Config, error) {
	const op = "service.Config.Validate"
	if c.StatusPath == "" {
		return c, apperr.InvalidArgument(op, "a service needs a status path", nil)
	}
	if len(c.Endpoints()) == 0 {
		// A service with nothing to serve would report itself ready while
		// answering nothing, which is the one answer a probe must not get.
		return c, apperr.InvalidArgument(op, "a service needs at least one endpoint to serve", nil)
	}
	if c.MaxConnections <= 0 {
		return c, apperr.InvalidArgument(op, "max_connections must be positive", nil)
	}
	if c.MaxRequest <= 0 || c.MaxRequest > 65_507 {
		return c, apperr.InvalidArgument(op, fmt.Sprintf(
			"max_request must be between 1 and 65507, got %d", c.MaxRequest), nil)
	}
	if c.Timeout <= 0 {
		return c, apperr.InvalidArgument(op, "timeout must be positive", nil)
	}
	if c.HTTPPrefix != "" && c.HTTPPrefix[0] != '/' {
		return c, apperr.InvalidArgument(op, "the http prefix has to start with a slash", nil)
	}
	if c.HTTPStatus < 100 || c.HTTPStatus > 599 {
		return c, apperr.InvalidArgument(op, fmt.Sprintf(
			"%d is not an HTTP status code", c.HTTPStatus), nil)
	}
	for name, address := range c.DNSNames {
		if name == "" {
			return c, apperr.InvalidArgument(op, "a dns name cannot be empty", nil)
		}
		if _, err := netip.ParseAddr(address); err != nil {
			return c, apperr.InvalidArgument(op, fmt.Sprintf(
				"the dns name %s is mapped to %q, which is not an address", name, address), nil)
		}
	}
	return c, nil
}

// Endpoints reports the "protocol/port" pairs a configuration asks for, which
// is what a laboratory records so a reader can see what was on the far end.
func (c Config) Endpoints() []string {
	var out []string
	add := func(name string, port uint16) {
		if port != 0 {
			out = append(out, name+"/"+strconv.Itoa(int(port)))
		}
	}
	add("tcp", c.TCPPort)
	add("stream", c.StreamPort)
	add("udp", c.UDPPort)
	add("rudp", c.RUDPPort)
	add("dns", c.DNSPort)
	add("tls", c.TLSPort)
	add("http", c.HTTPPort)
	return out
}

// Service is a running set of listeners.
type Service struct {
	config   Config
	listener []net.Listener
	packet   []net.PacketConn
	reliable *transport.Listener
	// reliablePeers remembers the reliable connections already being echoed, so
	// a continuing conversation does not grow a second reader per datagram.
	reliablePeers map[string]*transport.Conn
	reliableMu    sync.Mutex
	server        []*http.Server
	bound         []string
	slots         chan struct{}
	wg            sync.WaitGroup
	cert          tls.Certificate
}

// New opens every configured listener. If any of them cannot be opened, the
// ones that were are closed again, so a half-started service never survives.
func New(config Config) (*Service, error) {
	const op = "service.New"
	checked, err := config.Validate()
	if err != nil {
		return nil, err
	}
	service := &Service{
		config:        checked,
		slots:         make(chan struct{}, checked.MaxConnections),
		reliablePeers: make(map[string]*transport.Conn),
	}
	defer func() {
		if err != nil {
			service.closeListeners()
		}
	}()

	if checked.TCPPort != 0 {
		if err := service.listenTCP("tcp", checked.TCPPort, service.serveEchoTCP); err != nil {
			return nil, err
		}
	}
	if checked.StreamPort != 0 {
		if err := service.listenTCP("stream", checked.StreamPort, service.serveEchoStream); err != nil {
			return nil, err
		}
	}
	if checked.TLSPort != 0 {
		certificate, err := selfSignedCertificate()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		service.cert = certificate
		if err := service.listenTCP("tls", checked.TLSPort, service.serveEchoTLS); err != nil {
			return nil, err
		}
	}
	if checked.HTTPPort != 0 {
		if err := service.listenHTTP(checked.HTTPPort); err != nil {
			return nil, err
		}
	}
	if checked.UDPPort != 0 {
		if err := service.listenUDP("udp", checked.UDPPort, service.serveEchoUDP); err != nil {
			return nil, err
		}
	}
	if checked.RUDPPort != 0 {
		if err := service.listenReliable(checked.RUDPPort); err != nil {
			return nil, err
		}
	}
	if checked.DNSPort != 0 {
		if err := service.listenUDP("dns", checked.DNSPort, service.serveDNS); err != nil {
			return nil, err
		}
	}
	return service, nil
}

// Bound reports the endpoints that are listening, each as "protocol/address"
// exactly as it was bound. The address is the one the listener really holds, so
// a wildcard shows as a wildcard rather than being papered over with an
// address nobody chose.
func (s *Service) Bound() []string {
	return append([]string(nil), s.bound...)
}

func (s *Service) listenTCP(name string, port uint16, handle func(net.Conn)) error {
	const op = "service.listenTCP"
	listener, err := net.Listen("tcp", ":"+strconv.Itoa(int(port)))
	if err != nil {
		return fmt.Errorf("%s: cannot listen for %s on port %d: %w", op, name, port, err)
	}
	s.listener = append(s.listener, listener)
	s.bound = append(s.bound, name+"/"+listener.Addr().String())
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.accept(listener, handle)
	}()
	return nil
}

func (s *Service) listenUDP(name string, port uint16, handle func([]byte, net.Addr) []byte) error {
	const op = "service.listenUDP"
	conn, err := net.ListenPacket("udp", ":"+strconv.Itoa(int(port)))
	if err != nil {
		return fmt.Errorf("%s: cannot listen for %s on port %d: %w", op, name, port, err)
	}
	s.packet = append(s.packet, conn)
	s.bound = append(s.bound, name+"/"+conn.LocalAddr().String())
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.datagrams(conn, handle)
	}()
	return nil
}

// listenReliable opens the reliable-UDP echo endpoint. The transport's own
// limits are the service's limits, so the endpoint a laboratory serves is as
// bounded as the datagram one: a reliable peer cannot make the service hold
// more connections or read a larger payload than the configuration allows.
func (s *Service) listenReliable(port uint16) error {
	const op = "service.listenReliable"
	limits := transport.Limits{
		MaxConnections: s.config.MaxConnections,
		MaxPayload:     s.config.MaxRequest,
		MaxWindow:      transport.DefaultLimits().MaxWindow,
		IdleTimeout:    s.config.Timeout,
	}
	listener, err := transport.Listen(context.Background(), ":"+strconv.Itoa(int(port)), limits)
	if err != nil {
		return fmt.Errorf("%s: cannot listen for rudp on port %d: %w", op, port, err)
	}
	s.reliable = listener
	s.bound = append(s.bound, "rudp/"+net.JoinHostPort("", strconv.Itoa(int(port))))
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.readReliable(listener)
	}()
	return nil
}

// readReliable applies datagrams to the peer they belong to until the listener
// closes. The listener reads one datagram at a time, so the loop is the one
// the transport's own tests run; a peer refused over the connection limit is a
// limit working, not a reason to stop serving the peers already admitted.
func (s *Service) readReliable(listener *transport.Listener) {
	ctx := context.Background()
	for {
		conn, err := listener.ServePeer(ctx)
		if err != nil {
			if errors.Is(err, transport.ErrClosed) {
				return
			}
			var overLimit interface{ Error() string }
			if errors.As(err, &overLimit) {
				continue
			}
			return
		}
		s.echoTo(conn)
	}
}

// echoTo starts the one echo a reliable peer gets. The peer is remembered
// first, so the datagrams that keep arriving on an already served connection
// do not start a second reader for it: two readers would answer each payload
// twice, which is a different protocol than the one the peer is speaking. At
// the connection limit the peer is refused rather than queued, the same answer
// the stream endpoint gives.
func (s *Service) echoTo(conn *transport.Conn) {
	if conn == nil {
		return
	}
	key := conn.RemoteAddr().String()
	s.reliableMu.Lock()
	if _, served := s.reliablePeers[key]; served {
		s.reliableMu.Unlock()
		return
	}
	s.reliablePeers[key] = conn
	s.reliableMu.Unlock()

	select {
	case s.slots <- struct{}{}:
	default:
		_ = conn.Close()
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() { <-s.slots }()
		s.serveEchoReliable(conn)
	}()
}

// serveEchoReliable writes back every payload the peer sends until it closes.
// The datagram and stream endpoints answer one request and hang up, which is
// what a probe wants; this endpoint has to survive a whole transfer, so the
// exchange lasts as long as the peer keeps it open and the timeout bounds one
// message rather than the connection.
func (s *Service) serveEchoReliable(conn *transport.Conn) {
	defer func() { _ = conn.Close() }()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), s.config.Timeout)
		payload, err := conn.Read(ctx)
		if err != nil {
			cancel()
			return
		}
		_, err = conn.Write(ctx, payload)
		cancel()
		if err != nil {
			return
		}
	}
}

func (s *Service) accept(listener net.Listener, handle func(net.Conn)) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		select {
		case s.slots <- struct{}{}:
		default:
			// At the connection limit the answer is a refusal, not a queue:
			// a laboratory must never accumulate work it cannot bound.
			_ = conn.Close()
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { <-s.slots }()
			handle(conn)
		}()
	}
}

func (s *Service) datagrams(conn net.PacketConn, handle func([]byte, net.Addr) []byte) {
	// One buffer for the life of the listener: a datagram service that
	// allocates per packet can be driven into allocating without bound.
	buffer := make([]byte, s.config.MaxRequest)
	for {
		read, from, err := conn.ReadFrom(buffer)
		if err != nil {
			return
		}
		if read > s.config.MaxRequest {
			continue
		}
		request := make([]byte, read)
		copy(request, buffer[:read])
		answer := handle(request, from)
		if len(answer) == 0 {
			continue
		}
		if len(answer) > s.config.MaxRequest {
			answer = answer[:s.config.MaxRequest]
		}
		_, _ = conn.WriteTo(answer, from)
	}
}

// serveEchoTCP reads a bounded request and writes it back, which is what makes
// a TCP or TLS probe a round trip rather than only a handshake.
func (s *Service) serveEchoTCP(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(s.config.Timeout))
	buffer := make([]byte, s.config.MaxRequest)
	read, err := conn.Read(buffer)
	if err != nil || read == 0 {
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(s.config.Timeout))
	_, _ = conn.Write(buffer[:read])
}

// serveEchoStream writes back every message the peer sends until it closes.
// The deadline is moved for every exchange rather than set once, so a peer that
// keeps talking is answered for as long as it is talking and a peer that stops
// is still bounded.
func (s *Service) serveEchoStream(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	buffer := make([]byte, s.config.MaxRequest)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(s.config.Timeout))
		read, err := conn.Read(buffer)
		if err != nil || read == 0 {
			return
		}
		_ = conn.SetWriteDeadline(time.Now().Add(s.config.Timeout))
		if _, err := conn.Write(buffer[:read]); err != nil {
			return
		}
	}
}

func (s *Service) serveEchoTLS(conn net.Conn) {
	// The handshake path owns the connection too, so every way out of here
	// closes it.
	defer func() { _ = conn.Close() }()
	if s.cert.Certificate == nil {
		return
	}
	secured := tls.Server(conn, &tls.Config{
		Certificates: []tls.Certificate{s.cert},
		MinVersion:   tls.VersionTLS12,
	})
	_ = secured.SetDeadline(time.Now().Add(s.config.Timeout))
	if err := secured.Handshake(); err != nil {
		return
	}
	s.serveEchoTCP(secured)
}

func (s *Service) serveEchoUDP(request []byte, _ net.Addr) []byte {
	return request
}

// serveDNS answers an address query from the configured map and refuses
// anything it does not know, which is a real resolver behaviour a probe can
// measure.
func (s *Service) serveDNS(request []byte, _ net.Addr) []byte {
	message, err := packet.ParseDNS(request)
	if err != nil || message.Response || len(message.Questions) == 0 {
		return nil
	}
	question := message.Questions[0]
	if question.Type != packet.DNSTypeA {
		refusal, err := packet.MarshalRefusal(request)
		if err != nil {
			return nil
		}
		return refusal
	}
	mapped, ok := s.config.DNSNames[question.Name]
	if !ok {
		refusal, err := packet.MarshalRefusal(request)
		if err != nil {
			return nil
		}
		return refusal
	}
	address, err := netip.ParseAddr(mapped)
	if err != nil {
		return nil
	}
	answer, err := packet.MarshalResponse(request, address, 30)
	if err != nil {
		return nil
	}
	return answer
}

// pathUnderPrefix reports whether a request path is the prefix itself or below
// it.
func pathUnderPrefix(path, prefix string) bool {
	if path == prefix {
		return true
	}
	if prefix == "/" {
		return true
	}
	return len(path) > len(prefix) && path[:len(prefix)] == prefix &&
		(path[len(prefix)] == '/' || path[len(prefix)] == '?')
}

// listenHTTP serves the HTTP endpoint. The server owns the listener, so it
// lives as long as the service does rather than for one connection, and the
// connection bound is applied to what it accepts.
func (s *Service) listenHTTP(port uint16) error {
	const op = "service.listenHTTP"
	listener, err := net.Listen("tcp", ":"+strconv.Itoa(int(port)))
	if err != nil {
		return fmt.Errorf("%s: cannot listen for http on port %d: %w", op, port, err)
	}
	server := &http.Server{
		Handler:           s.httpHandler(),
		ReadHeaderTimeout: s.config.Timeout,
		ReadTimeout:       s.config.Timeout,
		WriteTimeout:      s.config.Timeout,
		MaxHeaderBytes:    8 * 1024,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	s.server = append(s.server, server)
	s.listener = append(s.listener, listener)
	s.bound = append(s.bound, "http/"+listener.Addr().String())
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		_ = server.Serve(&boundedListener{Listener: listener, slots: s.slots})
	}()
	return nil
}

// httpHandler answers the configured prefix and refuses everything else.
func (s *Service) httpHandler() http.Handler {
	prefix := s.config.HTTPPrefix
	if prefix == "" {
		prefix = "/"
	}
	status := s.config.HTTPStatus
	if status == 0 {
		status = 200
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !pathUnderPrefix(request.URL.Path, prefix) {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "text/plain")
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(s.config.HTTPBody))
	})
}

// boundedListener refuses connections past the service's limit rather than
// queueing them, and returns the slot it took when the connection it handed
// over is closed.
type boundedListener struct {
	net.Listener
	slots chan struct{}
}

func (l *boundedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	select {
	case l.slots <- struct{}{}:
		return &slotConn{Conn: conn, slots: l.slots}, nil
	default:
		// At the connection limit the answer is a refusal, not a queue.
		_ = conn.Close()
		return nil, errRefused
	}
}

// slotConn gives a slot back when the connection it carries is closed.
type slotConn struct {
	net.Conn
	slots    chan struct{}
	released bool
}

func (c *slotConn) Close() error {
	if !c.released {
		c.released = true
		<-c.slots
	}
	return c.Conn.Close()
}

// errRefused reports that the service is at its connection limit. It is not
// temporary, so a server stops rather than spinning on it.
var errRefused = errors.New("the service is at its connection limit")

func (s *Service) closeListeners() {
	if s.reliable != nil {
		_ = s.reliable.Close()
	}
	for _, server := range s.server {
		_ = server.Close()
	}
	for _, listener := range s.listener {
		_ = listener.Close()
	}
	for _, conn := range s.packet {
		_ = conn.Close()
	}
}

// Close stops every listener and waits for the goroutines serving them.
func (s *Service) Close() error {
	s.closeListeners()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	return nil
}

// Serve runs the service until the context is cancelled, reporting its state
// to the status file as it goes.
func Serve(ctx context.Context, config Config) error {
	const op = "service.Serve"
	checked, err := config.Validate()
	if err != nil {
		writeStatus(config, Status{State: StateFailed, Error: err.Error()})
		return err
	}
	instance, err := New(checked)
	if err != nil {
		writeStatus(checked, Status{State: StateFailed, Error: err.Error()})
		return fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = instance.Close() }()
	writeStatus(checked, Status{
		State: StateRunning,
		Bound: instance.Bound(),
		PID:   os.Getpid(),
		At:    time.Now(),
	})

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The laboratory agent holds the write end of a pipe whose read end is this
	// process's standard input, and the kernel closes it when the agent exits
	// however it exits. A service inside a namespace nobody owns any more has
	// nothing left to serve, so the end of that pipe is the signal to stop; the
	// listeners close on the way out, which is what frees the namespace.
	//
	// Nothing is expected to arrive on the pipe and everything read is discarded,
	// but the read is bounded anyway: a peer that found it and wrote to it must
	// not be able to keep this process reading forever.
	//
	// Only a pipe is watched. Started from a terminal or with no standard input
	// at all, a service runs until it is signalled.
	if info, statErr := os.Stdin.Stat(); statErr == nil && info.Mode()&(os.ModeNamedPipe|os.ModeSocket) != 0 {
		go func() {
			defer cancel()
			_, _ = io.Copy(io.Discard, io.LimitReader(os.Stdin, controlPipeBytes))
		}()
	}

	<-ctx.Done()
	return instance.Close()
}

// ReadStatus reads a service status file.
func ReadStatus(path string) (Status, error) {
	const op = "service.ReadStatus"
	data, err := os.ReadFile(path)
	if err != nil {
		return Status{}, apperr.IO(op, "read "+path, err)
	}
	var status Status
	if err := json.Unmarshal(data, &status); err != nil {
		return Status{}, apperr.IO(op, "decode "+path, err)
	}
	return status, nil
}

// writeStatus reports the service state atomically, so a reader never sees a
// half-written file.
func writeStatus(config Config, status Status) {
	if config.StatusPath == "" {
		return
	}
	if status.At.IsZero() {
		status.At = time.Now()
	}
	data, err := json.Marshal(status)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(config.StatusPath), 0o700); err != nil {
		return
	}
	temporary := config.StatusPath + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(temporary, config.StatusPath)
}

// Dispatch runs the service agent.
func Dispatch(args []string) (bool, int) {
	if len(args) == 0 || args[0] != serviceMode {
		return false, 0
	}
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, serviceMode+": expected one configuration argument")
		return true, 2
	}
	var config Config
	if err := json.Unmarshal([]byte(args[1]), &config); err != nil {
		fmt.Fprintln(os.Stderr, serviceMode+": "+err.Error())
		return true, 2
	}
	ctx, stop := signalContext()
	defer stop()
	if err := Serve(ctx, config); err != nil {
		fmt.Fprintln(os.Stderr, serviceMode+": "+err.Error())
		return true, 1
	}
	return true, 0
}

// selfSignedCertificate generates the certificate the TLS endpoint presents.
// It is generated in the laboratory, for the laboratory, and never leaves it:
// the probe either pins it or is told explicitly not to verify.
func selfSignedCertificate() (tls.Certificate, error) {
	const op = "service.selfSignedCertificate"
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("%s: %w", op, err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("%s: %w", op, err)
	}
	now := time.Now()
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "netforge.lab"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"netforge.lab", "localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("%s: %w", op, err)
	}
	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("%s: %w", op, err)
	}
	certificate, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}),
	)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("%s: %w", op, err)
	}
	return certificate, nil
}

// signalContext returns a context cancelled by SIGTERM or SIGINT, so the
// service is stopped by the same signal the laboratory uses.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
}
