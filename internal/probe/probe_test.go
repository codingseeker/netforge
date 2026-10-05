package probe

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/netforge/netforge/internal/apperr"
)

type fakeRunner struct {
	mu      sync.Mutex
	stdout  string
	stderr  string
	code    int
	err     error
	argv    []string
	delay   time.Duration
	failing error
}

func (f *fakeRunner) Run(namespace string, argv ...string) (string, string, int, error) {
	f.mu.Lock()
	f.argv = append([]string(nil), argv...)
	delay, stdout, stderr, code, err := f.delay, f.stdout, f.stderr, f.code, f.err
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	return stdout, stderr, code, err
}

func (f *fakeRunner) command() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.argv, " ")
}

const healthyReport = `PING 10.42.0.6 (10.42.0.6) 1400(1428) bytes of data.
1408 bytes from 10.42.0.6: icmp_seq=1 ttl=63 time=0.181 ms
1408 bytes from 10.42.0.6: icmp_seq=2 ttl=63 time=0.103 ms
1408 bytes from 10.42.0.6: icmp_seq=3 ttl=63 time=0.079 ms

--- 10.42.0.6 ping statistics ---
3 packets transmitted, 3 received, 0% packet loss, time 200ms
rtt min/avg/max/mdev = 0.079/0.121/0.181/0.044 ms
`

func newMeasurer(t *testing.T, limits Limits, runner Runner) *Measurer {
	t.Helper()
	measurer, err := New(limits, runner)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return measurer
}

func TestMeasureReadsWhatCameBack(t *testing.T) {
	runner := &fakeRunner{stdout: healthyReport}
	measurer := newMeasurer(t, Limits{}, runner)

	options := Options{Count: 3, Interval: 10 * time.Millisecond, Size: 1400, Timeout: time.Second, Deadline: 5 * time.Second}
	measurement, err := measurer.Measure(context.Background(), "nf-client0-tc", "10.42.0.6", options)
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if measurement.Sent != 3 || measurement.Received != 3 {
		t.Errorf("got %d sent and %d received, want 3 and 3", measurement.Sent, measurement.Received)
	}
	if measurement.Loss != 0 {
		t.Errorf("got %v%% loss, want none", measurement.Loss)
	}
	if measurement.Bytes != 3*1408 {
		t.Errorf("got %d bytes, want %d", measurement.Bytes, 3*1408)
	}
	if measurement.MinRTT != 79*time.Microsecond {
		t.Errorf("got a minimum of %s, want 79µs", measurement.MinRTT)
	}
	if measurement.MaxRTT != 181*time.Microsecond {
		t.Errorf("got a maximum of %s, want 181µs", measurement.MaxRTT)
	}
	if measurement.DeviationRTT != 44*time.Microsecond {
		t.Errorf("got a deviation of %s, want 44µs", measurement.DeviationRTT)
	}
	if !measurement.Replied() {
		t.Error("a measurement with replies must report that it replied")
	}
	if got := runner.command(); got != "ping -n -c 3 -i 0.010 -s 1400 -W 1 -w 5 10.42.0.6" {
		t.Errorf("got %q", got)
	}
}

func TestMeasureAcceptsLossAsAnAnswer(t *testing.T) {

	report := `PING 10.42.0.6 (10.42.0.6) 1400(1428) bytes of data.

--- 10.42.0.6 ping statistics ---
20 packets transmitted, 8 received, 60% packet loss, time 1905ms
rtt min/avg/max/mdev = 50.101/50.180/50.301/0.062 ms
`
	runner := &fakeRunner{stdout: report, code: 1}
	measurer := newMeasurer(t, Limits{}, runner)

	measurement, err := measurer.Measure(context.Background(), "nf-client0-tc", "10.42.0.6",
		Options{Count: 20, Interval: 10 * time.Millisecond, Size: 1400, Timeout: time.Second, Deadline: 5 * time.Second})
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if measurement.Loss != 60 {
		t.Errorf("got %v%% loss, want 60%%", measurement.Loss)
	}
	if measurement.Received != 8 || measurement.Sent != 20 {
		t.Errorf("got %d of %d, want 8 of 20", measurement.Received, measurement.Sent)
	}
	if measurement.Bytes != 8*1408 {
		t.Errorf("got %d bytes, want %d", measurement.Bytes, 8*1408)
	}
}

func TestMeasureFailsClosedOnAnUnreadableReport(t *testing.T) {
	cases := map[string]struct {
		stdout string
		code   int
		stderr string
	}{
		"an empty report":            {stdout: "", code: 1},
		"a report with no summary":   {stdout: "PING 10.42.0.6 (10.42.0.6) 1400(1428) bytes of data.\n", code: 1},
		"a report from another tool": {stdout: "Cannot find device \"eth9\"\n", code: 2, stderr: "Cannot find device"},
		"a report that claims more replies than requests": {
			stdout: "3 packets transmitted, 9 received, 0% packet loss, time 2ms\n",
			code:   0,
		},
		"replies without round trip times": {
			stdout: "3 packets transmitted, 3 received, 0% packet loss, time 2ms\n",
			code:   0,
		},
		"a probe that could not start": {
			stdout: "ping: connect: Network is unreachable\n",
			code:   2,
			stderr: "ping: connect: Network is unreachable",
		},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			runner := &fakeRunner{stdout: test.stdout, code: test.code, stderr: test.stderr}
			measurer := newMeasurer(t, Limits{}, runner)
			measurement, err := measurer.Measure(context.Background(), "nf-client0-tc", "10.42.0.6", DefaultOptions())
			if err == nil {
				t.Fatalf("an unreadable report was reported as %+v", measurement)
			}
			if measurement.Replied() {
				t.Error("a failed measurement must not claim replies")
			}
		})
	}
}

func TestMeasureRefusesToOutrunTheLimits(t *testing.T) {
	runner := &fakeRunner{stdout: healthyReport}
	measurer := newMeasurer(t, Limits{
		MaxProbes: 10, MaxRate: 50, MaxSize: 512, MaxDuration: 5 * time.Second, MaxConcurrent: 1,
	}, runner)

	cases := map[string]Options{
		"too many packets":  {Count: 11, Interval: 10 * time.Millisecond, Size: 100, Timeout: time.Second, Deadline: 2 * time.Second},
		"too fast":          {Count: 5, Interval: time.Millisecond, Size: 100, Timeout: time.Second, Deadline: 2 * time.Second},
		"too large":         {Count: 5, Interval: 10 * time.Millisecond, Size: 513, Timeout: time.Second, Deadline: 2 * time.Second},
		"deadline too long": {Count: 5, Interval: 10 * time.Millisecond, Size: 100, Timeout: time.Second, Deadline: 6 * time.Second},
		"deadline below the timeout": {
			Count: 5, Interval: 10 * time.Millisecond, Size: 100, Timeout: 2 * time.Second, Deadline: time.Second,
		},
		"interval below the floor": {Count: 5, Interval: 100 * time.Microsecond, Size: 100, Timeout: time.Second, Deadline: 2 * time.Second},
		"no timeout":               {Count: 5, Interval: 10 * time.Millisecond, Size: 100, Timeout: -time.Second, Deadline: 2 * time.Second},
	}
	for name, options := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := measurer.Measure(context.Background(), "nf-client0-tc", "10.42.0.6", options); !apperr.IsKind(err, apperr.KindLimitExceeded) &&
				!apperr.IsKind(err, apperr.KindInvalidArgument) {
				t.Fatalf("got %v, want the measurement to be refused", err)
			}
			if runner.command() != "" {
				t.Error("a refused measurement still sent packets")
			}
		})
	}
}

func TestMeasureRefusesATargetThatIsNotAnAddress(t *testing.T) {
	runner := &fakeRunner{stdout: healthyReport}
	measurer := newMeasurer(t, Limits{}, runner)

	for _, target := range []string{"", "  ", "10.42.0.6; rm -rf /", "10.42.0.6 -c 1", "$(id)"} {
		if _, err := measurer.Measure(context.Background(), "nf-client0-tc", target, DefaultOptions()); err == nil {
			t.Errorf("the target %q was accepted", target)
		}
	}
	if runner.command() != "" {
		t.Error("a refused target still ran a command")
	}
}

func TestMeasureRefusesMoreMeasurementsThanTheLimit(t *testing.T) {

	blocking := &fakeRunner{stdout: healthyReport, delay: 50 * time.Millisecond}
	measurer := newMeasurer(t, Limits{MaxConcurrent: 1}, blocking)

	var started sync.WaitGroup
	started.Add(1)
	go func() {
		defer started.Done()
		_, _ = measurer.Measure(context.Background(), "nf-client0-tc", "10.42.0.6", DefaultOptions())
	}()
	time.Sleep(5 * time.Millisecond)
	_, err := measurer.Measure(context.Background(), "nf-client0-tc", "10.42.0.6", DefaultOptions())
	started.Wait()
	if !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Fatalf("got %v, want the concurrent limit to be enforced", err)
	}
}

func TestMeasureRefusesACancelledContext(t *testing.T) {
	runner := &fakeRunner{stdout: healthyReport}
	measurer := newMeasurer(t, Limits{}, runner)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := measurer.Measure(ctx, "nf-client0-tc", "10.42.0.6", DefaultOptions()); !apperr.IsKind(err, apperr.KindCanceled) {
		t.Fatalf("got %v, want a cancelled error", err)
	}
	if runner.command() != "" {
		t.Error("a cancelled measurement still sent packets")
	}
}

func TestMeasureReportsATransportFailure(t *testing.T) {
	runner := &fakeRunner{err: fmt.Errorf("the laboratory is closed")}
	measurer := newMeasurer(t, Limits{}, runner)
	if _, err := measurer.Measure(context.Background(), "nf-client0-tc", "10.42.0.6", DefaultOptions()); err == nil {
		t.Fatal("a transport failure was reported as a measurement")
	}
}

func TestMeasureFillsInTheDefaults(t *testing.T) {
	runner := &fakeRunner{stdout: healthyReport}
	measurer := newMeasurer(t, Limits{}, runner)
	measurement, err := measurer.Measure(context.Background(), "nf-client0-tc", "10.42.0.6", Options{})
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if measurement.Options.Count != DefaultOptions().Count {
		t.Errorf("got %d packets, want the default %d", measurement.Options.Count, DefaultOptions().Count)
	}
	if measurement.Options.Size != DefaultOptions().Size {
		t.Errorf("got a payload of %d, want the default %d", measurement.Options.Size, DefaultOptions().Size)
	}
}

func TestGoodputFollowsTheBytesThatArrived(t *testing.T) {
	report := `PING 10.42.0.6 (10.42.0.6) 100(128) bytes of data.

--- 10.42.0.6 ping statistics ---
10 packets transmitted, 10 received, 0% packet loss, time 9ms
rtt min/avg/max/mdev = 1.001/1.100/1.300/0.100 ms
`
	runner := &fakeRunner{stdout: report, delay: 100 * time.Millisecond}
	measurer := newMeasurer(t, Limits{}, runner)
	measurement, err := measurer.Measure(context.Background(), "nf-client0-tc", "10.42.0.6",
		Options{Count: 10, Interval: 10 * time.Millisecond, Size: 100, Timeout: time.Second, Deadline: 5 * time.Second})
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if measurement.Bytes != 10*108 {
		t.Errorf("got %d bytes, want %d", measurement.Bytes, 10*108)
	}

	expected := float64(measurement.Bytes*8) / measurement.Duration.Seconds()
	difference := math.Abs(float64(measurement.Goodput)-expected) / expected
	if difference > 0.2 {
		t.Errorf("got a goodput of %d bits per second, the bytes and the time say %.0f",
			measurement.Goodput, expected)
	}
}

func TestParseReadsAReportWithoutReplies(t *testing.T) {
	report := `
--- 10.42.0.6 ping statistics ---
5 packets transmitted, 0 received, 100% packet loss, time 4005ms
`
	measurement, err := parse(report, "nf-client0-tc", "10.42.0.6", DefaultOptions(), 4*time.Second)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if measurement.Replied() {
		t.Error("a report with no replies must not claim any")
	}
	if measurement.Goodput != 0 {
		t.Errorf("got a goodput of %d, want none when nothing arrived", measurement.Goodput)
	}
	if measurement.MinRTT != 0 {
		t.Errorf("got a minimum of %s, want zero when nothing arrived", measurement.MinRTT)
	}
	if measurement.Loss != 100 {
		t.Errorf("got %v%% loss, want 100%%", measurement.Loss)
	}
}

func TestParseReadsTheDuplicateClause(t *testing.T) {
	report := `PING 10.42.0.6 (10.42.0.6) 1400(1428) bytes of data.
1408 bytes from 10.42.0.6: icmp_seq=1 ttl=63 time=0.104 ms
1408 bytes from 10.42.0.6: icmp_seq=1 ttl=63 time=0.106 ms (DUP!)
1408 bytes from 10.42.0.6: icmp_seq=2 ttl=63 time=0.130 ms
1408 bytes from 10.42.0.6: icmp_seq=3 ttl=63 time=0.056 ms
1408 bytes from 10.42.0.6: icmp_seq=3 ttl=63 time=0.057 ms (DUP!)

--- 10.42.0.6 ping statistics ---
4 packets transmitted, 4 received, +2 duplicates, 0% packet loss, time 101ms
rtt min/avg/max/mdev = 0.056/0.086/0.130/0.031 ms, pipe 4
`
	measurement, err := parse(report, "nf-client0-tc", "10.42.0.6", DefaultOptions(), 101*time.Millisecond)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if measurement.Duplicates != 2 {
		t.Errorf("got %d duplicate replies, want 2", measurement.Duplicates)
	}

	if measurement.Received != 4 || measurement.Sent != 4 {
		t.Errorf("got %d replies for %d requests, want 4 of 4", measurement.Received, measurement.Sent)
	}
	if measurement.Loss != 0 {
		t.Errorf("got %v%% loss, duplication should lose nothing", measurement.Loss)
	}
}

func TestParseReadsTheReorderEvidence(t *testing.T) {
	report := `PING 10.42.0.6 (10.42.0.6) 1400(1428) bytes of data.
1408 bytes from 10.42.0.6: icmp_seq=2 ttl=63 time=29.9 ms
1408 bytes from 10.42.0.6: icmp_seq=4 ttl=63 time=7.87 ms
1408 bytes from 10.42.0.6: icmp_seq=1 ttl=63 time=80.3 ms
1408 bytes from 10.42.0.6: icmp_seq=3 ttl=63 time=58.9 ms

--- 10.42.0.6 ping statistics ---
4 packets transmitted, 4 received, 0% packet loss, time 97ms
rtt min/avg/max/mdev = 7.87/44.2/80.3/28.6 ms, pipe 4
`
	measurement, err := parse(report, "nf-client0-tc", "10.42.0.6", DefaultOptions(), 97*time.Millisecond)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if measurement.OutOfOrder != 2 {
		t.Errorf("got %d out of order replies, want 2", measurement.OutOfOrder)
	}
	if measurement.Duplicates != 0 {
		t.Errorf("got %d duplicates, a reordered report has none", measurement.Duplicates)
	}

	if measurement.MinRTT != 7870*time.Microsecond {
		t.Errorf("got a minimum of %s, want 7.87ms", measurement.MinRTT)
	}
}

func TestParseReportsNoReorderOnAnOrderedPath(t *testing.T) {
	report := `PING 10.42.0.6 (10.42.0.6) 1400(1428) bytes of data.
1408 bytes from 10.42.0.6: icmp_seq=1 ttl=63 time=0.104 ms
1408 bytes from 10.42.0.6: icmp_seq=2 ttl=63 time=0.106 ms
1408 bytes from 10.42.0.6: icmp_seq=3 ttl=63 time=0.098 ms

--- 10.42.0.6 ping statistics ---
3 packets transmitted, 3 received, 0% packet loss, time 20ms
rtt min/avg/max/mdev = 0.098/0.103/0.106/0.003 ms
`
	measurement, err := parse(report, "nf-client0-tc", "10.42.0.6", DefaultOptions(), 20*time.Millisecond)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if measurement.OutOfOrder != 0 {
		t.Errorf("got %d out of order replies on an ordered path", measurement.OutOfOrder)
	}
}

func TestWholeSecondsRoundsUp(t *testing.T) {
	cases := map[time.Duration]int{
		0:                       1,
		500 * time.Millisecond:  1,
		time.Second:             1,
		1500 * time.Millisecond: 2,
		10 * time.Second:        10,
	}
	for given, want := range cases {
		if got := wholeSeconds(given); got != want {
			t.Errorf("%s rounded to %d, want %d", given, got, want)
		}
	}
}

func TestLimitsCheck(t *testing.T) {
	if _, err := DefaultLimits().check(); err != nil {
		t.Fatalf("the default limits must be valid: %v", err)
	}
	if _, err := (Limits{MaxSize: 100_000}).check(); !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Errorf("got %v, want an oversized payload to be refused", err)
	}
	for _, limits := range []Limits{{MaxProbes: -1}, {MaxRate: -1}, {MaxDuration: -1}, {MaxConcurrent: -1}} {
		if _, err := limits.check(); err == nil {
			t.Errorf("the limits %+v were accepted", limits)
		}
	}
}

func TestParseReadsAnErrorClause(t *testing.T) {
	report := `PING 10.42.0.6 (10.42.0.6) 1400(1428) bytes of data.
From 10.42.0.2 icmp_seq=1 Destination Host Unreachable
From 10.42.0.2 icmp_seq=2 Destination Host Unreachable

--- 10.42.0.6 ping statistics ---
174 packets transmitted, 0 received, +4 errors, 100% packet loss, time 3075ms
pipe 174
`
	measurement, err := parse(report, "nf-client0-tc", "10.42.0.6", DefaultOptions(), 3075*time.Millisecond)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if measurement.Sent != 174 || measurement.Received != 0 {
		t.Errorf("got %d sent and %d received, want 174 and 0", measurement.Sent, measurement.Received)
	}
	if measurement.Errors != 4 {
		t.Errorf("got %d errors, want 4", measurement.Errors)
	}
	if measurement.Loss != 100 {
		t.Errorf("got %v%% loss, want 100%%", measurement.Loss)
	}
	if measurement.Replied() {
		t.Error("a broken path must not report replies")
	}

	if measurement.MinRTT != 0 || measurement.Goodput != 0 {
		t.Errorf("got %s and %d bit/s, want no timing and no goodput", measurement.MinRTT, measurement.Goodput)
	}
}

func TestParseReadsBothClausesInEitherOrder(t *testing.T) {
	for _, summary := range []string{
		"10 packets transmitted, 9 received, +2 duplicates, +1 errors, 10% packet loss, time 40ms",
		"10 packets transmitted, 9 received, +1 errors, +2 duplicates, 10% packet loss, time 40ms",
	} {
		report := "--- 10.42.0.6 ping statistics ---\n" + summary + "\n" +
			"rtt min/avg/max/mdev = 1.0/2.0/3.0/0.5 ms\n"
		measurement, err := parse(report, "nf-client0-tc", "10.42.0.6", DefaultOptions(), 40*time.Millisecond)
		if err != nil {
			t.Fatalf("parse(%q): %v", summary, err)
		}
		if measurement.Duplicates != 2 || measurement.Errors != 1 {
			t.Errorf("parse(%q) got %d duplicates and %d errors, want 2 and 1",
				summary, measurement.Duplicates, measurement.Errors)
		}
		if measurement.Loss != 10 {
			t.Errorf("parse(%q) got %v%% loss, want 10%%", summary, measurement.Loss)
		}
	}
}
