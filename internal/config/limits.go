package config

import (
	"net/netip"
	"strconv"
	"time"

	"github.com/codingseeker/netforge/internal/apperr"
)

func DefaultLimits() Limits {
	return Limits{
		MaxNamespaces:       10,
		MaxInterfaces:       30,
		MaxDuration:         Duration(10 * time.Minute),
		MaxBandwidth:        Gbps,
		MaxPacketRate:       100_000,
		MaxConcurrentProbes: 100,
	}
}

func (l Limits) WithDefaults() Limits {
	defaults := DefaultLimits()
	if l.MaxNamespaces == 0 {
		l.MaxNamespaces = defaults.MaxNamespaces
	}
	if l.MaxInterfaces == 0 {
		l.MaxInterfaces = defaults.MaxInterfaces
	}
	if l.MaxDuration == 0 {
		l.MaxDuration = defaults.MaxDuration
	}
	if l.MaxBandwidth == 0 {
		l.MaxBandwidth = defaults.MaxBandwidth
	}
	if l.MaxPacketRate == 0 {
		l.MaxPacketRate = defaults.MaxPacketRate
	}
	if l.MaxConcurrentProbes == 0 {
		l.MaxConcurrentProbes = defaults.MaxConcurrentProbes
	}
	return l
}

func ParseNetwork(value string) (netip.Prefix, error) {
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		return netip.Prefix{}, apperr.InvalidArgument("config.ParseNetwork",
			"scope.network "+strconv.Quote(value)+" is not a valid CIDR", err)
	}
	if prefix.Bits() == 0 {
		return netip.Prefix{}, apperr.InvalidArgument("config.ParseNetwork",
			"scope.network "+strconv.Quote(value)+" must be a specific network, not a default route", nil)
	}
	if prefix != prefix.Masked() {
		return netip.Prefix{}, apperr.InvalidArgument("config.ParseNetwork",
			"scope.network "+strconv.Quote(value)+" has host bits set; use the canonical network "+prefix.Masked().String(), nil)
	}
	return prefix, nil
}
