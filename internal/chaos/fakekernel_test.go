package chaos

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// The helpers below model how the kernel turns an argument vector into the
// state it reports back. They are deliberately literal: a duration becomes
// seconds, a percentage becomes a fraction, a rate becomes bytes per second and
// the queueing limit becomes microseconds, because that is exactly the
// conversion the engine has to survive.

var handleCounter = 0x8000

func nextHandle() string {
	handleCounter++
	return strconv.FormatInt(int64(handleCounter), 16) + ":"
}

// argumentAfter returns the value that follows a flag.
func argumentAfter(argv []string, flag string) string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag {
			return argv[i+1]
		}
	}
	return ""
}

func hasArgument(argv []string, flag string) bool {
	for _, item := range argv {
		if item == flag {
			return true
		}
	}
	return false
}

// qdiscKind finds which discipline an install vector asks for.
func qdiscKind(argv []string) string {
	for _, item := range argv {
		switch item {
		case "netem", "tbf":
			return item
		}
	}
	return ""
}

func kernelOptions(kind string, argv []string, lie bool) map[string]any {
	if kind == "tbf" {
		return kernelBucket(argv, lie)
	}
	return kernelNetem(argv, lie)
}

func kernelBucket(argv []string, lie bool) map[string]any {
	rate := parseBits(argumentAfter(argv, "rate"))
	if lie {
		rate *= 2
	}
	burst := parseBits(argumentAfter(argv, "burst")) / 8
	latency := parseDuration(argumentAfter(argv, "latency"))
	return map[string]any{
		"rate":  rate / 8,
		"burst": burst,
		"lat":   int64(latency / time.Microsecond),
	}
}

func kernelNetem(argv []string, lie bool) map[string]any {
	options := map[string]any{
		"limit": 1000,
		"seed":  uint64(15287240990339982236),
		"ecn":   false,
		"gap":   0,
	}
	if value := argumentAfter(argv, "limit"); value != "" {
		depth, _ := strconv.Atoi(value)
		options["limit"] = depth
	}
	if hasArgument(argv, "delay") {
		delay := parseDuration(argumentAfter(argv, "delay"))
		jitter := time.Duration(0)
		correlation := 0.0
		tail := argumentsAfter(argv, "delay")
		if len(tail) > 1 && !strings.HasSuffix(tail[1], "%") {
			jitter = parseDuration(tail[1])
		}
		if len(tail) > 2 {
			correlation = parsePercent(tail[2])
		}
		if lie {
			delay /= 2
		}
		options["delay"] = map[string]any{
			"delay":       delay.Seconds(),
			"jitter":      jitter.Seconds(),
			"correlation": correlation,
		}
	}
	if hasArgument(argv, "loss") {
		loss := parsePercent(argumentAfter(argv, "loss"))
		correlation := 0.0
		if tail := argumentsAfter(argv, "loss"); len(tail) > 1 {
			correlation = parsePercent(tail[1])
		}
		if lie {
			loss /= 2
		}
		options["loss-random"] = map[string]any{"loss": loss, "correlation": correlation}
	}
	if hasArgument(argv, "duplicate") {
		duplicate := parsePercent(argumentAfter(argv, "duplicate"))
		if lie {
			duplicate = 0
		}
		options["duplicate"] = map[string]any{"duplicate": duplicate, "correlation": 0.0}
	}
	if hasArgument(argv, "reorder") {
		tail := argumentsAfter(argv, "reorder")
		reorder := parsePercent(tail[0])
		correlation := 0.0
		gap := 1
		if len(tail) > 1 {
			correlation = parsePercent(tail[1])
		}
		if len(tail) > 2 {
			gap, _ = strconv.Atoi(tail[2])
		}
		if lie {
			reorder = 0
		}
		options["reorder"] = map[string]any{"reorder": reorder, "correlation": correlation}
		options["gap"] = gap
	}
	return options
}

// argumentsAfter returns the values that follow a flag.
func argumentsAfter(argv []string, flag string) []string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag {
			return argv[i+1:]
		}
	}
	return nil
}

func parseDuration(value string) time.Duration {
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0
	}
	return parsed
}

func parsePercent(value string) float64 {
	trimmed := strings.TrimSuffix(value, "%")
	parsed, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0
	}
	return parsed / 100
}

// parseBits reads a rate or a size and returns it in bits per second, which is
// the unit the engine asks for. Traffic control writes a bare number as bytes
// and accepts bit, kbit, mbit, gbit, b, kb, mb and gb.
func parseBits(value string) uint64 {
	number := value
	multiplier := uint64(8)
	switch {
	case strings.HasSuffix(value, "kbit"):
		number, multiplier = strings.TrimSuffix(value, "kbit"), 1000
	case strings.HasSuffix(value, "mbit"):
		number, multiplier = strings.TrimSuffix(value, "mbit"), 1000*1000
	case strings.HasSuffix(value, "gbit"):
		number, multiplier = strings.TrimSuffix(value, "gbit"), 1000*1000*1000
	case strings.HasSuffix(value, "bit"):
		number, multiplier = strings.TrimSuffix(value, "bit"), 1
	case strings.HasSuffix(value, "kb"):
		number, multiplier = strings.TrimSuffix(value, "kb"), 1000*8
	case strings.HasSuffix(value, "mb"):
		number, multiplier = strings.TrimSuffix(value, "mb"), 1000*1000*8
	case strings.HasSuffix(value, "gb"):
		number, multiplier = strings.TrimSuffix(value, "gb"), 1000*1000*1000*8
	case strings.HasSuffix(value, "b"):
		number, multiplier = strings.TrimSuffix(value, "b"), 8
	}
	parsed, err := strconv.ParseUint(number, 10, 64)
	if err != nil {
		return 0
	}
	return parsed * multiplier
}

// renderState is used by the tests that need a kernel report for a device.
func renderState(entries ...map[string]any) string {
	data, err := json.Marshal(entries)
	if err != nil {
		return ""
	}
	return string(data)
}
