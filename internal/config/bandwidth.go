package config

import (
	"errors"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/codingseeker/netforge/internal/apperr"
)

const (
	BitsPerSecond Bandwidth = 1
	Kbps                    = 1000 * BitsPerSecond
	Mbps                    = 1000 * Kbps
	Gbps                    = 1000 * Mbps
)

var bandwidthUnits = []struct {
	suffix string
	factor uint64
}{
	{suffix: "gbps", factor: uint64(Gbps)},
	{suffix: "mbps", factor: uint64(Mbps)},
	{suffix: "kbps", factor: uint64(Kbps)},
	{suffix: "bps", factor: 1},
}

type Bandwidth uint64

func (b Bandwidth) BitsPerSecond() uint64 {
	return uint64(b)
}

func (b Bandwidth) String() string {
	return formatBandwidth(uint64(b))
}

func (b Bandwidth) MarshalYAML() (any, error) {
	return b.String(), nil
}

func (b *Bandwidth) UnmarshalYAML(node *yaml.Node) error {
	raw, err := requireScalar("config.Bandwidth", node, "bandwidth", `"100Mbps" or "1Gbps"`)
	if err != nil {
		return err
	}
	parsed, err := ParseBandwidth(raw)
	if err != nil {
		return err
	}
	*b = parsed
	return nil
}

func ParseBandwidth(raw string) (Bandwidth, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, apperr.InvalidArgument("config.ParseBandwidth", "bandwidth must not be empty", nil)
	}

	lowered := strings.ToLower(trimmed)
	matched := ""
	factor := uint64(0)
	for _, unit := range bandwidthUnits {
		if strings.HasSuffix(lowered, unit.suffix) {
			matched = unit.suffix
			factor = unit.factor
			break
		}
	}
	if matched == "" {
		return 0, apperr.InvalidArgument("config.ParseBandwidth",
			"bandwidth "+quote(raw)+" must end with bps, kbps, mbps or gbps", nil)
	}

	magnitude := strings.TrimSpace(trimmed[:len(trimmed)-len(matched)])
	if magnitude == "" {
		return 0, apperr.InvalidArgument("config.ParseBandwidth", "bandwidth "+quote(raw)+" is missing a value", nil)
	}
	if strings.ContainsAny(magnitude, ".eE") {
		return 0, apperr.InvalidArgument("config.ParseBandwidth",
			"bandwidth "+quote(raw)+" must be a whole number of bits per second", nil)
	}

	value, err := strconv.ParseUint(magnitude, 10, 64)
	if err != nil {
		switch {
		case strings.HasPrefix(magnitude, "-"):
			return 0, apperr.InvalidArgument("config.ParseBandwidth", "bandwidth "+quote(raw)+" must not be negative", nil)
		case errors.Is(err, strconv.ErrRange):
			return 0, apperr.LimitExceeded("config.ParseBandwidth", "bandwidth "+quote(raw)+" overflows the supported range", nil)
		default:
			return 0, apperr.InvalidArgument("config.ParseBandwidth", "bandwidth "+quote(raw)+" is not a whole number", nil)
		}
	}
	if value == 0 {
		return 0, apperr.InvalidArgument("config.ParseBandwidth", "bandwidth "+quote(raw)+" must be positive", nil)
	}

	limit := ^uint64(0) / factor
	if value > limit {
		return 0, apperr.LimitExceeded("config.ParseBandwidth", "bandwidth "+quote(raw)+" overflows the supported range", nil)
	}

	return Bandwidth(value * factor), nil
}

func formatBandwidth(bits uint64) string {
	if bits == 0 {
		return "0bps"
	}
	units := []struct {
		suffix string
		factor uint64
	}{
		{suffix: "Gbps", factor: uint64(Gbps)},
		{suffix: "Mbps", factor: uint64(Mbps)},
		{suffix: "kbps", factor: uint64(Kbps)},
	}
	for _, unit := range units {
		if bits >= unit.factor && bits%unit.factor == 0 {
			return strconv.FormatUint(bits/unit.factor, 10) + unit.suffix
		}
	}
	return strconv.FormatUint(bits, 10) + "bps"
}
