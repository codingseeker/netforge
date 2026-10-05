package config

import (
	"errors"
	"fmt"
	"strconv"

	"gopkg.in/yaml.v3"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/chaos"
	"github.com/netforge/netforge/internal/iface"
)

const (
	// CeilingFaults is the largest number of faults one configuration may
	// declare. It matches the largest fault list the engine will hold.
	CeilingFaults = chaos.CeilingFaults
)

// FaultType names a kind of fault in configuration.
type FaultType string

const (
	FaultLatency     FaultType = "latency"
	FaultJitter      FaultType = "jitter"
	FaultPacketLoss  FaultType = "packet_loss"
	FaultBandwidth   FaultType = "bandwidth"
	FaultDuplication FaultType = "duplication"
	FaultReordering  FaultType = "reordering"
)

// FaultDirection names which side of a link a fault sits on.
type FaultDirection string

const (
	DirectionOut FaultDirection = "out"
	DirectionIn  FaultDirection = "in"
)

func (t FaultType) String() string { return string(t) }

// unit names the unit that value is written in for this fault type.
func (t FaultType) unit() string {
	switch t {
	case FaultLatency, FaultJitter:
		return "a duration such as \"60ms\""
	case FaultBandwidth:
		return "a bandwidth such as \"200kbps\""
	case FaultPacketLoss, FaultDuplication, FaultReordering:
		return "a percentage such as \"10%\""
	default:
		return "a value"
	}
}

func (t FaultType) valid() bool {
	switch t {
	case FaultLatency, FaultJitter, FaultPacketLoss, FaultBandwidth, FaultDuplication, FaultReordering:
		return true
	default:
		return false
	}
}

func (d FaultDirection) valid() bool {
	return d == DirectionOut || d == DirectionIn
}

// FaultSpec is one declared fault.
//
// The type of a fault decides what value means and which of the optional fields
// belong to it, so a fault cannot carry a field its type has no use for. A
// bandwidth fault with a jitter on it is a mistake in the file, and it is
// reported rather than quietly dropped.
type FaultSpec struct {
	// Target is the namespace that feels the fault. It must be one of the
	// namespaces in scope.
	Target string `yaml:"target"`
	// Interface is the data interface of the target. When empty the engine
	// resolves the single data interface of the namespace.
	Interface string `yaml:"interface,omitempty"`
	// Type is the kind of fault to inject.
	Type FaultType `yaml:"type"`
	// Direction selects which side of the link is impaired.
	Direction FaultDirection `yaml:"direction,omitempty"`
	// Value is the fault's main number, written in the unit that Type asks
	// for. It is kept as written so the file and the record agree.
	Value string `yaml:"value"`
	// Delay is the base delay for a jitter or a reordering fault. Both need
	// one, because there is nothing to jitter or to reorder against without a
	// delay to sit behind.
	Delay Duration `yaml:"delay,omitempty"`
	// Jitter is unused: the jitter of a jitter fault is its value.
	// Correlation is how strongly the fault's events bunch together.
	Correlation Percent `yaml:"correlation,omitempty"`
	// Gap is how many packets a reordering fault skips over.
	Gap int `yaml:"gap,omitempty"`
	// Queue is the netem queue depth.
	Queue int `yaml:"queue,omitempty"`
	// Duration bounds how long the fault may live.
	Duration Duration `yaml:"duration,omitempty"`

	// value holds value after it has been read in the unit the type asks for.
	value FaultValue
}

// FaultValue is a fault's main number once it has been read.
type FaultValue struct {
	// time is set for a latency or jitter fault.
	time Duration
	// percent is set for a packet loss, duplication or reordering fault.
	percent Percent
	// rate is set for a bandwidth fault.
	rate Bandwidth
}

func (v FaultValue) kind() FaultType {
	switch {
	case v.time != 0:
		return FaultLatency
	case v.percent != 0:
		return FaultPacketLoss
	case v.rate != 0:
		return FaultBandwidth
	default:
		return ""
	}
}

func (v FaultValue) String() string {
	switch v.kind() {
	case FaultLatency:
		return v.time.String()
	case FaultPacketLoss:
		return v.percent.String()
	case FaultBandwidth:
		return v.rate.String()
	default:
		return ""
	}
}

// UnmarshalYAML reads one fault. It reads the fields itself rather than letting
// the decoder fill the struct, because the meaning of value depends on type and
// because a hand written reader is the only way to keep refusing fields the
// type has no use for.
func (f *FaultSpec) UnmarshalYAML(node *yaml.Node) error {
	const op = "config.FaultSpec"
	if node == nil || node.Kind != yaml.MappingNode {
		return apperr.InvalidConfig(op, "a fault must be a mapping of its fields, got "+nodeKind(node), nil)
	}

	seen := make(map[string]bool, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.Kind != yaml.ScalarNode {
			return apperr.InvalidConfig(op, "a fault field name must be a plain name", nil)
		}
		if seen[key.Value] {
			return apperr.InvalidConfig(op, "the fault field "+quote(key.Value)+" is set twice", nil)
		}
		seen[key.Value] = true

		var err error
		switch key.Value {
		case "target":
			f.Target, err = faultString(op, value, "target")
		case "interface":
			f.Interface, err = faultString(op, value, "interface")
		case "type":
			var raw string
			if raw, err = faultString(op, value, "type"); err == nil {
				f.Type = FaultType(raw)
			}
		case "direction":
			var raw string
			if raw, err = faultString(op, value, "direction"); err == nil {
				f.Direction = FaultDirection(raw)
			}
		case "value":
			f.Value, err = faultString(op, value, "value")
		case "delay":
			f.Delay, err = faultDuration(op, value, "delay")
		case "correlation":
			f.Correlation, err = faultPercent(op, value, "correlation")
		case "gap":
			f.Gap, err = faultInt(op, value, "gap")
		case "queue":
			f.Queue, err = faultInt(op, value, "queue")
		case "duration":
			f.Duration, err = faultDuration(op, value, "duration")
		default:
			return apperr.InvalidConfig(op, fmt.Sprintf(
				"a fault has no field %q, it accepts target, interface, type, direction, value, "+
					"delay, correlation, gap, queue and duration", key.Value), nil)
		}
		if err != nil {
			return err
		}
	}

	// The unit of value follows the type, so the type has to be read first.
	return f.readValue(op)
}

// readValue reads value in the unit the fault type asks for.
func (f *FaultSpec) readValue(op string) error {
	if !f.Type.valid() {
		return apperr.InvalidArgument(op, fmt.Sprintf(
			"fault type %q is not supported, choose one of %s", f.Type, faultTypeList()), nil)
	}
	switch f.Type {
	case FaultLatency, FaultJitter:
		parsed, err := ParseDuration(f.Value)
		if err != nil {
			return apperr.InvalidConfig(op, fmt.Sprintf(
				"a %s fault takes %s, got %s", f.Type, f.Type.unit(), quote(f.Value)), err)
		}
		f.value = FaultValue{time: parsed}
	case FaultPacketLoss, FaultDuplication, FaultReordering:
		parsed, err := ParsePercent(f.Value)
		if err != nil {
			return apperr.InvalidConfig(op, fmt.Sprintf(
				"a %s fault takes %s, got %s", f.Type, f.Type.unit(), quote(f.Value)), err)
		}
		f.value = FaultValue{percent: parsed}
	case FaultBandwidth:
		parsed, err := ParseBandwidth(f.Value)
		if err != nil {
			return apperr.InvalidConfig(op, fmt.Sprintf(
				"a %s fault takes %s, got %s", f.Type, f.Type.unit(), quote(f.Value)), err)
		}
		f.value = FaultValue{rate: parsed}
	}
	return nil
}

// MarshalYAML writes a fault back out. The gap and the queue are plain counts,
// so they are written as quoted strings to match what the reader insists on;
// every other field already carries its unit.
func (f FaultSpec) MarshalYAML() (any, error) {
	out := map[string]any{
		"target": f.Target,
		"type":   string(f.Type),
		"value":  f.Value,
	}
	if f.Interface != "" {
		out["interface"] = f.Interface
	}
	if f.Direction != "" {
		out["direction"] = string(f.Direction)
	}
	if f.Delay != 0 {
		out["delay"] = f.Delay.String()
	}
	if f.Correlation != 0 {
		out["correlation"] = f.Correlation.String()
	}
	if f.Gap != 0 {
		out["gap"] = strconv.Itoa(f.Gap)
	}
	if f.Queue != 0 {
		out["queue"] = strconv.Itoa(f.Queue)
	}
	if f.Duration != 0 {
		out["duration"] = f.Duration.String()
	}
	return out, nil
}

// Translate turns a declared fault into the specification the engine applies.
// The result has already been checked against the limits, so a fault that
// survived decoding cannot be refused later, at the moment it would take effect.
func (f FaultSpec) Translate(limits Limits) (chaos.Spec, error) {
	const op = "config.FaultSpec.Translate"

	spec := chaos.Spec{
		Target:    f.Target,
		Interface: f.Interface,
		Direction: chaos.Direction(f.Direction),
		Kind:      chaos.Kind(f.Type),
		Duration:  chaos.Duration(f.Duration.Duration()),
	}
	switch f.Type {
	case FaultLatency:
		spec.Parameters.Latency = chaos.Duration(f.value.time.Duration())
	case FaultJitter:
		spec.Parameters.Latency = chaos.Duration(f.Delay.Duration())
		spec.Parameters.Jitter = chaos.Duration(f.value.time.Duration())
	case FaultPacketLoss:
		spec.Parameters.Loss = chaos.Percent(f.value.percent)
	case FaultDuplication:
		spec.Parameters.Duplication = chaos.Percent(f.value.percent)
	case FaultReordering:
		spec.Parameters.Reordering = chaos.Percent(f.value.percent)
		spec.Parameters.Latency = chaos.Duration(f.Delay.Duration())
		spec.Parameters.Gap = f.Gap
	case FaultBandwidth:
		spec.Parameters.Rate = uint64(f.value.rate)
	}
	spec.Parameters.Correlation = chaos.Percent(f.Correlation)
	spec.Parameters.Queue = f.Queue

	checked, err := spec.Validate(chaosLimits(limits))
	if err != nil {
		return chaos.Spec{}, apperr.Validation(op, fmt.Sprintf(
			"the %s fault on %s was refused: %s", f.Type, f.Target, messageOf(err)), err)
	}
	return checked, nil
}

// messageOf reports why a specification was refused, without repeating the
// operation the fault engine was called with.
func messageOf(err error) string {
	var reported *apperr.Error
	if errors.As(err, &reported) {
		return reported.Msg
	}
	return err.Error()
}

// chaosLimits carries the configuration limits into the fault engine, so a
// configuration cannot ask for more than the limits it declared.
func chaosLimits(limits Limits) chaos.Limits {
	return chaos.Limits{
		MaxFaults:    CeilingFaults,
		MaxDuration:  limits.MaxDuration.Duration(),
		MaxLatency:   CeilingLatency,
		MaxBandwidth: uint64(limits.MaxBandwidth),
		MaxQueue:     CeilingQueue,
		MaxGap:       CeilingGap,
	}
}

// Ceilings the configuration shares with the fault engine.
const (
	CeilingLatency = chaos.CeilingLatency
	CeilingQueue   = chaos.CeilingQueue
	CeilingGap     = chaos.CeilingGap
)

// checkFields refuses the optional fields that the fault's type has no use
// for, so a mistyped fault is reported rather than quietly half applied.
func (f FaultSpec) checkFields(op, field string) []error {
	var problems []error
	unused := func(name string) {
		problems = append(problems, apperr.Validation(op, fmt.Sprintf(
			"a %s fault has no %s, the field does not apply to this type", f.Type, name), nil))
	}
	hasDelay := f.Delay != 0
	hasCorrelation := f.Correlation != 0
	hasGap := f.Gap != 0
	hasQueue := f.Queue != 0

	switch f.Type {
	case FaultLatency:
		if hasDelay {
			unused("delay")
		}
		if hasCorrelation {
			unused("correlation")
		}
		if hasGap {
			unused("gap")
		}
	case FaultJitter:
		if hasGap {
			unused("gap")
		}
		if !hasDelay {
			problems = append(problems, apperr.Validation(op,
				"a jitter fault needs a delay to jitter against, set the delay field", nil))
		}
	case FaultPacketLoss, FaultDuplication:
		if hasDelay {
			unused("delay")
		}
		if hasGap {
			unused("gap")
		}
	case FaultReordering:
		if hasGap && f.Gap < 1 {
			problems = append(problems, apperr.InvalidArgument(op,
				"a reorder gap is a number of packets and must be at least 1", nil))
		}
		if !hasDelay {
			problems = append(problems, apperr.Validation(op,
				"a reordering fault needs a delay to reorder against, set the delay field", nil))
		}
	case FaultBandwidth:
		if hasDelay {
			unused("delay")
		}
		if hasCorrelation {
			unused("correlation")
		}
		if hasGap {
			unused("gap")
		}
	}
	if f.Type != FaultLatency && f.Type != FaultJitter && f.Type != FaultPacketLoss &&
		f.Type != FaultDuplication && f.Type != FaultReordering && hasQueue {
		unused("queue")
	}
	return problems
}

// validate checks one fault against the scope and the limits.
func (f FaultSpec) validate(scope Scope, limits Limits) []error {
	const op = "config.validate"
	field := "faults"

	var problems []error
	if errs := checkName(op, field+".target", f.Target); len(errs) > 0 {
		return append(problems, errs...)
	}
	if !containsName(scope.Namespaces, f.Target) {
		problems = append(problems, apperr.Validation(op, fmt.Sprintf(
			"%s.target %q is not one of scope.namespaces %v, a fault cannot reach outside the scope",
			field, f.Target, scope.Namespaces), nil))
	}
	if f.Interface != "" {
		if err := iface.ValidateName(op, field+".interface", f.Interface); err != nil {
			problems = append(problems, apperr.InvalidArgument(op,
				field+".interface "+quote(f.Interface)+" is not a valid device name", err))
		}
	}
	if f.Direction != "" && !f.Direction.valid() {
		problems = append(problems, apperr.InvalidArgument(op, fmt.Sprintf(
			"%s.direction %q is not supported, choose %q or %q",
			field, f.Direction, DirectionOut, DirectionIn), nil))
	}
	problems = append(problems, f.checkFields(op, field)...)

	if _, err := f.Translate(limits); err != nil {
		problems = append(problems, err)
	}
	return problems
}

func containsName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

func faultTypeList() string {
	types := []FaultType{FaultLatency, FaultJitter, FaultPacketLoss, FaultBandwidth, FaultDuplication, FaultReordering}
	names := make([]string, 0, len(types))
	for _, t := range types {
		names = append(names, string(t))
	}
	return joinComma(names)
}

func joinComma(names []string) string {
	out := ""
	for i, name := range names {
		switch {
		case i == 0:
			out = name
		case i == len(names)-1:
			out += " or " + name
		default:
			out += ", " + name
		}
	}
	return out
}

func faultString(op string, node *yaml.Node, field string) (string, error) {
	example := `"` + field + `"`
	if field == "value" {
		example = `"60ms", "200kbps" or "10%"`
	}
	return requireScalar(op, node, field, example)
}

func faultDuration(op string, node *yaml.Node, field string) (Duration, error) {
	raw, err := requireScalar(op, node, field, `"30s"`)
	if err != nil {
		return 0, err
	}
	return ParseDuration(raw)
}

func faultPercent(op string, node *yaml.Node, field string) (Percent, error) {
	raw, err := requireScalar(op, node, field, `"10%"`)
	if err != nil {
		return 0, err
	}
	return ParsePercent(raw)
}

func faultInt(op string, node *yaml.Node, field string) (int, error) {
	raw, err := requireScalar(op, node, field, `"2"`)
	if err != nil {
		return 0, err
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, apperr.InvalidConfig(op, field+" "+quote(raw)+" must be a whole number", err)
	}
	if value < 0 {
		return 0, apperr.InvalidArgument(op, field+" "+quote(raw)+" must not be negative", nil)
	}
	return value, nil
}
