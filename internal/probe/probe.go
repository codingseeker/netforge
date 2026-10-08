package probe

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codingseeker/netforge/internal/apperr"
)

type Runner interface {
	Run(namespace string, argv ...string) (stdout, stderr string, code int, err error)
}

type Limits struct {
	MaxProbes int `json:"max_probes"`

	MaxRate int `json:"max_rate"`

	MaxSize int `json:"max_size"`

	MaxDuration time.Duration `json:"max_duration"`

	MaxConcurrent int `json:"max_concurrent"`

	MaxResponseSize int `json:"max_response_size"`

	MaxStoredResults int `json:"max_stored_results"`

	MaxNameLength int `json:"max_name_length"`
}

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

const defaultRequestTimeout = 5 * time.Second

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

type Options struct {
	Count int

	Interval time.Duration

	Size int

	Timeout time.Duration

	Deadline time.Duration
}

func DefaultOptions() Options {
	return Options{
		Count:    10,
		Interval: 10 * time.Millisecond,
		Size:     1400,
		Timeout:  time.Second,
		Deadline: 10 * time.Second,
	}
}

type TransportLimits struct {
	MaxPayload     int   `json:"max_payload"`
	MaxWindow      int   `json:"max_window"`
	MaxConnections int   `json:"max_connections"`
	IdleTimeout    int64 `json:"idle_timeout_ns"`
}

type Measurement struct {
	Target string `json:"target"`

	Namespace string `json:"namespace"`

	Sent     int `json:"sent"`
	Received int `json:"received"`

	Loss float64 `json:"loss"`

	MinRTT       time.Duration `json:"min_rtt"`
	AverageRTT   time.Duration `json:"average_rtt"`
	MaxRTT       time.Duration `json:"max_rtt"`
	DeviationRTT time.Duration `json:"deviation_rtt"`

	Retransmitted int `json:"retransmitted"`

	Transport *TransportLimits `json:"transport,omitempty"`

	Duplicates int `json:"duplicates"`

	Errors int `json:"errors"`

	OutOfOrder int `json:"out_of_order"`

	Bytes int64 `json:"bytes"`

	Duration time.Duration `json:"duration"`

	Goodput uint64 `json:"goodput"`

	At time.Time `json:"at"`

	Options Options `json:"options"`
}

func (m Measurement) Replied() bool { return m.Received > 0 }

type Measurer struct {
	limits Limits
	run    Runner
	slots  chan struct{}
	mu     sync.Mutex
}

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

func (m *Measurer) Limits() Limits { return m.limits }

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

var (
	summaryPattern = regexp.MustCompile(
		`(\d+)\ packets\ transmitted,\s*(\d+)\ (?:packets\ )?received` +
			`((?:,\s*\+\d+\ (?:duplicates|errors))*),\s*([\d.]+)%\ packet\ loss`)
	countClausePattern = regexp.MustCompile(`\+(\d+)\ (duplicates|errors)`)
	roundTripPattern   = regexp.MustCompile(
		`rtt\ min/avg/max/mdev\ =\s*([\d.]+)/([\d.]+)/([\d.]+)/([\d.]+)\ ms`)
	sequencePattern = regexp.MustCompile(`icmp_seq=(\d+)`)
)

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

func secondsText(interval time.Duration) string {
	milliseconds := interval.Milliseconds()
	return fmt.Sprintf("%d.%03d", milliseconds/1000, milliseconds%1000)
}

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

func kindOf(err error) apperr.Kind {
	if kind, ok := apperr.KindOf(err); ok {
		return kind
	}
	return apperr.KindInternal
}

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
