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

const serviceMode = "netforge-service"

const controlPipeBytes = 1 << 16

const (
	StateRunning = "running"

	StateFailed = "failed"
)

type Config struct {
	Namespace string `json:"namespace"`

	StatusPath string `json:"status_path"`

	TCPPort uint16 `json:"tcp_port,omitempty"`
	UDPPort uint16 `json:"udp_port,omitempty"`

	StreamPort uint16 `json:"stream_port,omitempty"`

	RUDPPort uint16 `json:"rudp_port,omitempty"`

	DNSPort uint16 `json:"dns_port,omitempty"`

	DNSNames map[string]string `json:"dns_names,omitempty"`

	TLSPort uint16 `json:"tls_port,omitempty"`

	HTTPPort uint16 `json:"http_port,omitempty"`

	HTTPPrefix string `json:"http_prefix,omitempty"`

	HTTPStatus int `json:"http_status,omitempty"`

	HTTPBody string `json:"http_body,omitempty"`

	MaxConnections int `json:"max_connections"`

	MaxRequest int `json:"max_request"`

	Timeout time.Duration `json:"timeout"`
}

type Status struct {
	State string `json:"state"`
	Error string `json:"error,omitempty"`

	Bound []string `json:"bound,omitempty"`

	PID int `json:"pid,omitempty"`

	At time.Time `json:"at"`
}

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

func (c Config) Validate() (Config, error) {
	const op = "service.Config.Validate"
	if c.StatusPath == "" {
		return c, apperr.InvalidArgument(op, "a service needs a status path", nil)
	}
	if len(c.Endpoints()) == 0 {

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

type Service struct {
	config   Config
	listener []net.Listener
	packet   []net.PacketConn
	reliable *transport.Listener

	reliablePeers map[string]*transport.Conn
	reliableMu    sync.Mutex
	server        []*http.Server
	bound         []string
	slots         chan struct{}
	wg            sync.WaitGroup
	cert          tls.Certificate
}

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

		_ = conn.Close()
		return nil, errRefused
	}
}

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

	if info, statErr := os.Stdin.Stat(); statErr == nil && info.Mode()&(os.ModeNamedPipe|os.ModeSocket) != 0 {
		go func() {
			defer cancel()
			_, _ = io.Copy(io.Discard, io.LimitReader(os.Stdin, controlPipeBytes))
		}()
	}

	<-ctx.Done()
	return instance.Close()
}

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

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
}
