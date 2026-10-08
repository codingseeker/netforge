package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newTestLogger(t *testing.T, opts Options) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	logger, err := New(buf, opts)
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	return logger, buf
}

func TestParseLevel(t *testing.T) {
	tests := []struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		{in: "debug", want: slog.LevelDebug},
		{in: "info", want: slog.LevelInfo},
		{in: "", want: slog.LevelInfo},
		{in: "INFO", want: slog.LevelInfo},
		{in: " warn ", want: slog.LevelWarn},
		{in: "warning", want: slog.LevelWarn},
		{in: "error", want: slog.LevelError},
		{in: "trace", wantErr: true},
		{in: "-4", wantErr: true},
	}

	for _, tc := range tests {
		got, err := ParseLevel(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("ParseLevel(%q) = %v, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ParseLevel(%q) = %v, want nil", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("ParseLevel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseFormat(t *testing.T) {
	tests := []struct {
		in      string
		want    Format
		wantErr bool
	}{
		{in: "text", want: FormatText},
		{in: "", want: FormatText},
		{in: "JSON", want: FormatJSON},
		{in: "json", want: FormatJSON},
		{in: "logfmt", wantErr: true},
	}

	for _, tc := range tests {
		got, err := ParseFormat(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("ParseFormat(%q) = %q, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ParseFormat(%q) = %v, want nil", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("ParseFormat(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(nil, Options{}); err == nil {
		t.Fatal("New(nil) = nil error, want error")
	}
	if _, err := New(&bytes.Buffer{}, Options{Format: Format("xml")}); err == nil {
		t.Fatal("New(unsupported format) = nil error, want error")
	}
}

func TestJSONRecordShape(t *testing.T) {
	logger, buf := newTestLogger(t, Options{Level: slog.LevelInfo, Format: FormatJSON, Component: DefaultComponent})
	logger.Info("namespace created", slog.String("namespace", "client"), slog.Int("mtu", 1500))

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("Unmarshal(%q) = %v, want valid JSON", buf.String(), err)
	}

	for _, key := range []string{"timestamp", "level", "event", FieldComponent, "namespace", "mtu"} {
		if _, ok := record[key]; !ok {
			t.Fatalf("record %v is missing key %q", record, key)
		}
	}
	if got := record[FieldEvent]; got != "namespace created" {
		t.Fatalf("%s = %v, want %q", FieldEvent, got, "namespace created")
	}
	if got := record["level"]; got != "info" {
		t.Fatalf("level = %v, want %q", got, "info")
	}
	if got := record[FieldComponent]; got != DefaultComponent {
		t.Fatalf("%s = %v, want %q", FieldComponent, got, DefaultComponent)
	}
	timestamp, ok := record["timestamp"].(string)
	if !ok {
		t.Fatalf("timestamp = %v, want string", record["timestamp"])
	}
	if _, err := time.Parse(time.RFC3339Nano, timestamp); err != nil {
		t.Fatalf("timestamp %q is not RFC3339: %v", timestamp, err)
	}
}

func TestTextRecordShape(t *testing.T) {
	logger, buf := newTestLogger(t, Options{Level: slog.LevelInfo, Format: FormatText, Component: DefaultComponent})
	logger.Error("route installation failed")

	out := buf.String()
	for _, want := range []string{"timestamp=", "level=error", "event=", FieldComponent + "="} {
		if !strings.Contains(out, want) {
			t.Fatalf("output %q is missing %q", out, want)
		}
	}
}

func TestLevelFiltering(t *testing.T) {
	logger, buf := newTestLogger(t, Options{Level: slog.LevelWarn, Format: FormatJSON})
	logger.Debug("debug event")
	logger.Info("info event")
	logger.Warn("warn event")
	logger.Error("error event")

	out := buf.String()
	if strings.Contains(out, "debug event") || strings.Contains(out, "info event") {
		t.Fatalf("output %q contains records below the configured level", out)
	}
	if !strings.Contains(out, "warn event") || !strings.Contains(out, "error event") {
		t.Fatalf("output %q is missing records at or above the configured level", out)
	}
}

func TestComponentAndExperimentContext(t *testing.T) {
	logger, buf := newTestLogger(t, Options{Level: slog.LevelInfo, Format: FormatJSON, Component: "control"})
	scoped := WithExperiment(WithComponent(logger, "network"), "exp-0001")
	scoped.Info("fault injected", slog.String("fault_id", "f-1"))

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("Unmarshal(%q) = %v, want valid JSON", buf.String(), err)
	}
	if got := record[FieldComponent]; got != "network" {
		t.Fatalf("%s = %v, want %q", FieldComponent, got, "network")
	}
	if got := record[FieldExperimentID]; got != "exp-0001" {
		t.Fatalf("%s = %v, want %q", FieldExperimentID, got, "exp-0001")
	}
	if got := record["fault_id"]; got != "f-1" {
		t.Fatalf("fault_id = %v, want %q", got, "f-1")
	}
}

func TestGroupedAttributesArePreserved(t *testing.T) {
	logger, buf := newTestLogger(t, Options{Level: slog.LevelInfo, Format: FormatJSON})
	logger.Info("probe complete", slog.Group("target", slog.String("address", "10.42.0.2:8080")))

	if !strings.Contains(buf.String(), `"target":{"address"`) {
		t.Fatalf("output %q does not preserve the attribute group", buf.String())
	}
}

func TestNilLoggerHelpers(t *testing.T) {
	if WithComponent(nil, "x") != nil {
		t.Fatal("WithComponent(nil) != nil")
	}
	if WithExperiment(nil, "x") != nil {
		t.Fatal("WithExperiment(nil) != nil")
	}
}

func TestComponentIsOptional(t *testing.T) {
	logger, buf := newTestLogger(t, Options{Level: slog.LevelInfo, Format: FormatJSON})
	logger.Info("event")

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("Unmarshal(%q) = %v, want valid JSON", buf.String(), err)
	}
	if _, ok := record[FieldComponent]; ok {
		t.Fatalf("record %v has a component even though none was configured", record)
	}
}

func TestDefaultFormatIsText(t *testing.T) {
	logger, buf := newTestLogger(t, Options{})
	logger.Info("event")

	out := buf.String()
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Fatalf("default output %q looks like JSON, want text", out)
	}
	if !strings.Contains(out, "event=event") {
		t.Fatalf("output %q is missing the event field", out)
	}
}

func TestARecordCarriesExactlyOneEventKey(t *testing.T) {
	logger, buf := newTestLogger(t, Options{Level: slog.LevelInfo, Format: FormatJSON})
	logger.Info("command dispatch",
		slog.String(FieldEvent, "dispatch"),
		slog.String("component", "experiment"))

	if got := strings.Count(buf.String(), `"event":`); got != 1 {
		t.Fatalf("the record carries %d event keys, want exactly one: %s", got, buf.String())
	}

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("Unmarshal(%q) = %v, want valid JSON", buf.String(), err)
	}
	if got := record[FieldEvent]; got != "command dispatch" {
		t.Fatalf("%s = %v, want the message, which is the record's one event name", FieldEvent, got)
	}
	if got := record[FieldComponent]; got != "experiment" {
		t.Fatalf("%s = %v, want experiment", FieldComponent, got)
	}
}

func TestATextRecordCarriesExactlyOneEventKey(t *testing.T) {
	logger, buf := newTestLogger(t, Options{Level: slog.LevelInfo, Format: FormatText})
	logger.Info("command dispatch", slog.String(FieldEvent, "dispatch"))

	out := buf.String()
	if got := strings.Count(out, "event="); got != 1 {
		t.Fatalf("the record carries %d event keys, want exactly one: %s", got, out)
	}
	if !strings.Contains(out, "event="+quoteIfNeeded("command dispatch")) {
		t.Fatalf("output %q does not carry the message as the event name", out)
	}
}

func quoteIfNeeded(value string) string {
	if strings.ContainsAny(value, " \t") {
		return strconv.Quote(value)
	}
	return value
}
