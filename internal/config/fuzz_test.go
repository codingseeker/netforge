package config

import (
	"bytes"
	"runtime"
	"strings"
	"testing"

	"github.com/codingseeker/netforge/internal/apperr"
)

func FuzzParse(f *testing.F) {
	seeds := []string{
		"",
		"{}",
		"null",
		"- item\n",
		"name: fuzz\nscope:\n  network: 10.42.0.0/16\n  namespaces:\n    - client\n",
		"name: fuzz\nseed: 1\n",
		"name: fuzz\nseed: -1\n",
		"name: fuzz\nlimits:\n  max_duration: 10m\n  max_bandwidth: 1Gbps\n",
		"name: fuzz\nlimits:\n  max_duration: -1m\n",
		"name: fuzz\nlimits:\n  max_bandwidth: 99999999999999999999999Gbps\n",
		"name: [a, b]\n",
		"name: fuzz\nunknown: 1\n",
		"a: &a [1, 2]\nb: *a\n",
		"a: &a [1, 2]\nb: &b [*a, *a, *a]\nc: &c [*b, *b, *b]\nd: &d [*c, *c, *c]\n",
		"name: \"\\u0000\"\n",
		"name: fuzz\nscope:\n  network: 0.0.0.0/0\n  namespaces: [client]\n",
		"---\nname: a\n---\nname: b\n",
		"name: fuzz\nseed: 18446744073709551615\n",
		"name: fuzz\nseed: 18446744073709551616\n",
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		cfg, err := Parse(data)
		if err != nil {
			if cfg != nil {
				t.Fatalf("Parse() returned a configuration and an error: %+v, %v", cfg, err)
			}
			switch {
			case apperr.IsKind(err, apperr.KindInvalidConfig),
				apperr.IsKind(err, apperr.KindInvalidArgument),
				apperr.IsKind(err, apperr.KindValidation),
				apperr.IsKind(err, apperr.KindLimitExceeded),
				apperr.IsKind(err, apperr.KindUnsupported),
				apperr.IsKind(err, apperr.KindInternal):
			default:
				t.Fatalf("Parse() returned an unclassified error %v", err)
			}
			return
		}

		if err := cfg.Validate(); err != nil {
			t.Fatalf("Parse() accepted %q that fails Validate(): %v", data, err)
		}
		if len(cfg.Name) > MaxNameLength {
			t.Fatalf("Parse() accepted a name of length %d", len(cfg.Name))
		}
		if len(cfg.NamespaceNames()) > CeilingNamespaces {
			t.Fatalf("Parse() accepted %d namespaces", len(cfg.NamespaceNames()))
		}
		if _, err := ParseNetwork(cfg.Network()); err != nil {
			t.Fatalf("Parse() accepted the invalid network %q: %v", cfg.Network(), err)
		}
		if cfg.Limits.MaxNamespaces > CeilingNamespaces ||
			cfg.Limits.MaxInterfaces > CeilingInterfaces ||
			cfg.Limits.MaxDuration.Duration() > CeilingDuration ||
			cfg.Limits.MaxBandwidth > CeilingBandwidth ||
			cfg.Limits.MaxPacketRate > CeilingPacketRate ||
			cfg.Limits.MaxConcurrentProbes > CeilingConcurrentProbes {
			t.Fatalf("Parse() accepted limits above the ceilings: %+v", cfg.Limits)
		}

		rendered, err := Marshal(*cfg)
		if err != nil {
			t.Fatalf("Marshal() = %v, want nil", err)
		}
		if len(rendered) > MaxConfigBytes {
			t.Fatalf("Marshal() produced %d bytes, above the maximum", len(rendered))
		}
		reparsed, err := Parse(rendered)
		if err != nil {
			t.Fatalf("Parse(Marshal()) = %v, want nil\n%s", err, rendered)
		}
		if reparsed.Name != cfg.Name || reparsed.Limits != cfg.Limits {
			t.Fatalf("round trip changed the configuration: %+v vs %+v", reparsed, cfg)
		}
	})
}

func FuzzParseBandwidth(f *testing.F) {
	for _, seed := range []string{"", "1Gbps", "1Gb", "-1Mbps", "0bps", "1.5Gbps", "18446744073709551616bps", "bps", "abc"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		value, err := ParseBandwidth(raw)
		if err != nil {
			if value != 0 {
				t.Fatalf("ParseBandwidth(%q) returned %d and an error", raw, value)
			}
			return
		}
		if value == 0 {
			t.Fatalf("ParseBandwidth(%q) returned zero without an error", raw)
		}
		text := value.String()
		if !strings.Contains(text, "bps") {
			t.Fatalf("ParseBandwidth(%q).String() = %q, want a unit suffix", raw, text)
		}
		roundTripped, err := ParseBandwidth(text)
		if err != nil || roundTripped != value {
			t.Fatalf("ParseBandwidth(%q) = %d does not round trip via %q: %d, %v", raw, uint64(value), text, uint64(roundTripped), err)
		}
	})
}

func FuzzParseDuration(f *testing.F) {
	for _, seed := range []string{"", "1s", "-1s", "1h", "1x", "0", "9223372036854775807ns"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		value, err := ParseDuration(raw)
		if err != nil {
			if value != 0 {
				t.Fatalf("ParseDuration(%q) returned %s and an error", raw, value)
			}
			return
		}
		if value < 0 {
			t.Fatalf("ParseDuration(%q) = %s, want a non-negative duration", raw, value)
		}
		reparsed, err := ParseDuration(value.String())
		if err != nil || reparsed != value {
			t.Fatalf("ParseDuration(%q) = %s does not round trip: %s, %v", raw, value, reparsed, err)
		}
	})
}

func FuzzValidateName(f *testing.F) {
	for _, seed := range []string{"", "client", "-client", "Client", "a b", "cl\x00ient", strings.Repeat("a", 100)} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, value string) {
		errs := checkName("config.validate", "name", value)
		if len(errs) == 0 {
			for i := 0; i < len(value); i++ {
				c := value[i]
				if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '-' {
					t.Fatalf("checkName(%q) accepted the character %q", value, c)
				}
			}
			if len(value) > MaxNameLength {
				t.Fatalf("checkName(%q) accepted an over-long name", value)
			}
			if len(value) > 0 && (value[0] == '-' || value[len(value)-1] == '-') {
				t.Fatalf("checkName(%q) accepted a name with a leading or trailing dash", value)
			}
		}
	})
}

func TestParseDoesNotAllocateUnboundedMemory(t *testing.T) {
	document := "name: memory\n" + strings.Repeat(" ", MaxConfigBytes)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	if _, err := Parse([]byte(document)); err == nil {
		t.Fatal("Parse(oversized) = nil error, want error")
	}

	runtime.ReadMemStats(&after)
	grown := after.TotalAlloc - before.TotalAlloc
	if grown > 8*MaxConfigBytes {
		t.Fatalf("Parse(oversized) allocated %d bytes for a %d byte limit", grown, MaxConfigBytes)
	}
}

func TestParseReaderRejectsNilReader(t *testing.T) {
	if _, err := ParseReader(nil, "config.ParseReader"); err == nil {
		t.Fatal("ParseReader(nil) = nil error, want error")
	}
}

func TestParseReaderPropagateReadFailure(t *testing.T) {
	_, err := ParseReader(failingReader{err: errRead}, "config.ParseReader")
	if !apperr.IsKind(err, apperr.KindIO) {
		t.Fatalf("ParseReader(failing) = %v, want io error", err)
	}
}

type failingReader struct {
	err error
}

func (r failingReader) Read([]byte) (int, error) {
	return 0, r.err
}

var errRead = apperr.IO("test", "read failure", nil)

func TestParseReaderAcceptsStream(t *testing.T) {
	document := bytes.NewBufferString("name: streamed\nscope:\n  network: 10.42.0.0/16\n  namespaces:\n    - client\n")
	cfg, err := ParseReader(document, "config.ParseReader")
	if err != nil {
		t.Fatalf("ParseReader() = %v, want nil", err)
	}
	if cfg.Name != "streamed" {
		t.Fatalf("Name = %q, want %q", cfg.Name, "streamed")
	}
}

func FuzzParsePercent(f *testing.F) {
	for _, seed := range []string{"", "0%", "1%", "10%", "100%", "101%", "-1%", "1e2", "10", "%", "10 %", "١٠%", "10%%"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		value, err := ParsePercent(raw)
		if err != nil {
			if value != 0 {
				t.Fatalf("ParsePercent(%q) returned %d and an error", raw, value)
			}
			return
		}
		if value < MinPercent || value > MaxPercent {
			t.Fatalf("ParsePercent(%q) = %d, outside %d..%d", raw, value, MinPercent, MaxPercent)
		}
		roundTripped, err := ParsePercent(value.String())
		if err != nil || roundTripped != value {
			t.Fatalf("ParsePercent(%q) = %d does not round trip via %q: %d, %v",
				raw, value, value.String(), roundTripped, err)
		}
	})
}
