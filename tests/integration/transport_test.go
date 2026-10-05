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

// The benchmark compares plain UDP, TCP and NetForge's reliable UDP over the
// same impaired path. The faults are injected by the kernel inside the
// laboratory's namespaces rather than by a proxy in userspace, because a
// proxy that drops bytes from a TCP stream corrupts that stream instead of
// modelling packet loss: TCP would never retransmit what a proxy swallowed,
// so the comparison would be measuring the proxy rather than the transports.
//
// The client half of every transfer runs in the client namespace, because a
// test process cannot enter a network namespace without taking its own threads
// with it. It re-executes this test binary with -test.run set to the child
// test below, and reports one RESULT line per transport.

const (
	// benchmarkStreamPort is a separate endpoint because the service's TCP echo
	// answers one request and hangs up, which is what a probe measures and not
	// what a transfer needs.
	benchmarkStreamPort = 9303
	benchmarkUDPPort    = 9301
	benchmarkRUDPPort   = 9302
	// benchmarkMessages and benchmarkSize fix the work every transport is asked
	// to do, so the numbers are comparable: 256 payloads of 1 KiB, which is
	// enough work that a lossy path shows what retransmission costs. A transfer
	// of a few payloads measures the path's first packet, not its behaviour.
	benchmarkMessages = 256
	benchmarkSize     = 1024
	// benchmarkTransfer bounds one transport's transfer. A path that cannot
	// finish in this time has failed, which is a result rather than a hang. It
	// is generous because a lossy path with a collapsed window is slow by
	// design, and a bound that ended the measurement would report the bound.
	benchmarkTransfer = 60 * time.Second
	// benchmarkRead bounds one read on an unreliable transport, where a lost
	// datagram is never retried and the read has to give up on its own.
	benchmarkRead = 400 * time.Millisecond
	// benchmarkNetwork and benchmarkMTU are the laboratory the comparison runs
	// in. They are named rather than written at the call site because the
	// recorded report has to carry them.
	benchmarkNetwork = "10.42.0.0/24"
	benchmarkMTU     = 1500
)

// resultPrefix marks the child's report so the parent can find it among the
// test framework's own output.
const resultPrefix = "RESULT "

// The client's arguments are registered before the test framework parses the
// command line, because a flag declared inside a test is not known yet when
// the parsing happens and the child would be rejected before it started.
var (
	benchAddress = flag.String("benchmark-address", "",
		"the server address the benchmark client measures against")
	benchTCPPort  = flag.Int("benchmark-tcp", 0, "the server's echo port for TCP")
	benchUDPPort  = flag.Int("benchmark-udp", 0, "the server's echo port for UDP")
	benchRUDPPort = flag.Int("benchmark-rudp", 0, "the server's echo port for reliable UDP")
)

// measurement is what one transport achieved over one path.
type measurement struct {
	Transport string  `json:"transport"`
	Sent      int     `json:"sent"`
	Delivered int     `json:"delivered"`
	Matches   int     `json:"matches"`
	Elapsed   float64 `json:"elapsed_ms"`
	Retransm  int     `json:"retransmitted"`
	SentSegs  int     `json:"sent_segments"`
	AckedSegs int     `json:"acknowledged"`
	// The receive side's own state, so a failure can be told apart from a
	// path that lost data: a connection holding what it was sent, with a
	// reader that was not given it, failed differently from one that never
	// received it.
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

// throughput is bytes handed to the transport per second, which is what was
// offered rather than what arrived.
func (m measurement) throughput() float64 {
	if m.Elapsed <= 0 {
		return 0
	}
	return float64(m.Sent) / (m.Elapsed / 1000)
}

// goodput is payload bytes that arrived intact per second, which is what the
// application actually got. A transport that loses data cannot be credited
// with the bytes it discarded.
func (m measurement) goodput() float64 {
	if m.Elapsed <= 0 {
		return 0
	}
	return float64(m.Delivered) / (m.Elapsed / 1000)
}

// lossPercent is the share of sent payload that never arrived.
func (m measurement) lossPercent() float64 {
	if m.Sent == 0 {
		return 0
	}
	return 100 * float64(m.Sent-m.Delivered) / float64(m.Sent)
}

// condition is one path the comparison is run over.
type condition struct {
	name   string
	spec   *chaos.Spec
	reason string
}

// benchmarkConditions are the scenarios the comparison records. Every one of
// them is described rather than implied, because a number without its
// conditions is not a result.
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

// TestTransportComparisonRecordsEveryScenario runs the comparison over every
// recorded condition and checks the numbers mean what they claim to. It writes
// the table it measured to docs/BENCHMARKS.md, so the evidence in the
// documentation is the run rather than a copy of it.
func TestTransportComparisonRecordsEveryScenario(t *testing.T) {
	rows, err := measureComparison(t)
	if err != nil {
		t.Fatalf("the comparison could not be run: %v", err)
	}
	if err := writeBenchmarks(rows); err != nil {
		t.Errorf("record the comparison: %v", err)
	}
}

// measureComparison builds one laboratory per condition, impairs the path with
// the kernel, and asks a client in the client namespace to run the three
// transfers across it.
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

// runCondition creates the laboratory, applies the fault, and collects what the
// client saw on the far side of it.
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
	// The echo endpoints have to exist in the server namespace before the
	// client is pointed at them.
	spec.Services = []network.ServiceSpec{{
		Namespace:  plan.Nodes[1].Namespace,
		StreamPort: benchmarkStreamPort,
		UDPPort:    benchmarkUDPPort,
		RUDPPort:   benchmarkRUDPPort,
		MaxRequest: transport.MaxPayloadSize,
		// The service's timeout bounds one exchange and is also the idle bound
		// of its reliable endpoint. A probe needs a few seconds; a transfer
		// across a lossy path needs longer than that to be answered, and a
		// bound that expires mid-transfer ends the echo rather than the fault
		// being measured.
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
		// The fault is applied to the client end of the link, so the client
		// sees the impaired path and the server's own queue stays clean.
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

// parseResults reads the child's report, which is one RESULT line per
// transport and nothing the parent has to interpret any other way.
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

// checkMeasurement asserts what a result has to mean before it is recorded, so
// a broken measurement cannot be written down as a comparison.
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
		// A reliable transport that loses payload is not reliable, whatever
		// the path did to it.
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
		// An unreliable transport is allowed to lose data, but not to invent
		// any: nothing it reports may exceed what was sent.
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

// TestTransportBenchmarkChild is the client half of the comparison. It runs
// inside the client namespace, against the echo endpoints the server namespace
// serves, and reports one line per transport. It is not a test in its own
// right: it exits through the report.
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

// measureUDP offers the same payloads over a plain datagram socket. There is
// no retransmission, so a lost datagram is a lost payload and the read that
// would have received it has to give up: the transfer finishes when the path
// stops answering, not when everything arrives.
func measureUDP(address string) (measurement, error) {
	conn, err := net.Dial("udp", address)
	if err != nil {
		return measurement{}, err
	}
	defer func() { _ = conn.Close() }()
	stream := &datagramStream{conn: conn}
	return runTransfer(stream)
}

// measureTCP offers the same payloads over a stream socket.
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

// benchmarkRUDPLimits are the reliable transport's limits for this comparison.
// They are a package variable rather than a literal inside measureRUDP because
// the recorded report has to carry the limits the numbers were produced with; a
// transport benchmark whose window size is not written down is not
// reproducible.
var benchmarkRUDPLimits = transport.Limits{
	MaxConnections: 4,
	MaxPayload:     benchmarkSize,
	MaxWindow:      32,
	// The idle timeout is a liveness bound, and this run is measuring
	// throughput on a path that is deliberately slow to recover on. A bound
	// tuned for a healthy path would end these transfers while they were
	// still making progress, which measures the bound rather than the
	// transport, so it is set well above what a healthy path needs and the
	// transfer itself is bounded by the context instead.
	IdleTimeout: 30 * time.Second,
	// How many times a segment may be sent again before the connection is
	// abandoned is a policy about dead peers, and it is measured by the
	// transport's own tests rather than here. A benchmark on a 10% loss
	// path needs the attempts to outlast the path's bad luck, so the
	// ceiling is raised and the transfer's own deadline is what ends a run.
	MaxRetransmits:    40,
	RetransmitTimeout: 100 * time.Millisecond,
}

// measureRUDP offers the same payloads over NetForge's reliable UDP transport,
// with limits large enough that the window rather than the bound is what
// shapes the transfer.
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

// stream is the one shape every transport is measured through, so the three
// rows of the comparison differ only in the transport underneath.
type stream interface {
	// Send offers one payload.
	Send(payload []byte) error
	// Receive returns the next payload that arrived. It returns a nil payload
	// with a nil error when the path has stopped answering, which is how an
	// unreliable transport's absence is reported.
	Receive() ([]byte, error)
	// Close ends the transfer.
	Close() error
}

// runTransfer is the measurement itself: offer every payload, read back what
// comes, and check that what comes back is what was sent. Every payload
// carries its own index, so a payload that arrived in the wrong place or was
// corrupted is caught rather than counted.
func runTransfer(s stream) (measurement, error) {
	defer func() { _ = s.Close() }()
	got := measurement{Sent: benchmarkMessages * benchmarkSize}
	expected := make(map[string]int, benchmarkMessages)
	sentAt := make([]atomic.Int64, benchmarkMessages)
	// The payloads are built before anything is sent, so the map the reader
	// matches against is complete and the two halves of the transfer do not
	// have to agree on who fills it.
	for index := range benchmarkMessages {
		payload := make([]byte, benchmarkSize)
		fill(payload, index)
		expected[string(payload)] = index
	}

	// The payloads are written while they are being read, which is what a
	// windowed transport requires of its caller. Offering the whole transfer
	// first and reading afterwards would not measure a slow path: every
	// transport here bounds what it will hold, so a sender that offers more
	// than the peer will take and never reads stalls against its own
	// backpressure, and the stall would be reported as the transport's
	// behaviour.
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

	// tally records what the transfer achieved, and is called on every way out.
	// A transfer that stops early still reports what it managed: a row reading
	// zero because the harness gave up says something different from one
	// reading zero because nothing arrived.
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
			// The writer's failure is the more useful of the two errors, since
			// a stalled reader is usually a consequence of a refused write.
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
			// The path has stopped answering. What is left never arrived, and
			// a transport that does not retransmit will not deliver it.
			break
		}
		delivered += len(answered)
		answers++
		key := string(answered)
		if index, wanted := expected[key]; wanted {
			delete(expected, key)
			matched++
			// The time from offering this payload to reading its answer back is
			// one round trip over whatever the path did in between, which is
			// what a latency figure has to be measured from.
			roundTrip += time.Since(time.Unix(0, sentAt[index].Load()))
		}
	}
	if err := <-writeDone; err != nil {
		return tally(err)
	}
	_ = answers
	return tally(nil)
}

// fill writes a payload that is unique to its index: the index itself is the
// first four bytes, and the rest is a pattern derived from it. A pattern alone
// is not enough, because any pattern over a large enough range of indices
// repeats, and two identical payloads would make an answer ambiguous: one
// answer would satisfy whichever of them the reader happened to be looking for,
// and a transfer would report itself complete while a message was still
// missing.
func fill(payload []byte, index int) {
	binary.BigEndian.PutUint32(payload[:4], uint32(index))
	for offset := 4; offset < len(payload); offset++ {
		payload[offset] = byte((index*31 + offset*7) % 251)
	}
}

func msSince(started time.Time) float64 {
	return float64(time.Since(started).Microseconds()) / 1000
}

// datagramStream measures a plain UDP socket. Each receive is bounded, because
// an absent datagram is never retried and only a bound ends the wait.
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
			// A quiet path, not a broken one: the datagram is simply gone.
			return nil, nil
		}
		return nil, err
	}
	return s.buf[:read], nil
}

func (s *datagramStream) Close() error { return s.conn.Close() }

// streamConn measures a stream socket, where one read can return part of a
// payload, so the stream is framed on the same index the sender wrote.
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

// reliableStream measures the reliable transport. Its reads are bounded by the
// connection's own idle timeout rather than by a read deadline, because the
// transport promises that a payload sent inside the window will arrive: a
// silent path here is a failure, not an absence.
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
	// A read on a reliable transport is bounded by the transfer, not by the
	// short read bound the unreliable ones use. A segment lost in transit is
	// retransmitted after the transport's own timer, and cutting the wait
	// shorter than that would abandon a payload the transport was going to
	// recover, which would measure this harness rather than the transport.
	ctx, cancel := context.WithTimeout(context.Background(), benchmarkTransfer)
	defer cancel()
	return s.conn.Read(ctx)
}

func (s *reliableStream) Close() error { return s.conn.Close() }
