package chaos

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/codingseeker/netforge/internal/apperr"
)

func TestKindsAreClosed(t *testing.T) {
	if len(Kinds()) != 6 {
		t.Fatalf("got %d kinds, want 6", len(Kinds()))
	}
	for _, kind := range Kinds() {
		if !kind.Valid() {
			t.Errorf("kind %q reports itself invalid", kind)
		}
	}
	if Kind("corruption").Valid() {
		t.Error("an unknown kind must not validate")
	}
	if got := Kinds(); len(got) != 6 {
		t.Fatalf("Kinds must return a copy, got %v", got)
	} else {
		got[0] = Kind("mutated")
		if Kinds()[0] == Kind("mutated") {
			t.Error("Kinds leaked its backing array")
		}
	}
}

func TestPercentRejectsNoOpFaults(t *testing.T) {
	for _, valid := range []Percent{0.1, 1, 12.5, 99.9, 100} {
		if !valid.Valid() {
			t.Errorf("%s should be valid", valid)
		}
	}
	for _, invalid := range []Percent{0, -1, 100.1, Percent(1e9)} {
		if invalid.Valid() {
			t.Errorf("%s must be rejected, it cannot impair anything", invalid)
		}
	}
	if got := Percent(12.5).Fraction(); got != 0.125 {
		t.Errorf("got %v, want 0.125", got)
	}
	if got := Percent(12.5).String(); got != "12.5%" {
		t.Errorf("got %q, want 12.5%%", got)
	}
}

func TestDurationMarshalsAsText(t *testing.T) {
	data, err := json.Marshal(Fault{Duration: Duration(90 * time.Second)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"duration":"1m30s"`) {
		t.Errorf("got %s, want a duration string", data)
	}
	var back Fault
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Duration.Std() != 90*time.Second {
		t.Errorf("got %s, want 1m30s", back.Duration)
	}
}

func TestFaultRecordCarriesTheRequiredFields(t *testing.T) {
	fault := Fault{
		ID:          "loss@nf-client0-tc/nfc0:out",
		Experiment:  "exp-1",
		Kind:        KindPacketLoss,
		Status:      StatusActive,
		Start:       time.Unix(0, 0).UTC(),
		Duration:    Duration(time.Minute),
		Source:      Endpoint{Namespace: "nf-client0-tc", Interface: "nfc0"},
		Destination: Endpoint{Namespace: "nf-router-tc", Interface: "nfr-c0"},
		Parameters:  Parameters{Loss: 10},
	}
	data, err := json.Marshal(fault)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, required := range []string{
		"fault_id", "experiment_id", "source", "destination",
		"start_time", "duration", "parameters", "status",
	} {
		if _, ok := fields[required]; !ok {
			t.Errorf("the record is missing the required field %q", required)
		}
	}
}

func TestSpecCheckFillsDefaults(t *testing.T) {
	spec := Spec{
		Target:     "nf-client0-tc",
		Kind:       KindLatency,
		Parameters: Parameters{Latency: Duration(50 * time.Millisecond)},
	}
	checked, err := spec.check(DefaultLimits())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if checked.Direction != DirectionOut {
		t.Errorf("got direction %q, want %q", checked.Direction, DirectionOut)
	}
	if checked.Duration.Std() != DefaultLimits().MaxDuration {
		t.Errorf("got duration %s, want the maximum %s", checked.Duration, DefaultLimits().MaxDuration)
	}
	if checked.Parameters.Queue != DefaultLimits().DefaultQueue {
		t.Errorf("got queue %d, want %d", checked.Parameters.Queue, DefaultLimits().DefaultQueue)
	}
}

func TestSpecCheckDefaultsTheReorderGap(t *testing.T) {
	checked, err := Spec{
		Target:     "nf-client0-tc",
		Kind:       KindReordering,
		Parameters: Parameters{Reordering: 25, Latency: Duration(20 * time.Millisecond)},
	}.check(DefaultLimits())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if checked.Parameters.Gap != 1 {
		t.Errorf("got gap %d, want 1", checked.Parameters.Gap)
	}
}

func TestSpecCheckRejections(t *testing.T) {
	base := func() Spec {
		return Spec{
			Target:     "nf-client0-tc",
			Kind:       KindLatency,
			Parameters: Parameters{Latency: Duration(50 * time.Millisecond)},
		}
	}
	cases := []struct {
		name    string
		mutate  func(*Spec)
		wantErr apperr.Kind
	}{
		{"unknown kind", func(s *Spec) { s.Kind = "corruption" }, apperr.KindValidation},
		{"empty target", func(s *Spec) { s.Target = "" }, apperr.KindValidation},
		{"target with a slash", func(s *Spec) { s.Target = "../host" }, apperr.KindValidation},
		{"target with uppercase", func(s *Spec) { s.Target = "Host" }, apperr.KindValidation},
		{"device with a slash", func(s *Spec) { s.Interface = "eth0/1" }, apperr.KindValidation},
		{"device too long", func(s *Spec) { s.Interface = strings.Repeat("e", 16) }, apperr.KindValidation},
		{"bad direction", func(s *Spec) { s.Direction = "sideways" }, apperr.KindValidation},
		{"negative duration", func(s *Spec) { s.Duration = Duration(-time.Second) }, apperr.KindValidation},
		{"duration over the limit", func(s *Spec) { s.Duration = Duration(time.Hour) }, apperr.KindLimitExceeded},
		{"zero latency", func(s *Spec) { s.Parameters.Latency = 0 }, apperr.KindValidation},
		{"latency over the limit", func(s *Spec) { s.Parameters.Latency = Duration(time.Hour) }, apperr.KindLimitExceeded},
		{"latency carrying jitter", func(s *Spec) { s.Parameters.Jitter = Duration(time.Millisecond) }, apperr.KindValidation},
		{"latency carrying loss", func(s *Spec) { s.Parameters.Loss = 5 }, apperr.KindValidation},
		{"zero loss", func(s *Spec) {
			s.Kind = KindPacketLoss
			s.Parameters = Parameters{Loss: 0}
		}, apperr.KindValidation},
		{"loss over 100", func(s *Spec) {
			s.Kind = KindPacketLoss
			s.Parameters = Parameters{Loss: 101}
		}, apperr.KindValidation},
		{"loss carrying a delay", func(s *Spec) {
			s.Kind = KindPacketLoss
			s.Parameters = Parameters{Loss: 10, Latency: Duration(time.Millisecond)}
		}, apperr.KindValidation},
		{"jitter without jitter", func(s *Spec) { s.Kind = KindJitter }, apperr.KindValidation},
		{"jitter above the delay", func(s *Spec) {
			s.Kind = KindJitter
			s.Parameters = Parameters{Latency: Duration(10 * time.Millisecond), Jitter: Duration(50 * time.Millisecond)}
		}, apperr.KindValidation},
		{"zero rate", func(s *Spec) {
			s.Kind = KindBandwidth
			s.Parameters = Parameters{}
		}, apperr.KindValidation},
		{"rate over the limit", func(s *Spec) {
			s.Kind = KindBandwidth
			s.Parameters = Parameters{Rate: 10_000_000_000}
		}, apperr.KindLimitExceeded},
		{"rate carrying a queue", func(s *Spec) {
			s.Kind = KindBandwidth
			s.Parameters = Parameters{Rate: 1000, Queue: 10}
		}, apperr.KindValidation},
		{"zero duplication", func(s *Spec) {
			s.Kind = KindDuplication
			s.Parameters = Parameters{}
		}, apperr.KindValidation},
		{"reordering without a delay", func(s *Spec) {
			s.Kind = KindReordering
			s.Parameters = Parameters{Reordering: 10}
		}, apperr.KindValidation},
		{"reorder gap over the limit", func(s *Spec) {
			s.Kind = KindReordering
			s.Parameters = Parameters{Reordering: 10, Latency: Duration(time.Millisecond), Gap: 100_000}
		}, apperr.KindLimitExceeded},
		{"queue over the limit", func(s *Spec) { s.Parameters.Queue = 1_000_000 }, apperr.KindLimitExceeded},
		{"fault id with a space", func(s *Spec) { s.ID = "my fault" }, apperr.KindValidation},
		{"fault id too long", func(s *Spec) { s.ID = strings.Repeat("f", 64) }, apperr.KindValidation},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			spec := base()
			test.mutate(&spec)
			_, err := spec.check(DefaultLimits())
			if err == nil {
				t.Fatal("the specification was accepted")
			}
			if !apperr.IsKind(err, test.wantErr) {
				t.Errorf("got %v, want kind %s", err, test.wantErr)
			}
		})
	}
}

func TestSpecCheckAcceptsEveryKind(t *testing.T) {
	cases := map[Kind]Parameters{
		KindLatency:     {Latency: Duration(50 * time.Millisecond)},
		KindJitter:      {Latency: Duration(50 * time.Millisecond), Jitter: Duration(5 * time.Millisecond), Correlation: 25},
		KindPacketLoss:  {Loss: 10, Correlation: 30},
		KindBandwidth:   {Rate: 200_000},
		KindDuplication: {Duplication: 20},
		KindReordering:  {Reordering: 25, Latency: Duration(20 * time.Millisecond), Gap: 3, Queue: 500},
	}
	for kind, parameters := range cases {
		t.Run(string(kind), func(t *testing.T) {
			spec := Spec{Target: "nf-client0-tc", Kind: kind, Parameters: parameters}
			if _, err := spec.check(DefaultLimits()); err != nil {
				t.Fatalf("check: %v", err)
			}
		})
	}
}

func TestLimitsCheck(t *testing.T) {
	t.Run("defaults are accepted", func(t *testing.T) {
		if _, err := DefaultLimits().check(); err != nil {
			t.Fatalf("the default limits must be valid: %v", err)
		}
	})
	t.Run("ceilings cannot be raised", func(t *testing.T) {
		cases := map[string]Limits{
			"max_faults":    {MaxFaults: CeilingFaults + 1},
			"max_duration":  {MaxDuration: CeilingDuration + time.Second},
			"max_latency":   {MaxLatency: CeilingLatency + time.Second},
			"max_bandwidth": {MaxBandwidth: CeilingBandwidth + 1},
			"max_queue":     {MaxQueue: CeilingQueue + 1},
			"max_gap":       {MaxGap: CeilingGap + 1},
		}
		for name, limits := range cases {
			t.Run(name, func(t *testing.T) {
				if _, err := limits.check(); err == nil {
					t.Fatalf("a %s above the ceiling was accepted", name)
				} else if !apperr.IsKind(err, apperr.KindLimitExceeded) {
					t.Errorf("got %v, want a limit error", err)
				}
			})
		}
	})
	t.Run("negative limits are rejected", func(t *testing.T) {
		if _, err := (Limits{MaxFaults: -1}).check(); err == nil {
			t.Fatal("a negative fault limit was accepted")
		}
	})
	t.Run("a default queue above the maximum is rejected", func(t *testing.T) {
		_, err := Limits{MaxQueue: 10, DefaultQueue: 20}.check()
		if err == nil {
			t.Fatal("an inconsistent queue limit was accepted")
		}
	})
}

func TestScopeCheck(t *testing.T) {
	valid := Scope{Network: mustPrefix(t, "10.42.0.0/24"), Namespaces: []string{"nf-client0-tc", "nf-router-tc"}}
	if err := valid.check(); err != nil {
		t.Fatalf("a laboratory scope must be valid: %v", err)
	}
	if !valid.allows("nf-router-tc") || valid.allows("host") {
		t.Error("the scope admitted the wrong namespaces")
	}
	cases := map[string]Scope{
		"no network":                         {Namespaces: []string{"nf-client0-tc"}},
		"no namespaces":                      {Network: mustPrefix(t, "10.42.0.0/24")},
		"duplicate":                          {Network: mustPrefix(t, "10.42.0.0/24"), Namespaces: []string{"a-x", "a-x"}},
		"host namespace":                     {Network: mustPrefix(t, "10.42.0.0/24"), Namespaces: []string{"host"}},
		"empty name":                         {Network: mustPrefix(t, "10.42.0.0/24"), Namespaces: []string{""}},
		"namespace is not a laboratory name": {Network: mustPrefix(t, "10.42.0.0/24"), Namespaces: []string{"../../host"}},
	}
	for name, scope := range cases {
		t.Run(name, func(t *testing.T) {
			if err := scope.check(); err == nil {
				t.Fatal("the scope was accepted")
			}
		})
	}
}

func mustPrefix(t *testing.T, text string) netip.Prefix {
	t.Helper()
	prefix, err := netip.ParsePrefix(text)
	if err != nil {
		t.Fatalf("parse %q: %v", text, err)
	}
	return prefix
}
