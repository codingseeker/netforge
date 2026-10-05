package metrics

import (
	"testing"
	"time"

	"github.com/netforge/netforge/internal/experiment"
	"github.com/netforge/netforge/internal/probe"
)

func sampleMeasurement(sent, received int, rtt time.Duration, bytes int64, goodput uint64) probe.Measurement {
	return probe.Measurement{
		Target:     "10.42.0.6",
		Namespace:  "nf-client0-x",
		Sent:       sent,
		Received:   received,
		Loss:       100 * float64(sent-received) / float64(sent),
		MinRTT:     rtt,
		AverageRTT: rtt,
		MaxRTT:     rtt + 10*time.Millisecond,
		Bytes:      bytes,
		Duration:   time.Second,
		Goodput:    goodput,
	}
}

func sampleReport() experiment.Report {
	return experiment.Report{
		Name:         "sample",
		ExperimentID: "nf-sample-12345678",
		Baseline: []probe.Measurement{
			sampleMeasurement(5, 5, time.Millisecond, 360, 1000),
		},
		Observed: []probe.Measurement{
			sampleMeasurement(5, 0, 0, 0, 0),
		},
		Verified: []probe.Measurement{
			sampleMeasurement(5, 5, 2*time.Millisecond, 360, 900),
		},
		Steps: []experiment.StepReport{
			{State: experiment.StateBaseline, Duration: time.Second},
			{State: experiment.StateObserve, Duration: 2 * time.Second},
			{State: experiment.StateDiagnose, Duration: 20 * time.Millisecond},
			{State: experiment.StateRecover, Duration: 50 * time.Millisecond},
			{State: experiment.StateVerify, Duration: time.Second},
		},
	}
}

func TestFromReportDerivesTheMeasuredNumbers(t *testing.T) {
	m := FromReport(sampleReport())

	if m.PacketsSent != 15 {
		t.Errorf("PacketsSent = %d, want 15", m.PacketsSent)
	}
	if m.Bytes != 720 {
		t.Errorf("Bytes = %d, want 720", m.Bytes)
	}
	if m.PacketLoss != 100 {
		t.Errorf("PacketLoss = %f, want the broken observation's 100", m.PacketLoss)
	}
	if m.Availability != 0 {
		t.Errorf("Availability = %f, want 0 for a fully broken phase", m.Availability)
	}
	if m.ConnectionFailures != 1 {
		t.Errorf("ConnectionFailures = %d, want the one measurement that got no reply", m.ConnectionFailures)
	}
	if m.Retransmissions != 0 {
		t.Errorf("Retransmissions = %d, want 0 for echo measurements", m.Retransmissions)
	}

	if m.Throughput != 1000 {
		t.Errorf("Throughput = %d, want 1000", m.Throughput)
	}
	if m.RecoveryTime != "50ms" {
		t.Errorf("RecoveryTime = %q, want the recover step's 50ms", m.RecoveryTime)
	}
	if m.DiagnosticDuration != "20ms" {
		t.Errorf("DiagnosticDuration = %q, want the diagnose step's 20ms", m.DiagnosticDuration)
	}
}

func TestFromReportAggregatesRoundTripOverEveryReply(t *testing.T) {
	m := FromReport(sampleReport())
	if m.RTT.Min != "1ms" {
		t.Errorf("RTT.Min = %q, want the fastest reply at 1ms", m.RTT.Min)
	}
	if m.RTT.Average != "1.5ms" {
		t.Errorf("RTT.Average = %q, want the reply weighted mean of 1ms and 2ms", m.RTT.Average)
	}
	if m.RTT.Max != "12ms" {
		t.Errorf("RTT.Max = %q, want the slowest reply at 12ms", m.RTT.Max)
	}
}

func TestFromReportBreaksNumbersApartByPhase(t *testing.T) {
	m := FromReport(sampleReport())
	if len(m.Phases) != 3 {
		t.Fatalf("the report carries %d phases, want baseline, observe and verify", len(m.Phases))
	}
	byName := map[string]Phase{}
	for _, phase := range m.Phases {
		byName[phase.Name] = phase
	}
	if observed := byName["observe"]; observed.Received != 0 || observed.Loss != 100 {
		t.Errorf("the observed phase reports received=%d loss=%f, want a broken phase", observed.Received, observed.Loss)
	}
	if baseline := byName["baseline"]; baseline.AverageRTT != "1ms" {
		t.Errorf("the baseline phase reports average RTT %q, want 1ms", baseline.AverageRTT)
	}
}

func TestFromReportHandlesARunThatMeasuredNothing(t *testing.T) {
	m := FromReport(experiment.Report{Name: "empty", Steps: []experiment.StepReport{}})
	if m.PacketsSent != 0 || m.Bytes != 0 || m.PacketLoss != 0 || m.ConnectionFailures != 0 {
		t.Errorf("an empty run produced %+v, want all zero metrics", m)
	}
	if m.Availability != 100 {
		t.Errorf("Availability = %f, want 100 when nothing was measured", m.Availability)
	}
	if m.RTT.Min != "" || m.RTT.Average != "" || m.RTT.Max != "" {
		t.Errorf("an empty run produced RTT %+v, want no round trip to report", m.RTT)
	}
	if len(m.Phases) != 0 {
		t.Errorf("an empty run produced %d phases, want none", len(m.Phases))
	}
}

func TestFromReportDerivesLossFromCounts(t *testing.T) {

	var report experiment.Report
	report.Observed = []probe.Measurement{
		{Target: "10.42.0.6", Sent: 10, Received: 4, Loss: 0, Bytes: 100},
	}
	m := FromReport(report)
	if m.PacketLoss != 60 {
		t.Errorf("PacketLoss = %f, want 60%% derived from the counts", m.PacketLoss)
	}
	if m.Availability != 40 {
		t.Errorf("Availability = %f, want 40%%", m.Availability)
	}
}
