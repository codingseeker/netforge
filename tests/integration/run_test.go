package integration

import (
	"encoding/json"
	"strings"
	"testing"
)

// The tests here run the Phase 11 report end to end, through the binary: netforge
// experiment run builds a real laboratory from a document, measures the path
// before and after impairing it, performs the authorized repair, and writes the
// report the phase's ten metrics describe, as JSON and as text. Nothing is
// stubbed, so what is checked is the report a real run makes.

// The laboratory identifier a Phase 11 run is pinned to. The namespaces a
// laboratory creates are nf-<role>-<identifier>, so a document grants them
// before the run exists and the flags name them.
const phase11LabID = "nf-ph11"

var (
	phase11Router  = "nf-router-" + phase11LabID
	phase11Client  = "nf-client0-" + phase11LabID
	phase11Server  = "nf-server0-" + phase11LabID
	phase11Address = "10.42.0.6"
)

// writtenReport is the JSON report of a run, parsed into the parts the phase's
// claims rest on.
type writtenReport struct {
	experimentID string
	succeeded    bool
	cleanedUp    bool
	steps        []stepRecord
	faults       []faultRecord
	recoveries   []recoveryRecord
	metrics      metricsRecord
	baseline     []measurementRecord
	observed     []measurementRecord
}

type stepRecord struct {
	state    string
	detail   string
	duration string
}

type faultRecord struct {
	id     string
	kind   string
	target string
	status string
}

type recoveryRecord struct {
	action string
	target string
	passed bool
	reason string
	result string
}

type metricsRecord struct {
	packetsSent        int
	bytes              int
	packetLoss         float64
	availability       float64
	retransmissions    int
	throughput         uint64
	recoveryTime       string
	diagnosticDuration string
	phases             []any
}

type measurementRecord struct {
	sent int
	loss float64
}

// parseReport decodes a JSON report. It consumes the fields the phase says a
// report must carry, so a run that wrote less than that fails the parse and the
// test with it.
func parseReport(t *testing.T, stdout string) writtenReport {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		t.Fatalf("the run did not write JSON: %v\n%s", err, stdout)
	}
	report := writtenReport{
		experimentID: stringOf(t, raw, "experiment_id"),
		succeeded:    boolOf(t, raw, "succeeded"),
		cleanedUp:    boolOf(t, raw, "cleaned_up"),
		metrics:      parseMetrics(t, raw["metrics"]),
		baseline:     parseMeasurements(t, raw["baseline"]),
		observed:     parseMeasurements(t, raw["observed"]),
	}
	for _, entry := range sliceOf(t, raw, "steps") {
		step, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("a lifecycle step is %T, want an object", entry)
		}
		report.steps = append(report.steps, stepRecord{state: stringOf(t, step, "state")})
	}
	for _, entry := range sliceOf(t, raw, "faults") {
		fault, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("a fault record is %T, want an object", entry)
		}
		report.faults = append(report.faults, faultRecord{
			id:     stringOf(t, fault, "fault_id"),
			kind:   stringOf(t, fault, "type"),
			target: stringOf(t, fault, "target"),
			status: stringOf(t, fault, "status"),
		})
	}
	for _, entry := range sliceOf(t, raw, "recoveries") {
		recovery, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("a recovery record is %T, want an object", entry)
		}
		report.recoveries = append(report.recoveries, recoveryRecord{
			action: stringOf(t, recovery, "action"),
			target: stringOf(t, recovery, "target"),
			passed: boolOf(t, recovery, "passed"),
			reason: stringOf(t, recovery, "reason"),
			result: stringOf(t, recovery, "result"),
		})
	}
	if report.succeeded && hasField(raw, "error") {
		t.Errorf("a successful run carried an error field: %q", raw["error"])
	}
	return report
}

func parseMetrics(t *testing.T, raw any) metricsRecord {
	t.Helper()
	metrics, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("the report's metrics are %T, want an object", raw)
	}
	record := metricsRecord{
		packetsSent:        intOf(t, metrics, "packets_sent"),
		bytes:              intOf(t, metrics, "bytes"),
		packetLoss:         numberOf(t, metrics, "packet_loss"),
		availability:       numberOf(t, metrics, "availability"),
		retransmissions:    intOf(t, metrics, "retransmissions"),
		throughput:         uint64Of(t, metrics, "throughput"),
		recoveryTime:       stringOf(t, metrics, "recovery_time"),
		diagnosticDuration: stringOf(t, metrics, "diagnostic_duration"),
	}
	if phases, ok := metrics["phases"].([]any); ok {
		record.phases = phases
	}
	return record
}

func parseMeasurements(t *testing.T, raw any) []measurementRecord {
	t.Helper()
	measurements, ok := raw.([]any)
	if !ok {
		t.Fatalf("the report's measurements are %T, want a list", raw)
	}
	records := make([]measurementRecord, 0, len(measurements))
	for _, entry := range measurements {
		one, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("a measurement is %T, want an object", entry)
		}
		records = append(records, measurementRecord{
			sent: intOf(t, one, "sent"),
			loss: numberOf(t, one, "loss"),
		})
	}
	return records
}

func sliceOf(t *testing.T, object map[string]any, key string) []any {
	t.Helper()
	entries, ok := object[key].([]any)
	if !ok {
		t.Fatalf("the report field %q is %T, want a list", key, object[key])
	}
	return entries
}

func boolOf(t *testing.T, object map[string]any, key string) bool {
	t.Helper()
	value, ok := object[key].(bool)
	if !ok {
		t.Fatalf("the report field %q is %T, want a boolean", key, object[key])
	}
	return value
}

func stringOf(t *testing.T, object map[string]any, key string) string {
	t.Helper()
	value, ok := object[key].(string)
	if !ok {
		t.Fatalf("the report field %q is %T, want a string", key, object[key])
	}
	return value
}

func jsonNumber(t *testing.T, value any) float64 {
	t.Helper()
	switch number := value.(type) {
	case float64:
		return number
	case int:
		return float64(number)
	case int64:
		return float64(number)
	}
	t.Fatalf("a report value is %T (%v), want a number", value, value)
	return 0
}

func intOf(t *testing.T, object map[string]any, key string) int {
	t.Helper()
	return int(jsonNumber(t, object[key]))
}

func uint64Of(t *testing.T, object map[string]any, key string) uint64 {
	t.Helper()
	return uint64(jsonNumber(t, object[key]))
}

func numberOf(t *testing.T, object map[string]any, key string) float64 {
	t.Helper()
	return jsonNumber(t, object[key])
}

func hasField(object map[string]any, key string) bool {
	_, ok := object[key]
	return ok
}

// runDocument is the document a Phase 11 run lives in: it grants the laboratory
// the identifier above builds, and declares the packet loss the experiment
// observes and then removes.
func runDocument(t *testing.T, name string) string {
	t.Helper()
	return "name: " + name + "\n" +
		"scope:\n" +
		"  network: 10.42.0.0/24\n" +
		"  namespaces:\n" +
		"    - " + phase11Router + "\n" +
		"    - " + phase11Client + "\n" +
		"    - " + phase11Server + "\n" +
		"limits:\n" +
		"  max_namespaces: 8\n" +
		"  max_interfaces: 16\n" +
		"  max_duration: 5m\n" +
		"  max_bandwidth: 1Gbps\n" +
		"  max_packet_rate: 100000\n" +
		"  max_concurrent_probes: 64\n" +
		"faults:\n" +
		"  - target: " + phase11Server + "\n" +
		"    type: packet_loss\n" +
		// Nine tenths, so that the impaired phase of a run is impaired on
		// every run. The report has to carry a loss to report, and half loss
		// measured over a handful of probes leaves a one in sixteen chance of
		// measuring none at all.
		"    value: 90%\n"
}

func TestExperimentRunWritesTheReportItMeasured(t *testing.T) {
	document := writeFile(t, "phase11.yaml", runDocument(t, "phase11"))
	result := execNetforge(t, "",
		"experiment", "run",
		"--id", phase11LabID,
		"--namespace", phase11Client,
		"--target", phase11Address,
		"--protocol", "icmp",
		"--count", "12",
		"--size", "64",
		"--timeout", "300ms",
		"--deadline", "15s",
		"--after-fault", "600ms",
		"--settle", "500ms",
		"-recover", "remove_fault:"+phase11Server,
		"-authorize",
		"-why", "the operator authorized removing the fault this experiment declared",
		"--output", "json",
		document)
	if result.exitCode != 0 {
		t.Fatalf("experiment run: exit %d, stderr %q", result.exitCode, result.stderr)
	}
	if result.stdout == "" {
		t.Fatal("a successful run wrote no report")
	}

	report := parseReport(t, result.stdout)
	if !strings.HasPrefix(report.experimentID, "nf-phase11-") {
		t.Errorf("experiment_id = %q, want the document's deterministic identity", report.experimentID)
	}
	if !report.succeeded || !report.cleanedUp {
		t.Errorf("the report says succeeded=%t cleaned_up=%t, want the run to have finished and cleaned up",
			report.succeeded, report.cleanedUp)
	}

	// The lifecycle it walked, in the lifecycle's order, ending with the cleanup
	// that runs last whatever the outcome.
	if len(report.steps) == 0 || report.steps[0].state != "setup" {
		t.Fatalf("the lifecycle starts at %q, want setup", report.steps[0].state)
	}
	if report.steps[len(report.steps)-1].state != "cleanup" {
		t.Errorf("the lifecycle ends at %q, want cleanup to be the last thing a run does",
			report.steps[len(report.steps)-1].state)
	}

	// The ten metrics a run reports. Each one is derived from a real
	// measurement, so a reader can cross-check the summary against the
	// measurements in the same report.
	if report.metrics.packetsSent < 15 {
		t.Errorf("packets_sent = %d, want at least the probes of three phases",
			report.metrics.packetsSent)
	}
	if report.metrics.bytes <= 0 {
		t.Errorf("bytes = %d, want the payloads of the measurements", report.metrics.bytes)
	}
	if report.metrics.packetLoss <= 0 || report.metrics.packetLoss > 100 {
		t.Errorf("packet_loss = %f, want the observed loss to show", report.metrics.packetLoss)
	}
	if report.metrics.availability != 100-report.metrics.packetLoss {
		t.Errorf("availability = %f, want it to follow the packet loss", report.metrics.availability)
	}
	if report.metrics.retransmissions != 0 {
		t.Errorf("retransmissions = %d, want 0 for echo measurements", report.metrics.retransmissions)
	}
	if report.metrics.recoveryTime == "" || report.metrics.diagnosticDuration == "" {
		t.Errorf("recovery_time=%q diagnostic_duration=%q, want the two steps the operator is quoted for",
			report.metrics.recoveryTime, report.metrics.diagnosticDuration)
	}
	if len(report.metrics.phases) != 3 {
		t.Errorf("the metrics count %d phases, want baseline, observe and verify", len(report.metrics.phases))
	}

	// The measurements the run kept: a healthy baseline and an observation on
	// which the fault that was declared shows.
	if len(report.baseline) != 1 || len(report.observed) != 1 {
		t.Fatalf("the report holds %d baseline and %d observed measurements, want one of each",
			len(report.baseline), len(report.observed))
	}
	if report.baseline[0].loss >= report.observed[0].loss {
		t.Errorf("baseline lost %.1f%% and the observation lost %.1f%%, want the fault to show",
			report.baseline[0].loss, report.observed[0].loss)
	}

	// The fault that was injected is in the report, and the authorized removal
	// is recorded with the verdict of the measurement that followed it.
	if len(report.faults) != 1 {
		t.Fatalf("the report holds %d faults, want the one the document declared", len(report.faults))
	}
	fault := report.faults[0]
	if fault.kind != "packet_loss" || !strings.Contains(fault.target, phase11Server) {
		t.Errorf("the fault %+v is not the declared loss on the server", fault)
	}
	if len(report.recoveries) != 1 {
		t.Fatalf("the report holds %d recoveries, want the one that was authorized", len(report.recoveries))
	}
	recovery := report.recoveries[0]
	if recovery.action != "remove_fault" || recovery.target != phase11Server {
		t.Errorf("the recovery %+v is not the authorized removal", recovery)
	}
	if recovery.reason != "the operator authorized removing the fault this experiment declared" {
		t.Errorf("the recovery lost the operator's justification: %q", recovery.reason)
	}
	if !recovery.passed {
		t.Errorf("the recovery was judged failed: %q", recovery.result)
	}
	if recovery.result == "" {
		t.Error("the recovery carries no verdict, so nothing says its repair worked")
	}
}

func TestExperimentRunRefusesAnUnauthorizedRepair(t *testing.T) {
	document := writeFile(t, "phase11-unconsented.yaml", runDocument(t, "phase11-unconsented"))
	result := execNetforge(t, "",
		"experiment", "run",
		"--id", phase11LabID,
		"--namespace", phase11Client,
		"--target", phase11Address,
		"--protocol", "icmp",
		"-recover", "remove_fault:"+phase11Server,
		"--output", "json",
		document)
	if result.exitCode == 0 {
		t.Fatal("a repair nobody authorized was performed")
	}
	if !strings.Contains(result.stderr, "explicit authorization") {
		t.Errorf("the refusal %q does not say authorization is required", result.stderr)
	}
	if result.stdout != "" {
		t.Errorf("stdout = %q, want no report for a refused run", result.stdout)
	}
}

func TestAnExperimentRunNeverLogsASecret(t *testing.T) {
	// A secret that happens to be in the environment must never reach the log
	// or the report, whatever a run is told. The run below is refused before
	// the laboratory exists, and the refusal is the part that would be most
	// likely to echo a value back, so the assertion is that the secret stays
	// out of the run's entire output.
	const secret = "netforge-secret-9d2f1b"
	document := writeFile(t, "phase11-secret.yaml", "name: phase11-secret\n"+
		"scope:\n  network: 10.42.0.0/24\n  namespaces: []\n"+
		"limits:\n  max_namespaces: 8\n  max_interfaces: 16\n  max_duration: 5m\n")
	result := execNetforgeEnv(t, "", map[string]string{
		"NETFORGE_TEST_TOKEN": secret,
	}, "experiment", "run", "--id", phase11LabID,
		"--namespace", phase11Client,
		"--target", phase11Address,
		"--protocol", "icmp",
		"--output", "json",
		document)
	if result.exitCode == 0 {
		t.Fatalf("a run of a document that grants nothing was accepted:\n%s", result.stdout)
	}
	if strings.Contains(result.stdout, secret) || strings.Contains(result.stderr, secret) {
		t.Fatalf("the run's output leaked the secret %q\nstdout: %s\nstderr: %s",
			secret, result.stdout, result.stderr)
	}
}
