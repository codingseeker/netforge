package service

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/netforge/netforge/internal/packet"
)

// testConfig is a small service with one port, so a test can be about one
// behaviour rather than five.
func testConfig(t *testing.T, mutate func(*Config)) Config {
	t.Helper()
	config := DefaultConfig()
	config.StatusPath = filepath.Join(t.TempDir(), "status.json")
	config.MaxConnections = 8
	config.Timeout = 2 * time.Second
	if mutate != nil {
		mutate(&config)
	}
	return config
}

// startService opens a service and closes it when the test ends.
func startService(t *testing.T, config Config) (*Service, context.CancelFunc) {
	t.Helper()
	checked, err := config.Validate()
	if err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	instance, err := New(checked)
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := instance.Close(); err != nil {
			t.Errorf("Close() = %v, want nil", err)
		}
	})
	return instance, func() {}
}

// freePort asks the kernel for a port nothing is using. A fixed port would make
// a test fail for reasons that have nothing to do with the service.
func freePort(t *testing.T) uint16 {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for a free port: %v", err)
	}
	defer func() { _ = listener.Close() }()
	return uint16(listener.Addr().(*net.TCPAddr).Port)
}

// dnsQuery encodes a resolver query the way a probe would send it.
func dnsQuery(t *testing.T, id uint16, name string) []byte {
	t.Helper()
	query, err := packet.MarshalQuery(id, name)
	if err != nil {
		t.Fatalf("MarshalQuery() = %v, want nil", err)
	}
	return query
}

// The status file is how a caller learns that a service is ready, so it has to
// name the endpoints that are actually bound.
func TestTheStatusFileNamesWhatIsBound(t *testing.T) {
	t.Parallel()
	port := freePort(t)
	config := testConfig(t, func(c *Config) { c.TCPPort = port })
	instance, _ := startService(t, config)

	status := Status{State: StateRunning, Bound: instance.Bound(), At: time.Now()}
	if len(status.Bound) != 1 {
		t.Fatalf("Bound = %v, want one endpoint", status.Bound)
	}
	if !strings.HasPrefix(status.Bound[0], "tcp/") {
		t.Errorf("Bound[0] = %q, want it to name the protocol", status.Bound[0])
	}
	if !strings.HasSuffix(status.Bound[0], ":"+strconv.Itoa(int(port))) {
		t.Errorf("Bound[0] = %q, want it to name port %d", status.Bound[0], port)
	}
}

func TestTheEchoServiceAnswersWithWhatItWasSent(t *testing.T) {
	t.Parallel()
	port := freePort(t)
	startService(t, testConfig(t, func(c *Config) { c.TCPPort = port }))

	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))), time.Second)
	if err != nil {
		t.Fatalf("Dial() = %v, want nil", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	sent := []byte("netforge")
	if _, err := conn.Write(sent); err != nil {
		t.Fatalf("Write() = %v, want nil", err)
	}
	buffer := make([]byte, 64)
	read, err := conn.Read(buffer)
	if err != nil {
		t.Fatalf("Read() = %v, want nil", err)
	}
	if string(buffer[:read]) != string(sent) {
		t.Errorf("the answer was %q, want %q", buffer[:read], sent)
	}
}

func TestTheDNSServiceAnswersForTheNamesItKnows(t *testing.T) {
	t.Parallel()
	port := freePort(t)
	startService(t, testConfig(t, func(c *Config) {
		c.DNSPort = port
		c.DNSNames = map[string]string{"service.lab": "10.60.0.2"}
	}))

	conn, err := net.DialTimeout("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))), time.Second)
	if err != nil {
		t.Fatalf("Dial() = %v, want nil", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	query := dnsQuery(t, 0x4321, "service.lab")
	if _, err := conn.Write(query); err != nil {
		t.Fatalf("Write() = %v, want nil", err)
	}
	buffer := make([]byte, 512)
	read, err := conn.Read(buffer)
	if err != nil {
		t.Fatalf("Read() = %v, want nil", err)
	}
	message, err := packet.ParseDNS(buffer[:read])
	if err != nil {
		t.Fatalf("decode the answer: %v", err)
	}
	if message.RCode != packet.DNSRCodeSuccess {
		t.Errorf("the response code is %s, want noerror", packet.RCodeName(message.RCode))
	}
	if len(message.Answers) != 1 || message.Answers[0].Address.String() != "10.60.0.2" {
		t.Errorf("the answer is %v, want a single 10.60.0.2", message.Answers)
	}

	// A name it does not know is refused rather than answered with something
	// plausible.
	if _, err := conn.Write(dnsQuery(t, 0x4322, "other.lab")); err != nil {
		t.Fatalf("Write() = %v, want nil", err)
	}
	read, err = conn.Read(buffer)
	if err != nil {
		t.Fatalf("Read() = %v, want nil", err)
	}
	refused, err := packet.ParseDNS(buffer[:read])
	if err != nil {
		t.Fatalf("decode the refusal: %v", err)
	}
	if len(refused.Answers) != 0 {
		t.Errorf("the refusal carried %v, want no answers", refused.Answers)
	}
}

func TestTheTLSServiceCompletesAHandshake(t *testing.T) {
	t.Parallel()
	port := freePort(t)
	startService(t, testConfig(t, func(c *Config) { c.TLSPort = port }))

	conn, err := tls.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))),
		&tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) //nolint:gosec // the service presents a certificate this test generates.
	if err != nil {
		t.Fatalf("tls.Dial() = %v, want nil", err)
	}
	defer func() { _ = conn.Close() }()
	state := conn.ConnectionState()
	if state.Version < tls.VersionTLS12 {
		t.Errorf("the handshake used %x, want at least TLS 1.2", state.Version)
	}
}

// An HTTP endpoint answers its own path and refuses the rest, because a probe
// that gets 404 for everything cannot tell a routing fault from a wrong path.
func TestTheHTTPServiceAnswersItsPathAndRefusesTheRest(t *testing.T) {
	t.Parallel()
	port := freePort(t)
	startService(t, testConfig(t, func(c *Config) {
		c.HTTPPort = port
		c.HTTPPrefix = "/health"
		c.HTTPBody = "ok"
	}))

	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	address := "http://127.0.0.1:" + strconv.Itoa(int(port))

	response, err := client.Get(address + "/health")
	if err != nil {
		t.Fatalf("Get(/health) = %v, want nil", err)
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64))
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Errorf("the status for /health is %d, want 200", response.StatusCode)
	}
	if string(body) != "ok" {
		t.Errorf("the body is %q, want ok", body)
	}

	// A path below the prefix is still the prefix, which is what a probe
	// asking for a resource under it expects.
	response, err = client.Get(address + "/health/detail")
	if err != nil {
		t.Fatalf("Get(/health/detail) = %v, want nil", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Errorf("the status for /health/detail is %d, want 200", response.StatusCode)
	}

	response, err = client.Get(address + "/other")
	if err != nil {
		t.Fatalf("Get(/other) = %v, want nil", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("the status for /other is %d, want 404", response.StatusCode)
	}
}

// At the connection limit the answer is a refusal, not a queue: a laboratory
// must never accumulate work it cannot bound.
func TestTheConnectionLimitRefusesRatherThanQueues(t *testing.T) {
	t.Parallel()
	port := freePort(t)
	config := testConfig(t, func(c *Config) {
		c.TCPPort = port
		c.MaxConnections = 1
	})
	startService(t, config)

	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
	held, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatalf("the first Dial() = %v, want nil", err)
	}
	defer func() { _ = held.Close() }()
	// The slot is taken by the first connection, which the service is holding
	// open by never reading it.
	_ = held.SetDeadline(time.Now().Add(500 * time.Millisecond))
	buffer := make([]byte, 16)
	_, _ = held.Read(buffer)

	second, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		return // A refused connection at the limit is also a valid answer.
	}
	defer func() { _ = second.Close() }()
	_ = second.SetDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := second.Write([]byte("netforge")); err != nil {
		return
	}
	if read, err := second.Read(buffer); err == nil && read > 0 {
		t.Errorf("the second connection was answered with %q, want it refused at the limit", buffer[:read])
	}
}

// A configuration that could not be served is refused before any listener is
// opened, so a half-started service never survives.
func TestAnInvalidConfigurationIsRefused(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"no status path", func(c *Config) { c.StatusPath = "" }},
		{"no endpoint at all", func(c *Config) {
			c.TCPPort, c.UDPPort, c.DNSPort, c.TLSPort, c.HTTPPort = 0, 0, 0, 0, 0
		}},
		{"a connection limit of zero", func(c *Config) { c.MaxConnections = 0 }},
		{"a request limit past the datagram maximum", func(c *Config) { c.MaxRequest = 1 << 20 }},
		{"no timeout", func(c *Config) { c.Timeout = 0 }},
		{"a path that is not a path", func(c *Config) { c.HTTPPrefix = "health" }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			// Every case except the one about endpoints needs something to
			// serve, or it would be refused for the wrong reason.
			config := testConfig(t, func(c *Config) {
				c.TCPPort = freePort(t)
				if test.mutate != nil {
					test.mutate(c)
				}
			})
			if _, err := config.Validate(); err == nil {
				t.Errorf("Validate() = nil error, want a refusal for %s", test.name)
			}
		})
	}
}

// A laboratory that cannot read a status file has to be told, rather than
// carrying on as though the service were ready.
func TestAnUnreadableStatusIsAnError(t *testing.T) {
	t.Parallel()
	if _, err := ReadStatus(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("ReadStatus() for a missing file = nil error, want an error")
	}
	path := filepath.Join(t.TempDir(), "status.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadStatus(path); err == nil {
		t.Error("ReadStatus() for unreadable content = nil error, want an error")
	}
}

func TestServeReportsAFailedConfiguration(t *testing.T) {
	t.Parallel()
	config := testConfig(t, func(c *Config) { c.StatusPath = "" })
	if err := Serve(context.Background(), config); err == nil {
		t.Error("Serve() with an invalid configuration = nil error, want a refusal")
	}
}

func TestCloseIsSafeToCallTwice(t *testing.T) {
	t.Parallel()
	port := freePort(t)
	instance, _ := startService(t, testConfig(t, func(c *Config) { c.TCPPort = port }))
	if err := instance.Close(); err != nil {
		t.Errorf("the first Close() = %v, want nil", err)
	}
	if err := instance.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Errorf("the second Close() = %v, want nil or a closed error", err)
	}
}
