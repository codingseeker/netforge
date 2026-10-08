package chaos

import (
	"strconv"
	"strings"
	"time"
)

var handleCounter = 0x8000

func nextHandle() string {
	handleCounter++
	return strconv.FormatInt(int64(handleCounter), 16) + ":"
}

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
