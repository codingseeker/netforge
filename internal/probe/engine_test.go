package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/netforge/netforge/internal/apperr"
)

func fakeScope() Scope {
	return Scope{
		Network:    netip.MustParsePrefix("10.60.0.0/24"),
		Namespaces: []string{"nf-client0", "nf-server0"},
	}
}

func testLimits() Limits {
	return Limits{
		MaxProbes:        4,
		MaxRate:          100,
		MaxSize:          256,
		MaxDuration:      10 * time.Second,
		MaxConcurrent:    2,
		MaxResponseSize:  512,
		MaxStoredResults: 8,
		MaxNameLength:    64,
	}
}

type engineRunner struct {
	mu      sync.Mutex
	calls   [][]string
	result  func(payload AgentPayload) Result
	code    int
	stdout  string
	stderr  string
	err     error
	delay   time.Duration
	running int
	peak    int
}

func (r *engineRunner) Run(namespace string, argv ...string) (string, string, int, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{namespace}, argv...))
	r.running++
	if r.running > r.peak {
		r.peak = r.running
	}
	stdout, stderr, code, err := r.stdout, r.stderr, r.code, r.err
	build := r.result
	delay := r.delay
	r.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	var payload AgentPayload
	if len(argv) == 3 {
		_ = json.Unmarshal([]byte(argv[2]), &payload)
	}
	out := stdout
	if build != nil {
		encoded, merr := json.Marshal(build(payload))
		if merr != nil {
			return "", merr.Error(), 1, nil
		}
		out = string(encoded)
	}

	r.mu.Lock()
	r.running--
	r.mu.Unlock()
	return out, stderr, code, err
}

func (r *engineRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func echoResult(payload AgentPayload) Result {
	return Result{
		Experiment:   payload.Experiment,
		Namespace:    payload.Namespace,
		Target:       payload.Target,
		Protocol:     payload.Protocol,
		OK:           true,
		RTT:          2 * time.Millisecond,
		ResponseSize: 8,
		Detail:       "answered",
	}
}

func newEngine(t *testing.T, runner Runner, echo Echor) *Engine {
	t.Helper()
	engine, err := NewEngine(testLimits(), fakeScope(), runner, echo, "/bin/true")
	if err != nil {
		t.Fatalf("NewEngine() = %v, want nil", err)
	}
	return engine
}

func validRequest() Request {
	return Request{
		Protocol:   ProtocolTCP,
		Namespace:  "nf-client0",
		Target:     "10.60.0.2",
		Port:       9100,
		Experiment: "unit",
		Timeout:    2 * time.Second,
	}
}

func TestARefusedProbeIsRecordedAsAProbe(t *testing.T) {
	t.Parallel()
	runner := &engineRunner{result: echoResult}
	engine := newEngine(t, runner, nil)

	request := validRequest()
	request.Namespace = "host"
	result, err := engine.Run(context.Background(), request)
	if err == nil {
		t.Fatalf("Run() = %+v, want a refusal", result)
	}
	if runner.count() != 0 {
		t.Errorf("the runner was called %d times, want 0: a refused probe never leaves the process", runner.count())
	}
	results := engine.Results()
	if len(results) != 1 {
		t.Fatalf("Results() = %d results, want 1", len(results))
	}
	if results[0].OK {
		t.Error("the recorded result is OK, want a failure")
	}
	if results[0].Namespace != "host" {
		t.Errorf("the recorded namespace = %q, want the namespace that was refused", results[0].Namespace)
	}
}

func TestScopeRefusesWhatIsOutsideTheLaboratory(t *testing.T) {
	t.Parallel()
	runner := &engineRunner{result: echoResult}
	engine := newEngine(t, runner, nil)

	cases := []struct {
		name    string
		request Request
	}{
		{"an unknown namespace", Request{Namespace: "host"}},
		{"an address outside the network", Request{Target: "203.0.113.9"}},
		{"the host loopback", Request{Target: "127.0.0.1"}},
		{"an empty namespace", Request{Namespace: ""}},
		{"an empty target", Request{Target: ""}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := validRequest()
			request.Namespace = test.request.Namespace
			request.Target = test.request.Target
			if _, err := engine.Run(context.Background(), request); err == nil {
				t.Errorf("Run(%s) = nil error, want a refusal", test.name)
			}
		})
	}
	if runner.count() != 0 {
		t.Errorf("the runner was called %d times, want 0", runner.count())
	}
}

func TestAnInvalidRequestIsRefused(t *testing.T) {
	t.Parallel()
	runner := &engineRunner{result: echoResult}
	engine := newEngine(t, runner, nil)

	cases := []struct {
		name    string
		mutate  func(*Request)
		wantSub string
	}{
		{"an unknown protocol", func(r *Request) { r.Protocol = "smtp" }, "protocol"},
		{"a negative port", func(r *Request) { r.Port = 0; r.Target = "10.60.0.2:99999" }, "address"},
		{"no port for a protocol that needs one", func(r *Request) { r.Port = 0 }, "port"},
		{"a payload past the limit", func(r *Request) { r.Payload = 100_000 }, "payload"},
		{"a timeout past the limit", func(r *Request) { r.Timeout = time.Hour }, "timeout"},
		{"a name that is too long", func(r *Request) {
			r.Experiment = strings.Repeat("x", 5000)
		}, "name"},
		{"a DNS probe with no name", func(r *Request) {
			r.Protocol, r.Name = ProtocolDNS, ""
		}, "name"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := validRequest()
			test.mutate(&request)
			_, err := engine.Run(context.Background(), request)
			if err == nil {
				t.Fatalf("Run(%s) = nil error, want a refusal", test.name)
			}
			if !strings.Contains(err.Error(), test.wantSub) {
				t.Errorf("Run(%s) = %q, want it to mention %q", test.name, err, test.wantSub)
			}
		})
	}
}

func TestAnEchoProbeDropsAPort(t *testing.T) {
	t.Parallel()
	echo := &fakeEchor{measurement: Measurement{Sent: 1, Received: 1, AverageRTT: time.Millisecond}}
	engine := newEngine(t, &engineRunner{result: echoResult}, echo)

	request := validRequest()
	request.Protocol = ProtocolICMP
	request.Port = 9100

	result, err := engine.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if !result.OK {
		t.Errorf("OK = false, want true (%s)", result.Error)
	}
	if result.RTT != time.Millisecond {
		t.Errorf("RTT = %s, want the echo measurement rather than a port probe", result.RTT)
	}
}

func TestTheBoundsTravelWithTheRequest(t *testing.T) {
	t.Parallel()
	runner := &engineRunner{result: echoResult}
	engine := newEngine(t, runner, nil)

	if _, err := engine.Run(context.Background(), validRequest()); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.calls) != 1 {
		t.Fatalf("the runner was called %d times, want 1", len(runner.calls))
	}
	call := runner.calls[0]
	if len(call) != 4 {
		t.Fatalf("the agent was called with %v, want a namespace, a binary, a mode and a request", call)
	}
	if call[0] != "nf-client0" {
		t.Errorf("the agent ran in %q, want the requested namespace", call[0])
	}
	if call[1] != "/bin/true" {
		t.Errorf("the agent binary = %q, want the executable path rather than a bare mode name", call[1])
	}
	if call[2] != probeMode {
		t.Errorf("the agent mode = %q, want %q", call[2], probeMode)
	}

	var payload AgentPayload
	if err := json.Unmarshal([]byte(call[3]), &payload); err != nil {
		t.Fatalf("decode the payload: %v", err)
	}
	if payload.Protocol != ProtocolTCP {
		t.Errorf("the payload protocol = %q, want %q", payload.Protocol, ProtocolTCP)
	}
	if payload.MaxResponseSize != 512 {
		t.Errorf("MaxResponseSize = %d, want the engine's 512", payload.MaxResponseSize)
	}
	if payload.MaxPayload != 256 {
		t.Errorf("MaxPayload = %d, want the engine's 256", payload.MaxPayload)
	}
}

func TestConcurrencyIsBounded(t *testing.T) {
	t.Parallel()
	runner := &engineRunner{result: echoResult, delay: 80 * time.Millisecond}
	engine := newEngine(t, runner, nil)

	var wg sync.WaitGroup
	results := make([]error, 6)
	for i := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request := validRequest()
			request.Experiment = fmt.Sprintf("probe-%d", i)
			_, results[i] = engine.Run(context.Background(), request)
		}()
	}
	wg.Wait()

	refused := 0
	for _, err := range results {
		if err != nil {
			refused++
			if !apperr.Is(err, apperr.KindLimitExceeded) {
				t.Errorf("Run() = %v, want a limit error", err)
			}
		}
	}
	if refused == 0 {
		t.Error("every probe was admitted, want the ones past MaxConcurrent refused")
	}
	if runner.peak > 2 {
		t.Errorf("%d probes ran at once, want at most the 2 the limits allow", runner.peak)
	}
}

func TestATimeoutIsAFailure(t *testing.T) {
	t.Parallel()
	runner := &engineRunner{
		result: echoResult,
		delay:  2300 * time.Millisecond,
	}
	limits := testLimits()
	limits.MaxDuration = 200 * time.Millisecond
	engine, err := NewEngine(limits, fakeScope(), runner, nil, "/bin/true")
	if err != nil {
		t.Fatalf("NewEngine() = %v, want nil", err)
	}
	request := validRequest()
	request.Timeout = 200 * time.Millisecond

	result, err := engine.Run(context.Background(), request)
	if err == nil {
		t.Fatalf("Run() = %+v, want a timeout", result)
	}
	if !strings.Contains(err.Error(), "took") {
		t.Errorf("Run() = %q, want it to report that the agent took too long", err)
	}
}

func TestAFailureTheAgentReportsIsAResult(t *testing.T) {
	t.Parallel()
	runner := &engineRunner{result: func(payload AgentPayload) Result {
		return Result{
			Namespace: payload.Namespace,
			Target:    payload.Target,
			Protocol:  payload.Protocol,
			Failure:   FailureRefused,
			Error:     "connect: connection refused",
		}
	}}
	engine := newEngine(t, runner, nil)

	result, err := engine.Run(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("Run() = %v, want nil: a refused connection is a measurement", err)
	}
	if result.OK {
		t.Error("OK = true, want false")
	}
	if result.Failure != FailureRefused {
		t.Errorf("Failure = %q, want %q", result.Failure, FailureRefused)
	}
	if len(engine.Results()) != 1 {
		t.Error("the failure was not recorded")
	}
}

func TestAnAnswerForTheWrongProtocolIsRefused(t *testing.T) {
	t.Parallel()
	runner := &engineRunner{result: func(payload AgentPayload) Result {
		result := echoResult(payload)
		result.Protocol = ProtocolHTTP
		return result
	}}
	engine := newEngine(t, runner, nil)

	if _, err := engine.Run(context.Background(), validRequest()); err == nil {
		t.Error("Run() = nil error, want a refusal for an answer about another protocol")
	}
	results := engine.Results()
	if len(results) != 1 {
		t.Fatalf("Results() = %d results, want the refusal recorded once", len(results))
	}

	if results[0].OK {
		t.Error("the recorded result is OK, want a failure")
	}
	if results[0].Protocol != ProtocolTCP {
		t.Errorf("the recorded protocol = %q, want %q", results[0].Protocol, ProtocolTCP)
	}
}

func TestAnUnreadableAnswerIsRefused(t *testing.T) {
	t.Parallel()
	runner := &engineRunner{stdout: "this is not json", code: 0}
	engine := newEngine(t, runner, nil)

	if _, err := engine.Run(context.Background(), validRequest()); err == nil {
		t.Error("Run() = nil error, want a refusal for an unreadable answer")
	}
}

func TestAFailingAgentIsALaboratoryProblem(t *testing.T) {
	t.Parallel()
	runner := &engineRunner{stderr: "the agent could not start", code: 1}
	engine := newEngine(t, runner, nil)

	_, err := engine.Run(context.Background(), validRequest())
	if err == nil {
		t.Fatal("Run() = nil error, want the agent failure reported")
	}
	if !strings.Contains(err.Error(), "exit status 1") {
		t.Errorf("Run() = %q, want it to report the exit status", err)
	}
}

func TestTheResultStoreIsBounded(t *testing.T) {
	t.Parallel()
	store, err := NewStore(4)
	if err != nil {
		t.Fatalf("NewStore(4) = %v, want nil", err)
	}
	for i := range 10 {
		store.Add(Result{Target: fmt.Sprintf("10.60.0.%d", i), At: time.Now()})
	}
	results := store.Results()
	if len(results) != 4 {
		t.Fatalf("Results() = %d results, want the 4 the store keeps", len(results))
	}

	if results[len(results)-1].Target != "10.60.0.9" {
		t.Errorf("the newest result is %q, want 10.60.0.9", results[len(results)-1].Target)
	}
	if results[0].Target != "10.60.0.6" {
		t.Errorf("the oldest kept result is %q, want 10.60.0.6", results[0].Target)
	}
}

func TestTheStoreHandsOutACopy(t *testing.T) {
	t.Parallel()
	store, err := NewStore(4)
	if err != nil {
		t.Fatalf("NewStore(4) = %v, want nil", err)
	}
	store.Add(Result{Target: "10.60.0.2", Protocol: ProtocolTCP})
	results := store.Results()
	results[0].Target = "203.0.113.1"
	results[0].Protocol = ProtocolHTTP
	if again := store.Results(); again[0].Target != "10.60.0.2" || again[0].Protocol != ProtocolTCP {
		t.Errorf("the stored result changed to %+v, want it left alone", again[0])
	}
}

func TestAnEchoProbeNeedsAnEchoMeasurer(t *testing.T) {
	t.Parallel()
	runner := &engineRunner{result: echoResult}
	engine := newEngine(t, runner, nil)

	request := validRequest()
	request.Protocol = ProtocolICMP
	request.Port = 0
	result, err := engine.Run(context.Background(), request)
	if err == nil {
		t.Fatal("Run() = nil error, want the laboratory problem reported")
	}
	if !strings.Contains(err.Error(), "echo") {
		t.Errorf("Run() = %q, want it to say the echo probe could not be measured", err)
	}
	if result.OK {
		t.Error("OK = true, want false when the laboratory cannot measure an echo probe")
	}
	if !strings.Contains(result.Error, "echo") {
		t.Errorf("Error = %q, want it to say the echo probe could not be measured", result.Error)
	}
	if runner.count() != 0 {
		t.Errorf("the runner was called %d times, want 0: an echo probe never reaches the agent", runner.count())
	}
}

func TestAnEchoProbeUsesTheMeasurer(t *testing.T) {
	t.Parallel()
	echo := &fakeEchor{measurement: Measurement{
		Sent: 3, Received: 3, AverageRTT: 1500 * time.Microsecond,
	}}
	engine := newEngine(t, &engineRunner{result: echoResult}, echo)

	request := validRequest()
	request.Protocol = ProtocolICMP
	request.Port = 0
	request.Payload = 64

	result, err := engine.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if !result.OK {
		t.Errorf("OK = false, want true (%s)", result.Error)
	}
	if result.RTT != 1500*time.Microsecond {
		t.Errorf("RTT = %s, want the measured 1.5ms", result.RTT)
	}
	if result.ResponseSize != 3 {
		t.Errorf("ResponseSize = %d, want the 3 replies received", result.ResponseSize)
	}
	if !strings.Contains(result.Detail, "3 of 3") {
		t.Errorf("Detail = %q, want it to report the reply count", result.Detail)
	}
	if echo.calls != 1 {
		t.Errorf("the measurer was called %d times, want 1", echo.calls)
	}
	if echo.namespace != "nf-client0" || echo.target != "10.60.0.2" {
		t.Errorf("the measurer measured %s in %s, want 10.60.0.2 in nf-client0", echo.target, echo.namespace)
	}
}

func TestAnEchoProbeWithNoReplyIsAFailure(t *testing.T) {
	t.Parallel()
	echo := &fakeEchor{measurement: Measurement{Sent: 3, Received: 0}}
	engine := newEngine(t, &engineRunner{result: echoResult}, echo)

	request := validRequest()
	request.Protocol = ProtocolICMP
	request.Port = 0

	result, err := engine.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("Run() = %v, want nil: a silent path is a measurement", err)
	}
	if result.OK {
		t.Error("OK = true, want false when nothing replied")
	}
	if !strings.Contains(result.Error, "no reply") {
		t.Errorf("Error = %q, want it to say that nothing replied", result.Error)
	}
	if !strings.Contains(result.Detail, "0 of 3") {
		t.Errorf("Detail = %q, want it to report the reply count", result.Detail)
	}
}

func syscallErr(errno syscall.Errno) error {
	return &net.OpError{
		Op:  "dial",
		Net: "tcp",
		Err: os.NewSyscallError("connect", errno),
	}
}

type fakeEchor struct {
	measurement Measurement
	err         error
	calls       int
	namespace   string
	target      string
}

func (e *fakeEchor) Measure(_ context.Context, namespace, target string, _ Options) (Measurement, error) {
	e.calls++
	e.namespace = namespace
	e.target = target
	return e.measurement, e.err
}

func TestEveryProtocolNamesALayer(t *testing.T) {
	t.Parallel()
	want := map[Protocol]string{
		ProtocolICMP: "network",
		ProtocolTCP:  "transport",
		ProtocolUDP:  "transport",
		ProtocolDNS:  "dns",
		ProtocolTLS:  "tls",
		ProtocolHTTP: "http",
	}
	for _, protocol := range Protocols() {
		if got := protocol.Layer(); got != want[protocol] {
			t.Errorf("Protocol(%s).Layer() = %q, want %q", protocol, got, want[protocol])
		}
		if !protocol.Valid() {
			t.Errorf("Protocol(%s).Valid() = false, want true", protocol)
		}
	}
	if Protocol("smtp").Valid() {
		t.Error("Protocol(smtp).Valid() = true, want false")
	}
	if got := Protocol("smtp").Layer(); got != "unknown" {
		t.Errorf("Protocol(smtp).Layer() = %q, want %q", got, "unknown")
	}
}

func TestFailuresAreClassified(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want Failure
	}{
		{"a timeout", context.DeadlineExceeded, FailureTimeout},
		{"a cancelled context", context.Canceled, FailureTimeout},
		{"a refused connection", syscallErr(syscall.ECONNREFUSED), FailureRefused},
		{"an unreachable host", syscallErr(syscall.EHOSTUNREACH), FailureUnreachable},
		{"an unreachable network", syscallErr(syscall.ENETUNREACH), FailureUnreachable},
		{"no route to host", syscallErr(syscall.EHOSTUNREACH), FailureUnreachable},
		{"a reset connection", syscallErr(syscall.ECONNRESET), FailureReset},
		{"a name that does not resolve", &resolverError{code: 3}, FailureDNS},
		{"a resolver failure", &net.DNSError{Err: "no such host", IsNotFound: true}, FailureDNS},
		{"a certificate that does not verify", &tlsError{err: errors.New("x509: certificate signed by unknown authority")}, FailureTLS},
		{"an unexpected status", &statusError{status: 503}, FailureHTTP},
		{"a refusal before anything was sent", apperr.InvalidArgument("test", "no", nil), FailureMalformed},
		{"something unrecognised", errors.New("the moon is in the wrong phase"), FailureUnknown},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := Classify(test.err); got != test.want {
				t.Errorf("Classify(%v) = %q, want %q", test.err, got, test.want)
			}
		})
	}
}
