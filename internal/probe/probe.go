// Package probe measures a laboratory from real traffic.
//
// A measurement is never a configured value: it is what came back from the
// wire. Every measurement is bounded before a single packet is sent, so a
// misconfigured or hostile experiment cannot turn the laboratory into a packet
// generator, and an unparsable answer is an error rather than a flattering
// number.
package probe

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/netforge/netforge/internal/apperr"
)

// Runner executes a command inside a named laboratory namespace and reports
// its exit status. The namespace layer owns process creation.
type Runner interface {
	Run(namespace string, argv ...string) (stdout, stderr string, code int, err error)
}

// Limits bound what a measurement may cost.
type Limits struct {
	// MaxProbes is the number of packets one measurement may send.
	MaxProbes int `json:"max_probes"`
	// MaxRate is the fastest packet rate one measurement may send, in packets
	// per second. This is the packet rate limit: a measurement is the only
	// part of the laboratory that originates unbounded traffic, so this is
	// where that limit belongs.
	MaxRate int `json:"max_rate"`
	// MaxSize is the largest payload one measurement may send, in bytes.
	MaxSize int `json:"max_size"`
	// MaxDuration is the longest wall clock time one measurement may take.
	MaxDuration time.Duration `json:"max_duration"`
	// MaxConcurrent is the number of measurements that may run at once.
	MaxConcurrent int `json:"max_concurrent"`
	// MaxResponseSize is the largest answer one measurement may read, in
	// bytes. An answer longer than this is cut short and reported as
	// truncated rather than buffered, so a hostile or broken peer cannot make
	// the probe allocate without bound.
	MaxResponseSize int `json:"max_response_size"`
	// MaxStoredResults is the number of results the probe engine keeps. Older
	// results are dropped, so a long laboratory cannot grow the process
	// without limit.
	MaxStoredResults int `json:"max_stored_results"`
	// MaxNameLength is the longest experiment name a result may carry.
	MaxNameLength int `json:"max_name_length"`
}

// DefaultLimits returns the limits used when a laboratory does not configure
// any.
func DefaultLimits() Limits {
	return Limits{
		MaxProbes:        100,
		MaxRate:          200,
		MaxSize:          1400,
		MaxDuration:      30 * time.Second,
		MaxConcurrent:    4,
		MaxResponseSize:  65_507,
		MaxStoredResults: 1024,
		MaxNameLength:    64,
	}
}

// defaultRequestTimeout is how long a single protocol probe waits when the
// request does not say.
const defaultRequestTimeout = 5 * time.Second

// WithDefaults fills in every unset limit.
func (l Limits) WithDefaults() Limits {
	defaults := DefaultLimits()
	if l.MaxProbes == 0 {
		l.MaxProbes = defaults.MaxProbes
	}
	if l.MaxRate == 0 {
		l.MaxRate = defaults.MaxRate
	}
	if l.MaxSize == 0 {
		l.MaxSize = defaults.MaxSize
	}
	if l.MaxDuration == 0 {
		l.MaxDuration = defaults.MaxDuration
	}
	if l.MaxConcurrent == 0 {
		l.MaxConcurrent = defaults.MaxConcurrent
	}
	if l.MaxResponseSize == 0 {
		l.MaxResponseSize = defaults.MaxResponseSize
	}
	if l.MaxStoredResults == 0 {
		l.MaxStoredResults = defaults.MaxStoredResults
	}
	if l.MaxNameLength == 0 {
		l.MaxNameLength = defaults.MaxNameLength
	}
	return l
}

func (l Limits) check() (Limits, error) {
	const op = "probe.Limits.check"
	l = l.WithDefaults()
	if l.MaxProbes <= 0 {
		return Limits{}, apperr.Validation(op, "the max_probes limit must be positive", nil)
	}
	if l.MaxRate <= 0 {
		return Limits{}, apperr.Validation(op, "the max_rate limit must be positive", nil)
	}
	if l.MaxSize <= 0 {
		return Limits{}, apperr.Validation(op, "the max_size limit must be positive", nil)
	}
	if l.MaxSize > 65_507 {
		return Limits{}, apperr.LimitExceeded(op, fmt.Sprintf(
			"the max_size limit of %d is larger than an IP packet", l.MaxSize), nil)
	}
	if l.MaxDuration <= 0 {
		return Limits{}, apperr.Validation(op, "the max_duration limit must be positive", nil)
	}
	if l.MaxConcurrent <= 0 {
		return Limits{}, apperr.Validation(op, "the max_concurrent limit must be positive", nil)
	}
	if l.MaxResponseSize <= 0 {
		return Limits{}, apperr.Validation(op, "the max_response_size limit must be positive", nil)
	}
	if l.MaxResponseSize > 65_507 {
		return Limits{}, apperr.LimitExceeded(op, fmt.Sprintf(
			"the max_response_size limit of %d is larger than an IP packet", l.MaxResponseSize), nil)
	}
	if l.MaxStoredResults <= 0 {
		return Limits{}, apperr.Validation(op, "the max_stored_results limit must be positive", nil)
	}
	if l.MaxStoredResults > 1_000_000 {
		return Limits{}, apperr.LimitExceeded(op, fmt.Sprintf(
			"the max_stored_results limit of %d is larger than this build keeps", l.MaxStoredResults), nil)
	}
	if l.MaxNameLength <= 0 {
		return Limits{}, apperr.Validation(op, "the max_name_length limit must be positive", nil)
	}
	if l.MaxNameLength > 255 {
		return Limits{}, apperr.LimitExceeded(op, fmt.Sprintf(
			"the max_name_length limit of %d is longer than a DNS label", l.MaxNameLength), nil)
	}
	return l, nil
}

// Options describe one measurement.
type Options struct {
	// Count is the number of echo requests to send.
	Count int
	// Interval is the wait between two requests. It is bounded by MaxRate.
	Interval time.Duration
	// Size is the payload size in bytes.
	Size int
	// Timeout is how long a single request may wait for its reply.
	Timeout time.Duration
	// Deadline is the overall time budget of the measurement.
	Deadline time.Duration
}

// DefaultOptions returns a small, quick measurement.
func DefaultOptions() Options {
	return Options{
		Count:    10,
		Interval: 10 * time.Millisecond,
		Size:     1400,
		Timeout:  time.Second,
		Deadline: 10 * time.Second,
	}
}

// Measurement is what actually came back from the wire.
type Measurement struct {
	// Target is the address that was probed.
	Target string `json:"target"`
	// Namespace is the laboratory namespace the probe ran in.
	Namespace string `json:"namespace"`
	// Sent and Received are the packet counts the kernel reported.
	Sent     int `json:"sent"`
	Received int `json:"received"`
	// Loss is the measured loss on a scale of 0 to 100.
	Loss float64 `json:"loss"`
	// MinRTT, AverageRTT, MaxRTT and DeviationRTT are measured over the replies
	// that came back. They are zero when nothing came back, which Sent and
	// Received distinguish from a genuinely instantaneous path.
	MinRTT       time.Duration `json:"min_rtt"`
	AverageRTT   time.Duration `json:"average_rtt"`
	MaxRTT       time.Duration `json:"max_rtt"`
	DeviationRTT time.Duration `json:"deviation_rtt"`
	// Duplicates is the number of extra copies of replies that came back. A
	// duplication fault shows up here rather than in Received, which stays the
	// number of requests that were answered.
	Duplicates int `json:"duplicates"`
	// Errors is the number of error messages the path returned, such as an
	// unreachable host. They are reported rather than ignored, because they
	// say the path is broken rather than merely lossy.
	Errors int `json:"errors"`
	// OutOfOrder is the number of replies that arrived after a reply with a
	// higher sequence number. A reordering fault shows up here.
	OutOfOrder int `json:"out_of_order"`
	// Bytes is the payload volume of the answers, not counting the duplicate
	// copies, so it stays comparable between measurements.
	Bytes int64 `json:"bytes"`
	// Duration is the wall clock time the measurement took.
	Duration time.Duration `json:"duration"`
	// Goodput is the measured payload rate in bits per second. It is zero
	// when nothing came back.
	Goodput uint64 `json:"goodput"`
	// At is when the measurement finished.
	At time.Time `json:"at"`
	// Options are the settings the measurement ran with, kept next to the
	// result so a reader can see what was asked for as well as what arrived.
	Options Options `json:"options"`
}

// Replied reports whether anything came back at all.
func (m Measurement) Replied() bool { return m.Received > 0 }

// Measurer runs bounded echo measurements in laboratory namespaces.
type Measurer struct {
	limits Limits
	run    Runner
	slots  chan struct{}
	mu     sync.Mutex
}

// New builds a measurer. The returned measurer is safe for concurrent use and
// admits at most MaxConcurrent measurements at a time.
func New(limits Limits, run Runner) (*Measurer, error) {
	const op = "probe.New"
	if run == nil {
		return nil, apperr.Internal(op, "a measurer needs a command runner", nil)
	}
	checked, err := limits.check()
	if err != nil {
		return nil, err
	}
	return &Measurer{
		limits: checked,
		run:    run,
		slots:  make(chan struct{}, checked.MaxConcurrent),
	}, nil
}

// Limits returns the limits the measurer enforces.
func (m *Measurer) Limits() Limits { return m.limits }

// Measure sends echo requests to a target from inside a laboratory namespace
// and reports what came back.
func (m *Measurer) Measure(ctx context.Context, namespace, target string, options Options) (Measurement, error) {
	const op = "probe.Measurer.Measure"
	if err := m.admit(); err != nil {
		return Measurement{}, err
	}
	defer m.release()

	settings, argv, err := m.plan(op, namespace, target, options)
	if err != nil {
		return Measurement{}, err
	}
	if err := apperr.FromContext(ctx, op); err != nil {
		return Measurement{}, err
	}

	started := time.Now()
	stdout, stderr, code, err := m.run.Run(namespace, argv...)
	elapsed := time.Since(started)
	if err != nil {
		return Measurement{}, apperr.Wrap(err, apperr.KindInternal, op, fmt.Sprintf(
			"the probe in %s could not be run", namespace))
	}
	if elapsed > m.limits.MaxDuration+time.Second {
		return Measurement{}, apperr.LimitExceeded(op, fmt.Sprintf(
			"the probe took %s, which is past the %s budget", elapsed, m.limits.MaxDuration), nil)
	}

	measurement, err := parse(stdout, namespace, target, settings, elapsed)
	if err != nil {
		// An exit status of one means the probe ran and packets were lost,
		// which is an answer rather than a failure. Anything else, including
		// output that cannot be read, is a failure: a measurement that cannot
		// be understood is never reported as a healthy one.
		if code == 1 {
			return Measurement{}, err
		}
		return Measurement{}, apperr.Wrap(err, kindOf(err), op, fmt.Sprintf(
			"the probe in %s failed: %s", namespace, strings.TrimSpace(stderr)))
	}
	if code != 0 && code != 1 {
		return Measurement{}, apperr.IO(op, fmt.Sprintf("the probe in %s failed with exit status %d", namespace, code),
			fmt.Errorf("%s", strings.TrimSpace(stderr)))
	}
	return measurement, nil
}

// plan validates the options against the limits and renders the argument
// vector. Nothing is sent before every bound has been checked.
func (m *Measurer) plan(op, namespace, target string, options Options) (Options, []string, error) {
	if strings.TrimSpace(target) == "" {
		return options, nil, apperr.InvalidArgument(op, "a probe needs a target", nil)
	}
	if err := checkAddress(op, target); err != nil {
		return options, nil, err
	}
	if strings.TrimSpace(namespace) == "" {
		return options, nil, apperr.InvalidArgument(op, "a probe needs a laboratory namespace", nil)
	}
	for _, r := range target {
		if r <= ' ' || r == 0x7f {
			return options, nil, apperr.InvalidArgument(op, fmt.Sprintf(
				"the target %q contains a space or a control character", target), nil)
		}
	}

	settings := options
	if settings.Count == 0 {
		settings.Count = DefaultOptions().Count
	}
	if settings.Interval == 0 {
		settings.Interval = DefaultOptions().Interval
	}
	if settings.Size == 0 {
		settings.Size = DefaultOptions().Size
	}
	if settings.Timeout == 0 {
		settings.Timeout = DefaultOptions().Timeout
	}
	if settings.Deadline == 0 {
		settings.Deadline = DefaultOptions().Deadline
	}

	if settings.Count < 0 || settings.Count > m.limits.MaxProbes {
		return options, nil, apperr.LimitExceeded(op, fmt.Sprintf(
			"a measurement of %d packets is outside 1 to %d", settings.Count, m.limits.MaxProbes), nil)
	}
	if settings.Interval < time.Millisecond {
		return options, nil, apperr.InvalidArgument(op, fmt.Sprintf(
			"the interval %s is below the one millisecond floor", settings.Interval), nil)
	}
	rate := int(time.Second / settings.Interval)
	if rate > m.limits.MaxRate {
		return options, nil, apperr.LimitExceeded(op, fmt.Sprintf(
			"an interval of %s is %d packets per second, the packet rate limit is %d",
			settings.Interval, rate, m.limits.MaxRate), nil)
	}
	if settings.Size < 0 || settings.Size > m.limits.MaxSize {
		return options, nil, apperr.LimitExceeded(op, fmt.Sprintf(
			"a payload of %d bytes is outside 0 to %d", settings.Size, m.limits.MaxSize), nil)
	}
	if settings.Timeout <= 0 {
		return options, nil, apperr.InvalidArgument(op, "the per packet timeout must be positive", nil)
	}
	if settings.Deadline < settings.Timeout {
		return options, nil, apperr.InvalidArgument(op, fmt.Sprintf(
			"the deadline %s is shorter than the per packet timeout %s",
			settings.Deadline, settings.Timeout), nil)
	}
	if settings.Deadline > m.limits.MaxDuration {
		return options, nil, apperr.LimitExceeded(op, fmt.Sprintf(
			"a deadline of %s is past the %s limit", settings.Deadline, m.limits.MaxDuration), nil)
	}

	argv := []string{
		"ping",
		"-n",
		"-c", strconv.Itoa(settings.Count),
		"-i", secondsText(settings.Interval),
		"-s", strconv.Itoa(settings.Size),
		"-W", strconv.Itoa(wholeSeconds(settings.Timeout)),
		"-w", strconv.Itoa(wholeSeconds(settings.Deadline)),
		target,
	}
	return settings, argv, nil
}

// admit takes a measurement slot, or refuses when the laboratory is already
// running as many measurements as it is allowed to.
func (m *Measurer) admit() error {
	const op = "probe.Measurer.Measure"
	select {
	case m.slots <- struct{}{}:
		return nil
	default:
	}
	m.mu.Lock()
	running := len(m.slots)
	m.mu.Unlock()
	return apperr.LimitExceeded(op, fmt.Sprintf(
		"the laboratory is already running %d measurements, the limit is %d", running, m.limits.MaxConcurrent), nil)
}

func (m *Measurer) release() { <-m.slots }

// The echo summary is read from the probe's own report. The namespace layer
// runs commands with a bare environment, so the report is in the C locale and
// these patterns match; an unparsable report is an error, never a zero.
var (
	// Between the reply count and the loss the probe reports what else came
	// back: copies of replies, and errors such as an unreachable host. Both
	// clauses are optional and either may come first, so they are read as a
	// group and picked apart below.
	summaryPattern = regexp.MustCompile(
		`(\d+)\ packets\ transmitted,\s*(\d+)\ (?:packets\ )?received` +
			`((?:,\s*\+\d+\ (?:duplicates|errors))*),\s*([\d.]+)%\ packet\ loss`)
	countClausePattern = regexp.MustCompile(`\+(\d+)\ (duplicates|errors)`)
	roundTripPattern   = regexp.MustCompile(
		`rtt\ min/avg/max/mdev\ =\s*([\d.]+)/([\d.]+)/([\d.]+)/([\d.]+)\ ms`)
	sequencePattern = regexp.MustCompile(`icmp_seq=(\d+)`)
)

// parse turns the probe's report into a measurement.
func parse(stdout, namespace, target string, options Options, elapsed time.Duration) (Measurement, error) {
	const op = "probe.parse"
	summary := summaryPattern.FindStringSubmatch(stdout)
	if summary == nil {
		return Measurement{}, apperr.IO(op, "the probe report could not be read, so the measurement is unknown: "+excerpt(stdout), nil)
	}
	sent, err := strconv.Atoi(summary[1])
	if err != nil {
		return Measurement{}, apperr.IO(op, "the packet count in the probe report could not be read", err)
	}
	received, err := strconv.Atoi(summary[2])
	if err != nil {
		return Measurement{}, apperr.IO(op, "the received count in the probe report could not be read", err)
	}
	loss, err := strconv.ParseFloat(summary[4], 64)
	if err != nil {
		return Measurement{}, apperr.IO(op, "the loss in the probe report could not be read", err)
	}
	duplicates, errors, err := readCounts(op, summary[3])
	if err != nil {
		return Measurement{}, err
	}
	if received > sent {
		return Measurement{}, apperr.IO(op, fmt.Sprintf(
			"the probe report claims %d replies to %d requests", received, sent), nil)
	}

	measurement := Measurement{
		Target:     target,
		Namespace:  namespace,
		Sent:       sent,
		Received:   received,
		Loss:       loss,
		Duplicates: duplicates,
		Errors:     errors,
		OutOfOrder: outOfOrder(stdout),
		Duration:   elapsed,
		At:         time.Now(),
		Options:    options,
	}
	// Each reply carries the payload plus the eight byte echo header.
	measurement.Bytes = int64(received) * int64(options.Size+8)

	if match := roundTripPattern.FindStringSubmatch(stdout); match != nil {
		values := make([]time.Duration, 0, 4)
		for _, field := range match[1:] {
			value, err := strconv.ParseFloat(field, 64)
			if err != nil {
				return Measurement{}, apperr.IO(op, "the round trip times in the probe report could not be read", err)
			}
			values = append(values, time.Duration(value*float64(time.Millisecond)))
		}
		measurement.MinRTT, measurement.AverageRTT = values[0], values[1]
		measurement.MaxRTT, measurement.DeviationRTT = values[2], values[3]
	} else if received > 0 {
		return Measurement{}, apperr.IO(op, fmt.Sprintf(
			"%d replies came back but the report carries no round trip times", received), nil)
	}

	if measurement.Bytes > 0 && elapsed > 0 {
		measurement.Goodput = uint64(float64(measurement.Bytes*8) / elapsed.Seconds())
	}
	return measurement, nil
}

// readCounts picks the duplicate and error counts out of the summary clauses.
// An error message that cannot be read is a parse failure rather than a zero,
// because a zero here would report a healthy path that was never measured.
func readCounts(op, clauses string) (duplicates, errors int, err error) {
	for _, clause := range countClausePattern.FindAllStringSubmatch(clauses, -1) {
		value, convErr := strconv.Atoi(clause[1])
		if convErr != nil {
			return 0, 0, apperr.IO(op, "the count in the probe report could not be read", convErr)
		}
		switch clause[2] {
		case "duplicates":
			duplicates = value
		case "errors":
			errors = value
		}
	}
	return duplicates, errors, nil
}

// excerpt returns the tail of a report, so a report that could not be read can
// still be seen by whoever has to work out why. It is bounded, because a report
// is program output rather than an error message.
func excerpt(stdout string) string {
	const limit = 400
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		return "the probe printed nothing"
	}
	if len(trimmed) <= limit {
		return "the probe printed: " + strconv.Quote(trimmed)
	}
	return "the probe printed: " + strconv.Quote(trimmed[len(trimmed)-limit:]) + " (truncated)"
}

// outOfOrder counts the replies that arrived after a reply with a higher
// sequence number, which is what a reordering fault leaves behind.
func outOfOrder(stdout string) int {
	matches := sequencePattern.FindAllStringSubmatch(stdout, -1)
	count := 0
	highest := -1
	for _, match := range matches {
		sequence, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		if sequence < highest {
			count++
			continue
		}
		highest = sequence
	}
	return count
}

// secondsText renders an interval for the probe, which wants seconds with up
// to three decimals.
func secondsText(interval time.Duration) string {
	milliseconds := interval.Milliseconds()
	return fmt.Sprintf("%d.%03d", milliseconds/1000, milliseconds%1000)
}

// wholeSeconds rounds a duration up to whole seconds, which is the unit the
// probe's own timeouts use.
func wholeSeconds(d time.Duration) int {
	seconds := int(d / time.Second)
	if d%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	return seconds
}

// kindOf keeps the kind of an error that already carries one.
func kindOf(err error) apperr.Kind {
	if kind, ok := apperr.KindOf(err); ok {
		return kind
	}
	return apperr.KindInternal
}

// checkAddress keeps a target to something that can be an address or a host
// name, never a shell word.
func checkAddress(op, target string) error {
	for _, r := range target {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == ':', r == '_':
		default:
			return apperr.InvalidArgument(op, fmt.Sprintf(
				"the target %q contains %q, which is not a valid address", target, string(r)), nil)
		}
	}
	return nil
}
