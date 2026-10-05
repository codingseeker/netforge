package integration

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/netforge/netforge/internal/chaos"
	"github.com/netforge/netforge/internal/network"
	"github.com/netforge/netforge/internal/probe"
	"github.com/netforge/netforge/internal/transport"
)

const (
	benchmarkStreamPort = 9303
	benchmarkUDPPort    = 9301
	benchmarkRUDPPort   = 9302

	benchmarkMessages = 256
	benchmarkSize     = 1024

	benchmarkTransfer = 60 * time.Second

	benchmarkRead = 400 * time.Millisecond

	benchmarkNetwork = "10.42.0.0/24"
	benchmarkMTU     = 1500
)

const resultPrefix = "RESULT "

var (
	benchAddress = flag.String("benchmark-address", "",
		"the server address the benchmark client measures against")
	benchTCPPort  = flag.Int("benchmark-tcp", 0, "the server's echo port for TCP")
	benchUDPPort  = flag.Int("benchmark-udp", 0, "the server's echo port for UDP")
	benchRUDPPort = flag.Int("benchmark-rudp", 0, "the server's echo port for reliable UDP")
)

type measurement struct {
	Transport string  `json:"transport"`
	Sent      int     `json:"sent"`
	Delivered int     `json:"delivered"`
	Matches   int     `json:"matches"`
	Elapsed   float64 `json:"elapsed_ms"`
	Retransm  int     `json:"retransmitted"`
	SentSegs  int     `json:"sent_segments"`
	AckedSegs int     `json:"acknowledged"`

	Buffered   int     `json:"buffered"`
	Waiting    int     `json:"waiting"`
	Duplicated int     `json:"duplicated"`
	Dropped    int     `json:"dropped"`
	Released   int     `json:"released"`
	ReadBytes  int     `json:"read_bytes"`
	Window     uint32  `json:"window"`
	MeanRTT    float64 `json:"mean_rtt_ms"`
	Complete   bool    `json:"complete"`
	Error      string  `json:"error,omitempty"`
}

func (m measurement) throughput() float64 {
	if m.Elapsed <= 0 {
		return 0
	}
	return float64(m.Sent) / (m.Elapsed / 1000)
}

func (m measurement) goodput() float64 {
	if m.Elapsed <= 0 {
		return 0
	}
	return float64(m.Delivered) / (m.Elapsed / 1000)
}

func (m measurement) lossPercent() float64 {
	if m.Sent == 0 {
		return 0
	}
	return 100 * float64(m.Sent-m.Delivered) / float64(m.Sent)
}

type condition struct {
	name   string
	spec   *chaos.Spec
	reason string
}

func benchmarkConditions() []condition {
	loss := func(percent int) *chaos.Spec {
		return &chaos.Spec{
			Kind: chaos.KindPacketLoss,
			Parameters: chaos.Parameters{
				Loss: chaos.Percent(percent),
			},
		}
	}
	return []condition{
		{name: "0% loss", reason: "an unimpaired path, the baseline every other row is read against"},
		{name: "1% loss", spec: loss(1), reason: "a path that is nearly clean, where retransmission should cost almost nothing"},
		{name: "5% loss", spec: loss(5), reason: "a lossy path, where the transports should start to differ"},
		{name: "10% loss", spec: loss(10), reason: "a badly lossy path, where a reliable transport should still deliver everything"},
		{
			name: "high latency",
			spec: &chaos.Spec{
				Kind:       chaos.KindLatency,
				Parameters: chaos.Parameters{Latency: chaos.Duration(60 * time.Millisecond)},
			},
			reason: "a slow path, where a window larger than the bandwidth delay product pays off",
		},
		{
			name: "high jitter",
			spec: &chaos.Spec{
				Kind: chaos.KindJitter,
				Parameters: chaos.Parameters{
					Latency: chaos.Duration(40 * time.Millisecond),
					Jitter:  chaos.Duration(30 * time.Millisecond),
				},
			},
			reason: "a path whose delay varies, which also reorders packets",
		},
	}
}

func TestTransportComparisonRecordsEveryScenario(t *testing.T) {
	rows, err := measureComparison(t)
	if err != nil {
		t.Fatalf("the comparison could not be run: %v", err)
	}
	if err := writeBenchmarks(rows); err != nil {
		t.Errorf("record the comparison: %v", err)
	}
}

func measureComparison(t *testing.T) ([]benchmarkRow, error) {
	t.Helper()
	var rows []benchmarkRow
	for _, test := range benchmarkConditions() {
		t.Run(test.name, func(t *testing.T) {
			results, err := runCondition(t, test)
			if err != nil {
				t.Fatalf("run the %s scenario: %v", test.name, err)
			}
			rows = append(rows, benchmarkRow{Condition: test, Results: results})
		})
	}
	if len(rows) == 0 {
		return nil, errors.New("no scenario produced a result")
	}
	return rows, nil
}

func runCondition(t *testing.T, test condition) ([]measurement, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)

	plan, err := network.BuildPlan("transport", benchmarkNetwork, 1, 1, benchmarkMTU)
	if err != nil {
		return nil, err
	}
	spec := granted(labSpec("transport"))
	spec.Chaos = chaos.Limits{
		MaxFaults:    4,
		MaxDuration:  3 * time.Minute,
		MaxLatency:   5 * time.Second,
		MaxBandwidth: 10_000_000,
		MaxQueue:     1024,
		MaxGap:       100,
	}
	spec.Probes = probe.Limits{
		MaxProbes: 10, MaxRate: 200, MaxSize: 1400,
		MaxDuration: 100 * time.Second, MaxConcurrent: 1,
	}

	spec.Services = []network.ServiceSpec{{
		Namespace:  plan.Nodes[1].Namespace,
		StreamPort: benchmarkStreamPort,
		UDPPort:    benchmarkUDPPort,
		RUDPPort:   benchmarkRUDPPort,
		MaxRequest: transport.MaxPayloadSize,

		Timeout: 30 * time.Second,
	}}
	lab, err := network.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() {
		if err := lab.Destroy(context.Background()); err != nil {
			t.Errorf("destroy: %v", err)
		}
	})

	_, client, err := lab.Client(0)
	if err != nil {
		return nil, err
	}
	_, server, err := lab.Server(0)
	if err != nil {
		return nil, err
	}
	if test.spec != nil {
		fault := *test.spec

		fault.Target = client.Namespace
		fault.Interface = client.Interface
		if _, err := lab.Chaos().Apply(ctx, "transport-comparison", fault); err != nil {
			return nil, fmt.Errorf("apply %s: %w", fault.Kind, err)
		}
	}

	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	stdout, stderr, code, err := lab.Run(client.Namespace,
		self,
		"-test.run=^TestTransportBenchmarkChild$",
		"-test.v",
		"-benchmark-address="+server.Address.Addr().String(),
		fmt.Sprintf("-benchmark-tcp=%d", benchmarkStreamPort),
		fmt.Sprintf("-benchmark-udp=%d", benchmarkUDPPort),
		fmt.Sprintf("-benchmark-rudp=%d", benchmarkRUDPPort),
	)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("the client in %s exited with %d: %s",
			client.Namespace, code, strings.TrimSpace(stderr))
	}
	results, err := parseResults(stdout)
	if err != nil {
		return nil, fmt.Errorf("read the client's report: %w\n%s", err, strings.TrimSpace(stdout))
	}
	if len(results) != 3 {
		return nil, fmt.Errorf("the client reported %d transports, want 3", len(results))
	}
	for _, got := range results {
		if err := checkMeasurement(test, got); err != nil {
			return nil, err
		}
	}
	return results, nil
}

func parseResults(stdout string) ([]measurement, error) {
	var out []measurement
	for _, line := range strings.Split(stdout, "\n") {
		index := strings.Index(line, resultPrefix)
		if index < 0 {
			continue
		}
		var got measurement
		payload := line[index+len(resultPrefix):]
		if err := json.Unmarshal([]byte(payload), &got); err != nil {
			return nil, fmt.Errorf("decode %q: %w", payload, err)
		}
		if got.Transport == "" {
			return nil, fmt.Errorf("a result names no transport: %q", payload)
		}
		out = append(out, got)
	}
	if len(out) == 0 {
		return nil, errors.New("the client reported nothing")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Transport < out[j].Transport })
	return out, nil
}

func checkMeasurement(test condition, got measurement) error {
	if got.Sent != benchmarkMessages*benchmarkSize {
		return fmt.Errorf("%s was offered %d bytes, want %d",
			got.Transport, got.Sent, benchmarkMessages*benchmarkSize)
	}
	if got.Matches > got.Delivered {
		return fmt.Errorf("%s matched %d payloads but only delivered %d",
			got.Transport, got.Matches, got.Delivered)
	}
	switch got.Transport {
	case "rudp":

		if got.Delivered != got.Sent || !got.Complete {
			return fmt.Errorf("the reliable transport delivered %d of %d bytes, complete=%t, "+
				"segments sent=%d acknowledged=%d retransmitted=%d window=%d, "+
				"released=%d read=%d waiting=%d buffered=%d duplicated=%d dropped=%d: %s",
				got.Delivered, got.Sent, got.Complete,
				got.SentSegs, got.AckedSegs, got.Retransm, got.Window,
				got.Released, got.ReadBytes,
				got.Waiting, got.Buffered, got.Duplicated, got.Dropped, got.Error)
		}
		if got.Retransm < 0 {
			return fmt.Errorf("the reliable transport reported %d retransmissions", got.Retransm)
		}
	default:

		if got.Delivered > got.Sent {
			return fmt.Errorf("%s delivered %d bytes from %d sent",
				got.Transport, got.Delivered, got.Sent)
		}
	}
	if test.name == "" {
		return errors.New("a recorded row has no condition")
	}
	return nil
}

func TestTransportBenchmarkChild(t *testing.T) {
	if *benchAddress == "" {
		t.Skip("this test is the client half of the comparison and is run by its parent")
	}
	targets := []struct {
		name string
		port int
		run  func(string) (measurement, error)
	}{
		{"udp", *benchUDPPort, func(addr string) (measurement, error) { return measureUDP(addr) }},
		{"tcp", *benchTCPPort, func(addr string) (measurement, error) { return measureTCP(addr) }},
		{"rudp", *benchRUDPPort, func(addr string) (measurement, error) { return measureRUDP(addr) }},
	}
	for _, target := range targets {
		if target.port == 0 {
			t.Fatalf("the %s comparison has no port to measure", target.name)
		}
		got, err := target.run(net.JoinHostPort(*benchAddress, fmt.Sprint(target.port)))
		got.Transport = target.name
		if err != nil && got.Error == "" {
			got.Error = err.Error()
		}
		payload, marshalErr := json.Marshal(got)
		if marshalErr != nil {
			t.Fatalf("encode the %s result: %v", target.name, marshalErr)
		}
		fmt.Printf("%s%s\n", resultPrefix, payload)
	}
}

func measureUDP(address string) (measurement, error) {
	conn, err := net.Dial("udp", address)
	if err != nil {
		return measurement{}, err
	}
	defer func() { _ = conn.Close() }()
	stream := &datagramStream{conn: conn}
	return runTransfer(stream)
}

func measureTCP(address string) (measurement, error) {
	conn, err := net.Dial("tcp", address)
	if err != nil {
		return measurement{}, err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(benchmarkTransfer)); err != nil {
		return measurement{}, err
	}
	stream := &streamConn{conn: conn}
	return runTransfer(stream)
}

var benchmarkRUDPLimits = transport.Limits{
	MaxConnections: 4,
	MaxPayload:     benchmarkSize,
	MaxWindow:      32,

	IdleTimeout: 30 * time.Second,

	MaxRetransmits:    40,
	RetransmitTimeout: 100 * time.Millisecond,
}

func measureRUDP(address string) (measurement, error) {
	limits := benchmarkRUDPLimits
	ctx, cancel := context.WithTimeout(context.Background(), benchmarkTransfer)
	defer cancel()
	conn, err := transport.Dial(ctx, "0.0.0.0:0", address, limits)
	if err != nil {
		return measurement{}, err
	}
	defer func() { _ = conn.Close() }()
	stream := &reliableStream{conn: conn}
	got, err := runTransfer(stream)
	counters := conn.Counters()
	receive := conn.ReceiveStats()
	got.Retransm = counters.Retransmitted
	got.SentSegs = counters.Sent
	got.AckedSegs = counters.Acknowledged
	got.Buffered = receive.Buffered
	got.Waiting = receive.Waiting
	got.Duplicated = receive.Duplicated
	got.Dropped = receive.Dropped
	got.Released = receive.Delivered
	got.ReadBytes = counters.Received
	got.Window = counters.Window
	return got, err
}

type stream interface {
	Send(payload []byte) error

	Receive() ([]byte, error)

	Close() error
}

func runTransfer(s stream) (measurement, error) {
	defer func() { _ = s.Close() }()
	got := measurement{Sent: benchmarkMessages * benchmarkSize}
	expected := make(map[string]int, benchmarkMessages)
	sentAt := make([]atomic.Int64, benchmarkMessages)

	for index := range benchmarkMessages {
		payload := make([]byte, benchmarkSize)
		fill(payload, index)
		expected[string(payload)] = index
	}

	started := time.Now()
	delivered, matched, answers := 0, 0, 0
	var roundTrip time.Duration
	writeDone := make(chan error, 1)
	go func() {
		for index := range benchmarkMessages {
			payload := make([]byte, benchmarkSize)
			fill(payload, index)
			sentAt[index].Store(time.Now().UnixNano())
			if err := s.Send(payload); err != nil {
				writeDone <- fmt.Errorf("payload %d: %w", index, err)
				return
			}
		}
		writeDone <- nil
	}()

	tally := func(err error) (measurement, error) {
		got.Elapsed = msSince(started)
		got.Delivered = delivered
		got.Matches = matched
		got.Complete = len(expected) == 0
		if matched > 0 {
			got.MeanRTT = float64(roundTrip.Microseconds()) / float64(matched) / 1000
		}
		return got, err
	}

	for len(expected) > 0 {
		answered, err := s.Receive()
		if err != nil {

			select {
			case writeErr := <-writeDone:
				if writeErr != nil {
					return tally(writeErr)
				}
			default:
			}
			return tally(err)
		}
		if len(answered) == 0 {

			break
		}
		delivered += len(answered)
		answers++
		key := string(answered)
		if index, wanted := expected[key]; wanted {
			delete(expected, key)
			matched++

			roundTrip += time.Since(time.Unix(0, sentAt[index].Load()))
		}
	}
	if err := <-writeDone; err != nil {
		return tally(err)
	}
	_ = answers
	return tally(nil)
}

func fill(payload []byte, index int) {
	binary.BigEndian.PutUint32(payload[:4], uint32(index))
	for offset := 4; offset < len(payload); offset++ {
		payload[offset] = byte((index*31 + offset*7) % 251)
	}
}

func msSince(started time.Time) float64 {
	return float64(time.Since(started).Microseconds()) / 1000
}

type datagramStream struct {
	conn net.Conn
	buf  []byte
}

func (s *datagramStream) Send(payload []byte) error {
	_, err := s.conn.Write(payload)
	return err
}

func (s *datagramStream) Receive() ([]byte, error) {
	if err := s.conn.SetReadDeadline(time.Now().Add(benchmarkRead)); err != nil {
		return nil, err
	}
	if s.buf == nil {
		s.buf = make([]byte, benchmarkSize)
	}
	read, err := s.conn.Read(s.buf)
	if err != nil {
		var expired net.Error
		if errors.As(err, &expired) && expired.Timeout() {

			return nil, nil
		}
		return nil, err
	}
	return s.buf[:read], nil
}

func (s *datagramStream) Close() error { return s.conn.Close() }

type streamConn struct {
	conn   net.Conn
	buf    []byte
	held   []byte
	closed bool
}

func (s *streamConn) Send(payload []byte) error {
	_, err := s.conn.Write(payload)
	return err
}

func (s *streamConn) Receive() ([]byte, error) {
	for len(s.held) < benchmarkSize {
		if s.buf == nil {
			s.buf = make([]byte, 8*1024)
		}
		read, err := s.conn.Read(s.buf)
		if read > 0 {
			s.held = append(s.held, s.buf[:read]...)
		}
		if err != nil {
			var expired net.Error
			if errors.As(err, &expired) && expired.Timeout() {
				return nil, nil
			}
			return nil, err
		}
	}
	payload := s.held[:benchmarkSize]
	s.held = s.held[benchmarkSize:]
	return payload, nil
}

func (s *streamConn) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.conn.Close()
}

type reliableStream struct {
	conn *transport.Conn
}

func (s *reliableStream) Send(payload []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), benchmarkTransfer)
	defer cancel()
	_, err := s.conn.Write(ctx, payload)
	return err
}

func (s *reliableStream) Receive() ([]byte, error) {

	ctx, cancel := context.WithTimeout(context.Background(), benchmarkTransfer)
	defer cancel()
	return s.conn.Read(ctx)
}

func (s *reliableStream) Close() error { return s.conn.Close() }
