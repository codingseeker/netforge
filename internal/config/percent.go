package config

import (
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/codingseeker/netforge/internal/apperr"
)

type Percent int

const (
	MinPercent = 1

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
