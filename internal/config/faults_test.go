package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/chaos"
)

// loadFaults parses a document that has faults in it, with the surrounding
// configuration filled in so only the fault itself can be at fault.
func loadFaults(t *testing.T, faults string) (*Config, error) {
	t.Helper()
	document := "name: f\nscope:\n  network: 10.42.0.0/16\n  namespaces:\n    - client\n    - server\n" +
		"limits:\n  max_duration: 5m\n  max_bandwidth: 1Gbps\n" + faults
	return Parse([]byte(document))
}

func TestFaultsLoadEveryType(t *testing.T) {
	cfg, err := Load(filepath.Join("testdata", "faults.yaml"))
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	if len(cfg.Faults) != 6 {
		t.Fatalf("got %d faults, want 6", len(cfg.Faults))
	}

	want := []struct {
		kind      chaos.Kind
		direction chaos.Direction
		latency   time.Duration
		jitter    time.Duration
		loss      float64
		dup       float64
		reorder   float64
		gap       int
		rate      uint64
		queue     int
	}{
		{kind: chaos.KindLatency, direction: chaos.DirectionOut, latency: 60 * time.Millisecond, queue: 1000},
		{kind: chaos.KindJitter, direction: chaos.DirectionOut, latency: 60 * time.Millisecond, jitter: 30 * time.Millisecond, queue: 1000},
		{kind: chaos.KindPacketLoss, direction: chaos.DirectionOut, loss: 10, queue: 1000},
		{kind: chaos.KindBandwidth, direction: chaos.DirectionOut, rate: 200_000},
		{kind: chaos.KindDuplication, direction: chaos.DirectionIn, dup: 5, queue: 1000},
		{kind: chaos.KindReordering, direction: chaos.DirectionIn, reorder: 20, latency: 40 * time.Millisecond, gap: 3, queue: 500},
	}
	for i, w := range want {
		spec, err := cfg.Faults[i].Translate(cfg.Limits)
		if err != nil {
			t.Fatalf("faults[%d].Translate() = %v", i, err)
		}
		if spec.Kind != w.kind {
			t.Errorf("faults[%d] kind = %q, want %q", i, spec.Kind, w.kind)
		}
		if spec.Direction != w.direction {
			t.Errorf("faults[%d] direction = %q, want %q", i, spec.Direction, w.direction)
		}
		p := spec.Parameters
		if time.Duration(p.Latency) != w.latency {
			t.Errorf("faults[%d] latency = %s, want %s", i, time.Duration(p.Latency), w.latency)
		}
		if time.Duration(p.Jitter) != w.jitter {
			t.Errorf("faults[%d] jitter = %s, want %s", i, time.Duration(p.Jitter), w.jitter)
		}
		if float64(p.Loss) != w.loss {
			t.Errorf("faults[%d] loss = %v, want %v", i, p.Loss, w.loss)
		}
		if float64(p.Duplication) != w.dup {
			t.Errorf("faults[%d] duplication = %v, want %v", i, p.Duplication, w.dup)
		}
		if float64(p.Reordering) != w.reorder {
			t.Errorf("faults[%d] reordering = %v, want %v", i, p.Reordering, w.reorder)
		}
		if p.Gap != w.gap {
			t.Errorf("faults[%d] gap = %d, want %d", i, p.Gap, w.gap)
		}
		if p.Rate != w.rate {
			t.Errorf("faults[%d] rate = %d, want %d", i, p.Rate, w.rate)
		}
		if p.Queue != w.queue {
			t.Errorf("faults[%d] queue = %d, want %d", i, p.Queue, w.queue)
		}
	}
}

func TestFaultsRejectAValueInTheWrongUnit(t *testing.T) {
	cases := map[string]string{
		"latency takes a percent":     "faults:\n  - {target: client, type: latency, value: \"10%\"}\n",
		"latency takes a bandwidth":   "faults:\n  - {target: client, type: latency, value: 1Gbps}\n",
		"bandwidth takes a percent":   "faults:\n  - {target: client, type: bandwidth, value: \"10%\"}\n",
		"loss takes a duration":       "faults:\n  - {target: client, type: packet_loss, value: 10ms}\n",
		"loss takes tc units":         "faults:\n  - {target: client, type: packet_loss, value: 200kbit}\n",
		"duplication takes a percent": "faults:\n  - {target: client, type: duplication, value: 5ms}\n",
	}
	for name, faults := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadFaults(t, faults)
			if err == nil {
				t.Fatal("Parse() = nil error, want the value refused")
			}
			if !strings.Contains(err.Error(), "takes") {
				t.Errorf("got %v, want the refusal to name the unit the type takes", err)
			}
		})
	}
}

func TestFaultsRejectFieldsTheirTypeCannotUse(t *testing.T) {
	cases := map[string]string{
		"latency with a delay":     "faults:\n  - {target: client, type: latency, value: 10ms, delay: 5ms}\n",
		"bandwidth with a jitter":  "faults:\n  - {target: client, type: bandwidth, value: 1Mbps, jitter: 5ms}\n",
		"bandwidth with a gap":     "faults:\n  - {target: client, type: bandwidth, value: 1Mbps, gap: \"2\"}\n",
		"loss with a delay":        "faults:\n  - {target: client, type: packet_loss, value: \"10%\", delay: 5ms}\n",
		"duplication with a gap":   "faults:\n  - {target: client, type: duplication, value: \"10%\", gap: \"2\"}\n",
		"jitter without a delay":   "faults:\n  - {target: client, type: jitter, value: 10ms}\n",
		"reordering without delay": "faults:\n  - {target: client, type: reordering, value: \"10%\"}\n",
	}
	for name, faults := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadFaults(t, faults)
			if err == nil {
				t.Fatal("Parse() = nil error, want the field refused")
			}
			if !strings.Contains(err.Error(), "fault") {
				t.Errorf("got %v, want the refusal to talk about the fault", err)
			}
		})
	}
}

func TestFaultsRejectAnUnknownFieldOrType(t *testing.T) {
	_, err := loadFaults(t, "faults:\n  - {target: client, type: latency, value: 10ms, evil: 1}\n")
	if err == nil || !strings.Contains(err.Error(), "no field") {
		t.Errorf("got %v, want an unknown fault field to be refused", err)
	}
	_, err = loadFaults(t, "faults:\n  - {target: client, type: corruption, value: \"10%\"}\n")
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("got %v, want an unknown fault type to be refused", err)
	}
	_, err = loadFaults(t, "faults:\n  - {target: client, type: latency, value: 10ms}\nfaults:\n  - {}\n")
	if err == nil {
		t.Error("a repeated faults key must be refused")
	}
}

func TestFaultsRejectATargetOutsideTheScope(t *testing.T) {
	_, err := loadFaults(t, "faults:\n  - {target: host, type: latency, value: 10ms}\n")
	if err == nil || !strings.Contains(err.Error(), "scope") {
		t.Errorf("got %v, want a target outside the scope refused", err)
	}
}

func TestFaultsRejectTwoFaultsOnOneDevice(t *testing.T) {
	_, err := loadFaults(t, "faults:\n  - {target: client, type: latency, value: 10ms}\n"+
		"  - {target: client, type: packet_loss, value: \"10%\"}\n")
	if err == nil || !strings.Contains(err.Error(), "one device carries at most one fault") {
		t.Errorf("got %v, want two faults on one device refused", err)
	}
	// The two ends of one link are two devices, so this pair is fine.
	if _, err := loadFaults(t, "faults:\n  - {target: client, type: latency, value: 10ms}\n"+
		"  - {target: client, direction: \"in\", type: packet_loss, value: \"10%\"}\n"); err != nil {
		t.Errorf("a fault and its opposite direction must both fit, got %v", err)
	}
}

func TestFaultsAreHeldToTheDeclaredLimits(t *testing.T) {
	document := "name: f\nscope:\n  network: 10.42.0.0/16\n  namespaces:\n    - client\n" +
		"limits:\n  max_duration: 30s\n  max_bandwidth: 1Mbps\n"
	_, err := Parse([]byte(document + "faults:\n  - {target: client, type: latency, value: 10ms, duration: 5m}\n"))
	if err == nil || !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Errorf("got %v, want a fault longer than max_duration refused", err)
	}
	_, err = Parse([]byte(document + "faults:\n  - {target: client, type: bandwidth, value: 10Mbps}\n"))
	if err == nil || !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Errorf("got %v, want a fault above max_bandwidth refused", err)
	}
}

func TestValidateRejectsTooManyFaults(t *testing.T) {
	document := "name: f\nscope:\n  network: 10.42.0.0/16\n  namespaces:\n    - client\nfaults:\n"
	for i := 0; i <= CeilingFaults; i++ {
		// Distinct directions keep the faults off each other's devices, so
		// the count is the only thing wrong.
		direction := "out"
		if i%2 == 1 {
			direction = "in"
		}
		document += "  - {target: client, type: latency, value: 10ms, direction: \"" + direction + "\"}\n"
	}
	_, err := Parse([]byte(document))
	if err == nil || !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Fatalf("got %v, want %d faults to be refused", err, CeilingFaults+1)
	}
	if !strings.Contains(err.Error(), "faults") {
		t.Errorf("got %v, want the refusal to name the fault count", err)
	}
}

func TestFaultsMarshalBackToAReadableFile(t *testing.T) {
	cfg, err := Load(filepath.Join("testdata", "faults.yaml"))
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	rendered, err := Marshal(*cfg)
	if err != nil {
		t.Fatalf("Marshal() = %v", err)
	}
	reparsed, err := Parse(rendered)
	if err != nil {
		t.Fatalf("Parse(Marshal()) = %v\n%s", err, rendered)
	}
	if len(reparsed.Faults) != len(cfg.Faults) {
		t.Fatalf("round trip changed the fault count: %d vs %d", len(reparsed.Faults), len(cfg.Faults))
	}
	for i := range cfg.Faults {
		want, err := cfg.Faults[i].Translate(cfg.Limits)
		if err != nil {
			t.Fatal(err)
		}
		got, err := reparsed.Faults[i].Translate(reparsed.Limits)
		if err != nil {
			t.Fatalf("faults[%d] did not survive the round trip: %v", i, err)
		}
		if got.Kind != want.Kind || got.Parameters != want.Parameters || got.Direction != want.Direction {
			t.Errorf("faults[%d] changed: %+v vs %+v", i, got.Parameters, want.Parameters)
		}
	}
}

func TestPercentParsing(t *testing.T) {
	for _, raw := range []string{"1%", "10%", "50%", "100%"} {
		parsed, err := ParsePercent(raw)
		if err != nil {
			t.Errorf("ParsePercent(%q) = %v", raw, err)
			continue
		}
		if parsed.String() != raw {
			t.Errorf("ParsePercent(%q).String() = %q", raw, parsed.String())
		}
	}
	for _, raw := range []string{
		"", "%", "10", "0%", "101%", "-10%", "1e2%", "10 %", "10%%", "10.5%",
		"١٠%", "+10%", "99999999999999999999999%",
	} {
		if parsed, err := ParsePercent(raw); err == nil {
			t.Errorf("ParsePercent(%q) = %d, want it refused", raw, parsed)
		}
	}
}
