package config

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/netforge/netforge/internal/apperr"
)

const (
	MaxConfigBytes = 1 << 20
	MinYAMLBytes   = 1
)

const (
	CeilingNamespaces       = 64
	CeilingInterfaces       = 512
	CeilingDuration         = 24 * time.Hour
	CeilingBandwidth        = 100 * Gbps
	CeilingPacketRate       = 10_000_000
	CeilingConcurrentProbes = 10_000
)

type Config struct {
	Name   string      `yaml:"name"`
	Seed   *uint64     `yaml:"seed,omitempty"`
	Scope  Scope       `yaml:"scope"`
	Limits Limits      `yaml:"limits"`
	Faults []FaultSpec `yaml:"faults,omitempty"`
}

type Scope struct {
	Network    string   `yaml:"network"`
	Namespaces []string `yaml:"namespaces"`
}

type Limits struct {
	MaxNamespaces       int       `yaml:"max_namespaces"`
	MaxInterfaces       int       `yaml:"max_interfaces"`
	MaxDuration         Duration  `yaml:"max_duration"`
	MaxBandwidth        Bandwidth `yaml:"max_bandwidth"`
	MaxPacketRate       int       `yaml:"max_packet_rate"`
	MaxConcurrentProbes int       `yaml:"max_concurrent_probes"`
}

func (c Config) ExperimentID() string {
	return ExperimentIDFor(c)
}

func (c Config) Network() string {
	return c.Scope.Network
}

func (c Config) NamespaceNames() []string {
	return c.Scope.Namespaces
}

func (c Config) Validate() error {
	problems := problems{}
	problems.add(validateName(c.Name)...)
	problems.add(validateNetwork(c.Scope.Network)...)
	problems.add(validateNamespaces(c.Scope.Namespaces)...)
	problems.add(validateLimits(c.Limits)...)
	problems.add(validateScope(c.Scope, c.Limits)...)
	problems.add(validateFaults(c.Faults, c.Scope, c.Limits)...)
	return problems.err()
}

func validateName(name string) []error {
	if name == "" {
		return []error{apperr.InvalidArgument("config.validate", "name is required", nil)}
	}
	return checkName("config.validate", "name", name)
}

func validateNetwork(network string) []error {
	if network == "" {
		return []error{apperr.InvalidArgument("config.validate", "scope.network is required", nil)}
	}
	prefix, err := ParseNetwork(network)
	if err != nil {
		return []error{err}
	}
	if prefix.Addr().Is6() && !prefix.Addr().Is4In6() {
		return []error{apperr.Unsupported("config.validate", "scope.network must be IPv4; IPv6 scope is not implemented in this phase", nil)}
	}
	return nil
}

func validateNamespaces(names []string) []error {
	if len(names) == 0 {
		return []error{apperr.InvalidArgument("config.validate", "scope.namespaces must declare at least one namespace", nil)}
	}
	if len(names) > CeilingNamespaces {
		return []error{apperr.LimitExceeded("config.validate", "scope.namespaces count exceeds the supported maximum of "+itoa(CeilingNamespaces), nil)}
	}

	var problems []error
	seen := make(map[string]struct{}, len(names))
	for i, name := range names {
		field := "scope.namespaces[" + itoa(i) + "]"
		if errs := checkName("config.validate", field, name); len(errs) > 0 {
			problems = append(problems, errs...)
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			problems = append(problems, apperr.Validation("config.validate", field+" duplicates namespace "+name, nil))
			continue
		}
		seen[name] = struct{}{}
	}
	return problems
}

func validateLimits(limits Limits) []error {
	var problems []error

	if limits.MaxNamespaces <= 0 {
		problems = append(problems, apperr.InvalidArgument("config.validate", "limits.max_namespaces must be positive", nil))
	} else if limits.MaxNamespaces > CeilingNamespaces {
		problems = append(problems, aboveCeiling("limits.max_namespaces", itoa(limits.MaxNamespaces), itoa(CeilingNamespaces)))
	}

	if limits.MaxInterfaces <= 0 {
		problems = append(problems, apperr.InvalidArgument("config.validate", "limits.max_interfaces must be positive", nil))
	} else if limits.MaxInterfaces > CeilingInterfaces {
		problems = append(problems, aboveCeiling("limits.max_interfaces", itoa(limits.MaxInterfaces), itoa(CeilingInterfaces)))
	}

	if limits.MaxDuration <= 0 {
		problems = append(problems, apperr.InvalidArgument("config.validate", "limits.max_duration must be positive", nil))
	} else if limits.MaxDuration.Duration() > CeilingDuration {
		problems = append(problems, aboveCeiling("limits.max_duration", limits.MaxDuration.String(), CeilingDuration.String()))
	}

	if limits.MaxBandwidth == 0 {
		problems = append(problems, apperr.InvalidArgument("config.validate", "limits.max_bandwidth must be positive", nil))
	} else if limits.MaxBandwidth > CeilingBandwidth {
		problems = append(problems, aboveCeiling("limits.max_bandwidth", limits.MaxBandwidth.String(), CeilingBandwidth.String()))
	}

	if limits.MaxPacketRate <= 0 {
		problems = append(problems, apperr.InvalidArgument("config.validate", "limits.max_packet_rate must be positive", nil))
	} else if limits.MaxPacketRate > CeilingPacketRate {
		problems = append(problems, aboveCeiling("limits.max_packet_rate", itoa(limits.MaxPacketRate), itoa(CeilingPacketRate)))
	}

	if limits.MaxConcurrentProbes <= 0 {
		problems = append(problems, apperr.InvalidArgument("config.validate", "limits.max_concurrent_probes must be positive", nil))
	} else if limits.MaxConcurrentProbes > CeilingConcurrentProbes {
		problems = append(problems, aboveCeiling("limits.max_concurrent_probes", itoa(limits.MaxConcurrentProbes), itoa(CeilingConcurrentProbes)))
	}

	return problems
}

func validateScope(scope Scope, limits Limits) []error {
	if len(scope.Namespaces) == 0 || limits.MaxNamespaces <= 0 {
		return nil
	}
	if len(scope.Namespaces) > limits.MaxNamespaces {
		return []error{apperr.LimitExceeded("config.validate",
			"scope.namespaces count "+itoa(len(scope.Namespaces))+" exceeds limits.max_namespaces "+itoa(limits.MaxNamespaces), nil)}
	}
	return nil
}

// validateFaults checks the declared faults. Every fault is reported, not just
// the first, so a file with three mistakes says so.
func validateFaults(faults []FaultSpec, scope Scope, limits Limits) []error {
	const op = "config.validate"
	if len(faults) > CeilingFaults {
		return []error{aboveCeiling("faults", itoa(len(faults)), itoa(CeilingFaults))}
	}

	var problems []error
	placements := make(map[string]int, len(faults))
	for i, fault := range faults {
		field := "faults[" + itoa(i) + "]"
		problems = append(problems, fault.validate(scope, limits)...)

		// One device carries at most one fault, because a queue discipline is
		// a single root and a second one would merge into the first. The
		// direction is part of the placement: an inbound fault sits on the far
		// end of the link, so it never shares a device with an outbound one.
		// Only the engine knows the topology, so it has the last word on
		// where a fault lands; this catches the clashes a reader can see.
		placement := fault.Target + "/" + fault.Interface + "/" + string(fault.Direction)
		if first, clash := placements[placement]; clash {
			problems = append(problems, apperr.Validation(op, field+
				" and faults["+itoa(first)+"] both impair "+placement+
				", one device carries at most one fault", nil))
			continue
		}
		placements[placement] = i
	}
	return problems
}

func aboveCeiling(field, got, ceiling string) error {
	return apperr.LimitExceeded("config.validate", field+" "+got+" exceeds the supported maximum of "+ceiling, nil)
}

type problems struct {
	collected []error
}

func (p *problems) add(errs ...error) {
	for _, err := range errs {
		if err != nil {
			p.collected = append(p.collected, err)
		}
	}
}

func (p problems) err() error {
	if len(p.collected) == 0 {
		return nil
	}
	joined := errors.Join(p.collected...)
	return fmt.Errorf("%d configuration problems: %w", len(p.collected), joined)
}

func itoa(value int) string {
	return strconv.Itoa(value)
}
