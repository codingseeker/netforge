package config

import (
	"strings"
	"testing"
	"time"

	"github.com/codingseeker/netforge/internal/apperr"
)

func TestParseBandwidth(t *testing.T) {
	tests := []struct {
		in    string
		want  Bandwidth
		units string
	}{
		{in: "1bps", want: 1},
		{in: "1kbps", want: Kbps},
		{in: "1Mbps", want: Mbps},
		{in: "1Gbps", want: Gbps},
		{in: "100Mbps", want: 100 * Mbps},
		{in: "1Gbps", want: 1 * Gbps},
		{in: "1gbps", want: 1 * Gbps},
		{in: "1GBPS", want: 1 * Gbps},
		{in: " 10Mbps ", want: 10 * Mbps},
		{in: "18446744073709551615bps", want: Bandwidth(18446744073709551615)},
		{in: "1000kbps", want: 1 * Mbps},
	}

	for _, tc := range tests {
		got, err := ParseBandwidth(tc.in)
		if err != nil {
			t.Fatalf("ParseBandwidth(%q) = %v, want nil", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("ParseBandwidth(%q) = %d bits/s, want %d bits/s", tc.in, got, tc.want)
		}
	}
}

func TestParseBandwidthRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		in       string
		wantKind apperr.Kind
	}{
		{in: "", wantKind: apperr.KindInvalidArgument},
		{in: "   ", wantKind: apperr.KindInvalidArgument},
		{in: "fast", wantKind: apperr.KindInvalidArgument},
		{in: "10", wantKind: apperr.KindInvalidArgument},
		{in: "10G", wantKind: apperr.KindInvalidArgument},
		{in: "10Tbps", wantKind: apperr.KindInvalidArgument},
		{in: "10bps;", wantKind: apperr.KindInvalidArgument},
		{in: "Gbps", wantKind: apperr.KindInvalidArgument},
		{in: "0bps", wantKind: apperr.KindInvalidArgument},
		{in: "-1Mbps", wantKind: apperr.KindInvalidArgument},
		{in: "1.5Mbps", wantKind: apperr.KindInvalidArgument},
		{in: "1e3Mbps", wantKind: apperr.KindInvalidArgument},
		{in: "abcMbps", wantKind: apperr.KindInvalidArgument},
		{in: "18446744073709551616bps", wantKind: apperr.KindLimitExceeded},
		{in: "99999999999999999999Tbps", wantKind: apperr.KindLimitExceeded},
		{in: "18446744073709551615kbps", wantKind: apperr.KindLimitExceeded},
	}

	for _, tc := range tests {
		got, err := ParseBandwidth(tc.in)
		if err == nil {
			t.Fatalf("ParseBandwidth(%q) = %d, want error", tc.in, got)
		}
		if !apperr.IsKind(err, tc.wantKind) {
			t.Fatalf("ParseBandwidth(%q) = %v, want kind %q", tc.in, err, tc.wantKind)
		}
	}
}

func TestBandwidthString(t *testing.T) {
	tests := map[Bandwidth]string{
		0:                      "0bps",
		1:                      "1bps",
		999:                    "999bps",
		Kbps:                   "1kbps",
		1500:                   "1500bps",
		Mbps:                   "1Mbps",
		100 * Mbps:             "100Mbps",
		1234 * Kbps:            "1234kbps",
		Gbps:                   "1Gbps",
		CeilingBandwidth:       "100Gbps",
		Gbps + Mbps:            "1001Mbps",
		Gbps + 1500:            "1000001500bps",
		(100 * Gbps) + 1:       "100000000001bps",
		Bandwidth(1500 * Kbps): "1500kbps",
	}

	for bandwidth, want := range tests {
		if got := bandwidth.String(); got != want {
			t.Fatalf("Bandwidth(%d).String() = %q, want %q", uint64(bandwidth), got, want)
		}
	}
}

func TestBandwidthStringRoundTrips(t *testing.T) {
	values := []Bandwidth{1, 999, Kbps, 1500, Mbps, 100 * Mbps, Gbps, 100 * Gbps}
	for _, value := range values {
		text := value.String()
		parsed, err := ParseBandwidth(text)
		if err != nil {
			t.Fatalf("ParseBandwidth(%q) = %v, want nil", text, err)
		}
		if parsed != value {
			t.Fatalf("round trip of %d via %q produced %d", uint64(value), text, uint64(parsed))
		}
	}
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		in   string
		want Duration
	}{
		{in: "1s", want: Duration(time.Second)},
		{in: "30s", want: Duration(30 * time.Second)},
		{in: "10m", want: Duration(10 * time.Minute)},
		{in: "1h30m", want: Duration(90 * time.Minute)},
		{in: "500ms", want: Duration(500 * time.Millisecond)},
		{in: "0s", want: 0},
	}

	for _, tc := range tests {
		got, err := ParseDuration(tc.in)
		if err != nil {
			t.Fatalf("ParseDuration(%q) = %v, want nil", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("ParseDuration(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestParseDurationRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		in       string
		wantKind apperr.Kind
	}{
		{in: "", wantKind: apperr.KindInvalidConfig},
		{in: "10", wantKind: apperr.KindInvalidConfig},
		{in: "ten seconds", wantKind: apperr.KindInvalidConfig},
		{in: "10x", wantKind: apperr.KindInvalidConfig},
		{in: "-10m", wantKind: apperr.KindInvalidArgument},
		{in: "-1s", wantKind: apperr.KindInvalidArgument},
	}

	for _, tc := range tests {
		got, err := ParseDuration(tc.in)
		if err == nil {
			t.Fatalf("ParseDuration(%q) = %s, want error", tc.in, got)
		}
		if !apperr.IsKind(err, tc.wantKind) {
			t.Fatalf("ParseDuration(%q) = %v, want kind %q", tc.in, err, tc.wantKind)
		}
	}
}

func TestDurationStringAndMarshal(t *testing.T) {
	d := Duration(90 * time.Second)
	if got := d.String(); got != "1m30s" {
		t.Fatalf("Duration.String() = %q, want %q", got, "1m30s")
	}
	if got := d.Duration(); got != 90*time.Second {
		t.Fatalf("Duration.Duration() = %s, want 1m30s", got)
	}

	marshalled, err := d.MarshalYAML()
	if err != nil {
		t.Fatalf("MarshalYAML() = %v, want nil", err)
	}
	if marshalled != "1m30s" {
		t.Fatalf("MarshalYAML() = %v, want %q", marshalled, "1m30s")
	}
}

func TestBandwidthMarshal(t *testing.T) {
	marshalled, err := Gbps.MarshalYAML()
	if err != nil {
		t.Fatalf("MarshalYAML() = %v, want nil", err)
	}
	if marshalled != "1Gbps" {
		t.Fatalf("MarshalYAML() = %v, want %q", marshalled, "1Gbps")
	}
}

func TestDefaultLimitsAreWithinCeilings(t *testing.T) {
	defaults := DefaultLimits()
	if defaults.MaxNamespaces > CeilingNamespaces {
		t.Fatalf("default max_namespaces %d exceeds the ceiling %d", defaults.MaxNamespaces, CeilingNamespaces)
	}
	if defaults.MaxInterfaces > CeilingInterfaces {
		t.Fatalf("default max_interfaces %d exceeds the ceiling %d", defaults.MaxInterfaces, CeilingInterfaces)
	}
	if defaults.MaxDuration.Duration() > CeilingDuration {
		t.Fatalf("default max_duration %s exceeds the ceiling %s", defaults.MaxDuration, CeilingDuration)
	}
	if defaults.MaxBandwidth > CeilingBandwidth {
		t.Fatalf("default max_bandwidth %s exceeds the ceiling %s", defaults.MaxBandwidth, CeilingBandwidth)
	}
	if defaults.MaxPacketRate > CeilingPacketRate {
		t.Fatalf("default max_packet_rate %d exceeds the ceiling %d", defaults.MaxPacketRate, CeilingPacketRate)
	}
	if defaults.MaxConcurrentProbes > CeilingConcurrentProbes {
		t.Fatalf("default max_concurrent_probes %d exceeds the ceiling %d", defaults.MaxConcurrentProbes, CeilingConcurrentProbes)
	}
}

func TestNameValidationBounds(t *testing.T) {
	tests := []struct {
		value string
		valid bool
	}{
		{value: "a", valid: true},
		{value: "0", valid: true},
		{value: "client", valid: true},
		{value: "client-1", valid: true},
		{value: "service-a", valid: true},
		{value: strings.Repeat("a", MaxNameLength), valid: true},
		{value: strings.Repeat("a", MaxNameLength+1), valid: false},
		{value: "-client", valid: false},
		{value: "client-", valid: false},
		{value: "", valid: false},
		{value: "Client", valid: false},
		{value: "cl_ient", valid: false},
		{value: "cl ient", valid: false},
		{value: "cl.ient", valid: false},
		{value: "cl/ient", valid: false},
		{value: "../etc", valid: false},
		{value: "cl ient\nrm -rf /", valid: false},
		{value: "client;nsenter", valid: false},
		{value: "$HOME", valid: false},
		{value: "cl\x00ient", valid: false},
		{value: "клиент", valid: false},
	}

	for _, tc := range tests {
		errs := checkName("config.validate", "name", tc.value)
		if tc.valid && len(errs) != 0 {
			t.Fatalf("checkName(%q) = %v, want valid", tc.value, errs)
		}
		if !tc.valid && len(errs) == 0 {
			t.Fatalf("checkName(%q) = valid, want error", tc.value)
		}
	}
}
