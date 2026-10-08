package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codingseeker/netforge/internal/reconcile"
)

const (
	phase18LabID  = "nfp18"
	phase18Client = "nf-client0-" + phase18LabID
	phase18Server = "nf-server0-" + phase18LabID
	phase18Router = "nf-router-" + phase18LabID
	phase18CIDR   = "10.48.0.0/24"
	phase18Target = "10.48.0.6"
	phase18Reason = "the trace names what the run did"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

type traceRecord map[string]any

func (r traceRecord) event() string {
	name, _ := r["event"].(string)
	return name
}

func (r traceRecord) text(key string) string {
	value, _ := r[key].(string)
	return value
}

var traceFields = []string{
	"state", "detail",
	"namespace", "role",
	"fault_id", "type", "direction", "status",
	"action", "target", "from", "reason",
	"classification", "layer", "rule",
	"measurements", "error",
}

func decodeRecords(t *testing.T, stderr string) []traceRecord {
	t.Helper()
	var records []traceRecord
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		if got := strings.Count(line, `"event":`); got != 1 {
			t.Fatalf("a log line carries %d event keys, want exactly one: %s", got, line)
		}
		decoder := json.NewDecoder(strings.NewReader(line))
		decoder.UseNumber()
		record := traceRecord{}
		if err := decoder.Decode(&record); err != nil {
			t.Fatalf("the log line %q is not a record: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func decodeTrace(t *testing.T, stderr string) []traceRecord {
	t.Helper()
	records := decodeRecords(t, stderr)
	if len(records) == 0 {
		t.Fatalf("the run wrote no log records at all:\n%s", stderr)
	}
	return records
}

func completeRecords(stderr string) []traceRecord {
	var records []traceRecord
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") || !strings.HasSuffix(line, "}") {
			continue
		}
		decoder := json.NewDecoder(strings.NewReader(line))
		decoder.UseNumber()
		record := traceRecord{}
		if err := decoder.Decode(&record); err != nil {
			continue
		}
		records = append(records, record)
	}
	return records
}

func awaitRecord(t *testing.T, out *syncBuffer, event string) traceRecord {
	t.Helper()
	deadline := time.Now().Add(awaitTimeout)
	for {
		for _, record := range completeRecords(out.String()) {
			if record.event() == event {
				return record
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run wrote no %q record within %s:\n%s", event, awaitTimeout, out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

const awaitTimeout = 30 * time.Second

func traceOf(records []traceRecord) string {
	lines := make([]string, 0, len(records))
	for _, record := range records {
		fields := []string{record.event()}
		for _, key := range traceFields {
			value, present := record[key]
			if !present {
				continue
			}
			if rendered := scalarOf(value); rendered != "" {
				fields = append(fields, key+"="+rendered)
			}
		}
		lines = append(lines, strings.Join(fields, " "))
	}
	return strings.Join(lines, "\n")
}

func scalarOf(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case json.Number:
		return typed.String()
	case bool:
		return strconv.FormatBool(typed)
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return ""
		}
		return string(encoded)
	}
}

func eventsNamed(records []traceRecord, event string) []traceRecord {
	var out []traceRecord
	for _, record := range records {
		if record.event() == event {
			out = append(out, record)
		}
	}
	return out
}

func phase18Document(t *testing.T) string {
	t.Helper()
	return writeFile(t, "phase18.yaml", "name: phase18-trace\n"+
		"seed: 2024\n"+
		"scope:\n"+
		"  network: "+phase18CIDR+"\n"+
		"  namespaces:\n"+
		"    - "+phase18Client+"\n"+
		"    - "+phase18Server+"\n"+
		"    - "+phase18Router+"\n"+
		"limits:\n"+
		"  max_namespaces: 8\n"+
		"  max_interfaces: 16\n"+
		"  max_duration: 5m\n"+
		"  max_bandwidth: 1Gbps\n"+
		"  max_packet_rate: 100000\n"+
		"  max_concurrent_probes: 64\n"+
		"faults:\n"+
		"  - target: "+phase18Server+"\n"+
		"    type: latency\n"+
		"    value: 500ms\n")
}

func phase18Trace(t *testing.T) (runResult, []traceRecord) {
	t.Helper()
	result := execNetforge(t, "",
		"--log-level=debug",
		"--log-format=json",
		"experiment", "run",
		"--id", phase18LabID,
		"--namespace", phase18Client,
		"--target", phase18Target,
		"--protocol", "icmp",
		"--count", "3",
		"--timeout", "200ms",
		"--deadline", "4s",
		"--after-fault", "300ms",
		"-recover", "remove_fault:"+phase18Server,
		"-authorize",
		"-why", phase18Reason,
		"--output", "json",
		phase18Document(t))
	if result.exitCode != 0 {
		t.Fatalf("the traced run failed: exit %d\n%s\n%s", result.exitCode, result.stdout, result.stderr)
	}
	return result, decodeTrace(t, result.stderr)
}

func TestEveryStageOfARunEmitsOneRecordWithItsOwnEventName(t *testing.T) {
	_, records := phase18Trace(t)

	var experimentID string
	for _, record := range records {
		if got := record.text("experiment_id"); got != "" {
			experimentID = got
			break
		}
	}
	if experimentID == "" {
		t.Fatalf("no record names the experiment:\n%s", traceOf(records))
	}

	for _, record := range records {
		if record.event() == "dispatch" {
			continue
		}
		if got := record.text("experiment_id"); got != experimentID {
			t.Errorf("the record %q carries the experiment %q, want %q", record.event(), got, experimentID)
		}
	}

	states := []string{"setup", "baseline", "fault", "observe", "diagnose",
		"recover", "verify", "report", "cleanup"}
	for _, state := range states {
		stage := "stage_" + state
		emitted := eventsNamed(records, stage)
		if len(emitted) != 1 {
			t.Errorf("the event %q was emitted %d times, want exactly one:\n%s", stage, len(emitted), traceOf(records))
			continue
		}
		if got := emitted[0].text("state"); got != state {
			t.Errorf("the event %q named the state %q, want %q", stage, got, state)
		}
		if emitted[0].text("detail") == "" {
			t.Errorf("the event %q carries no detail, so a reader cannot tell what the step did", stage)
		}
	}
}

func TestOneRunsRecordsReconstructTheRun(t *testing.T) {
	_, records := phase18Trace(t)

	golden := strings.Join([]string{
		"dispatch",
		"namespace_created namespace=" + phase18Client + " role=client",
		"namespace_created namespace=" + phase18Server + " role=server",
		"namespace_created namespace=" + phase18Router + " role=router",
		"stage_setup state=setup detail=create the laboratory from the document",
		"stage_baseline state=baseline detail=measure the path before anything is impaired",
		"fault_applied fault_id=latency@" + phase18Server + "/nfs0:out" +
			" type=latency direction=out status=active target=" + phase18Server + "/nfs0",
		"stage_fault state=fault detail=inject the declared faults",
		"stage_observe state=observe detail=measure the path while the faults are in place",
		"diagnosis_made classification=PROBABLE layer=network rule=degraded-path",
		"stage_diagnose state=diagnose detail=explain the measurement with evidence",
		"action_authorized action=remove_fault target=" + phase18Server +
			" from=" + phase18Client + " reason=" + phase18Reason,
		"fault_removed action=remove_fault target=" + phase18Server,
		"stage_recover state=recover detail=perform the authorized recovery actions",
		"verification_passed measurements=1",
		"stage_verify state=verify detail=measure the path after recovery",
		"stage_report state=report detail=write down what happened",
		"stage_cleanup state=cleanup detail=take the laboratory apart",
		"experiment run succeeded",
	}, "\n")

	if got := traceOf(records); got != golden {
		t.Errorf("the trace of the run does not match the contract:\n--- got ---\n%s\n--- want ---\n%s", got, golden)
	}
}

func TestTheTraceNamesTheReportBesideIt(t *testing.T) {
	result, records := phase18Trace(t)

	report := decodeRun(t, result.stdout)
	names := map[string]bool{}
	for _, record := range eventsNamed(records, "namespace_created") {
		names[record.text("namespace")] = true
	}
	for _, node := range report.Laboratory.Nodes {
		if !names[node.Namespace] {
			t.Errorf("the report built %s and the trace does not name it", node.Namespace)
		}
		delete(names, node.Namespace)
	}
	if report.Laboratory.RouterNamespace != "" && !names[report.Laboratory.RouterNamespace] {
		t.Errorf("the report forwarded in %s and the trace does not name it", report.Laboratory.RouterNamespace)
	}

	faults := eventsNamed(records, "fault_applied")
	if len(faults) != len(report.Faults) {
		t.Fatalf("the report carries %d faults and the trace %d", len(report.Faults), len(faults))
	}
	for i, fault := range report.Faults {
		if got := faults[i].text("fault_id"); got != fault.ID {
			t.Errorf("the trace names the fault %q where the report names %q", got, fault.ID)
		}
	}

	actions := eventsNamed(records, "action_authorized")
	if len(actions) != len(report.Recoveries) {
		t.Fatalf("the report carries %d recoveries and the trace %d", len(report.Recoveries), len(actions))
	}
	for i, outcome := range report.Recoveries {
		if got := actions[i].text("action"); got != outcome.Action {
			t.Errorf("the trace names the action %q where the report names %q", got, outcome.Action)
		}
	}
}

func TestTheRecommendationARunPrintsIsAFlagThatWorks(t *testing.T) {
	result := execNetforge(t, "",
		"--log-format=json",
		"experiment", "run",
		"--id", phase18LabID,
		"--namespace", phase18Client,
		"--target", phase18Target,
		"--protocol", "icmp",
		"--count", "3",
		"--timeout", "200ms",
		"--deadline", "4s",
		"--after-fault", "300ms",
		"--output", "json",
		phase18Document(t))
	if result.exitCode == 0 {
		t.Fatalf("a run nobody authorized to repair reported success:\n%s", result.stdout)
	}
	if !strings.Contains(result.stderr, "explicit authorization") &&
		!strings.Contains(result.stderr, "path did not recover") {
		t.Logf("the run failed as it should; stderr: %s", result.stderr)
	}

	report := readRunReport(t, result.stdout)
	if !report.Recommendation.Diagnosed {
		t.Fatalf("a run that measured loss did not diagnose it:\n%s", result.stdout)
	}
	flag := report.Recommendation.Flag
	if flag == "" {
		t.Fatalf("a diagnosed run recommends no flag, so nothing tells the operator what to type:\n%s", result.stdout)
	}
	value, found := strings.CutPrefix(flag, "-recover ")
	if !found {
		t.Fatalf("the recommendation %q is not a -recover flag", flag)
	}

	planned := execNetforge(t, "",
		"experiment", "plan",
		"--namespace", phase18Client,
		"--target", phase18Target,
		"--protocol", "icmp",
		"-recover", value,
		"-authorize",
		"-why", phase18Reason,
		phase18Document(t))
	if planned.exitCode != 0 {
		t.Fatalf("the recommendation %q does not compile:\n%s", flag, planned.stderr)
	}
	if !strings.Contains(planned.stdout, "recoveries: 1") {
		t.Errorf("the plan does not count the recommendation as an authorized recovery:\n%s", planned.stdout)
	}
}

func TestARunThatDiagnosedNothingSaysSoInBothFormats(t *testing.T) {
	clean := writeFile(t, "phase18-clean.yaml", "name: phase18-clean\n"+
		"seed: 2024\n"+
		"scope:\n"+
		"  network: "+phase18CIDR+"\n"+
		"  namespaces:\n"+
		"    - "+phase18Client+"\n"+
		"    - "+phase18Server+"\n"+
		"    - "+phase18Router+"\n"+
		"limits:\n"+
		"  max_namespaces: 8\n"+
		"  max_interfaces: 16\n"+
		"  max_duration: 5m\n"+
		"  max_bandwidth: 1Gbps\n"+
		"  max_packet_rate: 100000\n"+
		"  max_concurrent_probes: 64\n")
	args := []string{
		"experiment", "run",
		"--id", phase18LabID,
		"--namespace", phase18Client,
		"--target", phase18Target,
		"--protocol", "icmp",
		"--count", "2",
		"--timeout", "300ms",
		"--deadline", "3s",
	}

	asJSON := execNetforge(t, "", append(append([]string{}, args...), "--output", "json", clean)...)
	if asJSON.exitCode == 0 {
		t.Fatalf("a run with no failure was reported as a success:\n%s", asJSON.stdout)
	}
	if !strings.Contains(asJSON.stderr, "no failure to diagnose") {
		t.Errorf("the refusal does not say there was nothing to diagnose:\n%s", asJSON.stderr)
	}
	asText := execNetforge(t, "", append(append([]string{}, args...), clean)...)
	if asText.exitCode == 0 {
		t.Fatalf("the same run as text was reported as a success:\n%s", asText.stdout)
	}

	var raw map[string]any
	if err := json.Unmarshal([]byte(asJSON.stdout), &raw); err != nil {
		t.Fatalf("the failed run wrote no report: %v\n%s", err, asJSON.stdout)
	}
	recommendation, present := raw["recommendation"]
	if !present {
		t.Fatalf("a run that diagnosed nothing omitted the recommendation key instead of saying so:\n%s", asJSON.stdout)
	}
	fields, ok := recommendation.(map[string]any)
	if !ok {
		t.Fatalf("the recommendation is %T, want an object", recommendation)
	}
	if fields["diagnosed"] != false {
		t.Errorf("a run that diagnosed nothing is marked diagnosed: %+v", fields)
	}
	if statement, _ := fields["statement"].(string); !strings.Contains(statement, "nothing was diagnosed") {
		t.Errorf("the JSON report's recommendation says %q, want it to say nothing was diagnosed", statement)
	}
	if flag, offered := fields["flag"]; offered {
		t.Errorf("a run with nothing to recommend still offers the flag %v", flag)
	}
	for _, want := range []string{"recommendation:", "diagnosed: false", "nothing was diagnosed"} {
		if !strings.Contains(asText.stdout, want) {
			t.Errorf("the text report does not carry %q:\n%s", want, asText.stdout)
		}
	}
}

func TestAStateRootLargerThanTheListingBoundIsRefused(t *testing.T) {
	limit := reconcile.DefaultLimits().MaxEntries

	atBound := t.TempDir()
	phase18Fill(t, atBound, limit)
	listed := execNetforgeEnv(t, "", map[string]string{"NETFORGE_STATE_ROOT": atBound}, "reconcile", "list")
	if listed.exitCode != 0 {
		t.Fatalf("a state root of exactly %d entries was refused: %s", limit, listed.stderr)
	}
	if !strings.Contains(listed.stdout, "not identified and left alone") {
		t.Errorf("the listing did not account for %d entries:\n%s", limit, listed.stdout)
	}

	pastBound := t.TempDir()
	phase18Fill(t, pastBound, limit+1)
	refused := execNetforgeEnv(t, "", map[string]string{"NETFORGE_STATE_ROOT": pastBound},
		"reconcile", "list")
	if refused.exitCode == 0 {
		t.Fatalf("a state root of %d entries was listed:\n%s", limit+1, refused.stdout)
	}
	if !strings.Contains(refused.stderr, strconv.Itoa(limit)) {
		t.Errorf("the refusal does not name the bound %d:\n%s", limit, refused.stderr)
	}
	if !strings.Contains(refused.stderr, "at most") {
		t.Errorf("the refusal does not say what one listing reads:\n%s", refused.stderr)
	}
}

func phase18Fill(t *testing.T, root string, count int) {
	t.Helper()
	for i := range count {
		name := filepath.Join(root, "entry-"+strconv.Itoa(i))
		if err := os.WriteFile(name, nil, 0o600); err != nil {
			t.Fatalf("WriteFile(%s) = %v, want nil", name, err)
		}
	}
}
