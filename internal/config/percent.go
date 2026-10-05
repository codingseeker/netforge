package config

import (
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/netforge/netforge/internal/apperr"
)

// Percent is a whole percentage written with a trailing sign, such as "10%".
//
// The sign is required on purpose. A fault of "10" and a fault of "10%" are
// easy to confuse and impossible to tell apart once they are in a record, and a
// percentage with a fractional part is refused because the kernel takes one
// anyway. Every number this project accepts in configuration is written the way
// it is meant to be read, with its unit attached.
type Percent int

const (
	// MinPercent is the smallest effect a fault may ask for. A fault that
	// changes nothing is not a fault, and the kernel accepts a zero loss or
	// duplication that would quietly do nothing.
	MinPercent = 1
	// MaxPercent is the largest effect a fault may ask for.
	MaxPercent = 100
)

func (p Percent) String() string { return strconv.Itoa(int(p)) + "%" }

func (p Percent) MarshalYAML() (any, error) { return p.String(), nil }

func (p *Percent) UnmarshalYAML(node *yaml.Node) error {
	raw, err := requireScalar("config.Percent", node, "percentage", `"10%"`)
	if err != nil {
		return err
	}
	parsed, err := ParsePercent(raw)
	if err != nil {
		return err
	}
	*p = parsed
	return nil
}

func ParsePercent(raw string) (Percent, error) {
	const op = "config.ParsePercent"
	trimmed := strings.TrimSpace(raw)
	if !strings.HasSuffix(trimmed, "%") {
		return 0, apperr.InvalidConfig(op,
			"percentage "+quote(raw)+" must end with '%', such as \"10%\"", nil)
	}
	digits := strings.TrimSuffix(trimmed, "%")
	if digits == "" {
		return 0, apperr.InvalidConfig(op, "percentage "+quote(raw)+" has no number in front of the '%'", nil)
	}
	// strconv would read a sign, an exponent and an underscore here, none of
	// which belong in a percentage someone reads by eye.
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, apperr.InvalidConfig(op,
				"percentage "+quote(raw)+" must be whole digits in front of the '%', such as \"10%\"", nil)
		}
	}
	value, err := strconv.Atoi(digits)
	if err != nil {
		return 0, apperr.InvalidConfig(op, "percentage "+quote(raw)+" is not a number", err)
	}
	if value < MinPercent || value > MaxPercent {
		return 0, apperr.InvalidArgument(op, "percentage "+quote(raw)+" must be between "+
			strconv.Itoa(MinPercent)+"% and "+strconv.Itoa(MaxPercent)+"%", nil)
	}
	return Percent(value), nil
}
