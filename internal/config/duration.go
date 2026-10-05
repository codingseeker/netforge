package config

import (
	"time"

	"gopkg.in/yaml.v3"

	"github.com/netforge/netforge/internal/apperr"
)

type Duration time.Duration

func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

func (d Duration) String() string {
	return time.Duration(d).String()
}

func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	raw, err := requireScalar("config.Duration", node, "duration", `"30s" or "10m"`)
	if err != nil {
		return err
	}
	parsed, err := ParseDuration(raw)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

func ParseDuration(raw string) (Duration, error) {
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return 0, apperr.InvalidConfig("config.ParseDuration", "duration "+quote(raw)+" is not a valid duration", err)
	}
	if parsed < 0 {
		return 0, apperr.InvalidArgument("config.ParseDuration", "duration "+quote(raw)+" must not be negative", nil)
	}
	return Duration(parsed), nil
}
