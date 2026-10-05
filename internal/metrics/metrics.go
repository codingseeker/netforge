// Package metrics turns an experiment run into the numbers a report is kept
// for.
//
// Every metric is derived from what the run actually measured. Nothing in this
// package is a configured value or an assumption, so a number in a report can
// be traced back to a measurement in the same report.
package metrics

import (
	"time"

	"github.com/netforge/netforge/internal/experiment"
	"github.com/netforge/netforge/internal/probe"
)

// Metrics is the set of numbers one run reports.
type Metrics struct {
	// PacketsSent is the number of echo requests the run sent across all of its
	// measurements.
	PacketsSent int64 `json:"packets_sent"`
	// Bytes is the payload volume the run measured, in bytes.
	Bytes int64 `json:"bytes"`
	// RTT is the round trip the run measured across every reply that came back.
	RTT RTT `json:"rtt"`
	// PacketLoss is the measured loss of the impaired phase, as a percentage.
	PacketLoss float64 `json:"packet_loss"`
	// Retransmissions is how many retransmissions the measurements recorded. The
	// experiment's echo measurements have no transport to retransmit, so this is
	// the count a measurement carries when its protocol counts one.
	Retransmissions int64 `json:"retransmissions"`
	// Throughput is the highest payload rate the run measured, in bits per
	// second.
	Throughput uint64 `json:"throughput"`
	// ConnectionFailures is how many measurements received no reply to any of
	// their requests.
	ConnectionFailures int64 `json:"connection_failures"`
	// Availability is the share of the impaired phase's requests that were
	// answered, as a percentage.
	Availability float64 `json:"availability"`
	// RecoveryTime is how long the recover step took. It is empty when the step
	// never ran.
	RecoveryTime string `json:"recovery_time"`
	// DiagnosticDuration is how long the diagnose step took. It is empty when
	// the step never ran.
	DiagnosticDuration string `json:"diagnostic_duration"`
	// Phases breaks the measured numbers apart by lifecycle phase, so a reader
	// can see which phase a number came from.
	Phases []Phase `json:"phases"`

	// The round trip is accumulated over every reply and rendered at the end.
	// The fields stay unexported so they are never mistaken for report data.
	minRTT    int64
	maxRTT    int64
	avgSum    int64
	avgWeight int64
	haveRTT   bool
}

// RTT is the round trip a run measured.
type RTT struct {
	// Min is the fastest reply the run saw.
	Min string `json:"min"`
	// Average is the reply-count weighted mean of the replies the run saw.
	Average string `json:"average"`
	// Max is the slowest reply the run saw.
	Max string `json:"max"`
}

// Phase is one measurement phase of the lifecycle, with the numbers that phase
// measured.
type Phase struct {
	// Name is the lifecycle phase: baseline, observe or verify.
	Name string `json:"name"`
	// Sent and Received are the packet counts the kernel reported.
	Sent     int64 `json:"sent"`
	Received int64 `json:"received"`
	// Loss is the measured loss of the phase, as a percentage.
	Loss float64 `json:"loss"`
	// Bytes is the payload volume the phase measured, in bytes.
	Bytes int64 `json:"bytes"`
	// Throughput is the payload rate the phase measured, in bits per second.
	Throughput uint64 `json:"throughput"`
	// AverageRTT is the mean round trip of the phase's replies.
	AverageRTT string `json:"average_rtt"`
	// Duration is the wall clock time the phase's measurement took.
	Duration string `json:"duration"`
}

// FromReport derives the metrics of a run from its report. Every number is
// computed from the measurements the run took, so a metric cannot contradict
// the measurement it was computed from.
func FromReport(run experiment.Report) Metrics {
	m := Metrics{}
	var sent, received int64

	addPhases := func(name string, measurements []probe.Measurement) {
		for _, measurement := range measurements {
			m.PacketsSent += int64(measurement.Sent)
			m.Bytes += measurement.Bytes
			sent += int64(measurement.Sent)
			received += int64(measurement.Received)
			if measurement.Goodput > m.Throughput {
				m.Throughput = measurement.Goodput
			}
			if measurement.Received == 0 && measurement.Sent > 0 {
				m.ConnectionFailures++
			}
			if measurement.Replied() {
				m.accumulateRTT(measurement)
			}
			m.Phases = append(m.Phases, phaseOf(name, measurement))
		}
	}

	addPhases("baseline", run.Baseline)
	addPhases("observe", run.Observed)
	addPhases("verify", run.Verified)

	m.renderRTT()

	// The loss is derived from the counts rather than read from a field, so the
	// metric agrees with the packet counts in the same report. The impaired
	// phase is the one that matters; without it, the whole run is the answer.
	switch {
	case len(run.Observed) > 0:
		m.PacketLoss = 0
		for _, measurement := range run.Observed {
			m.PacketLoss += lossOf(measurement)
		}
		m.PacketLoss /= float64(len(run.Observed))
	default:
		m.PacketLoss = percentLoss(sent, received)
	}
	m.Availability = 100 - m.PacketLoss
	if m.Availability < 0 {
		m.Availability = 0
	}

	for _, step := range run.Steps {
		switch step.State {
		case experiment.StateDiagnose:
			m.DiagnosticDuration = step.Duration.String()
		case experiment.StateRecover:
			m.RecoveryTime = step.Duration.String()
		}
	}
	return m
}

// accumulateRTT folds one measurement's round trip into the run's summary. The
// replies are the weights, so a measurement that answered more moves the mean
// more than one that answered a single reply.
func (m *Metrics) accumulateRTT(measurement probe.Measurement) {
	replies := int64(measurement.Received)
	if !m.haveRTT {
		m.minRTT = int64(measurement.MinRTT)
		m.maxRTT = int64(measurement.MaxRTT)
		m.avgSum = int64(measurement.AverageRTT) * replies
		m.avgWeight = replies
		m.haveRTT = true
		return
	}
	if measurement.MinRTT < time.Duration(m.minRTT) {
		m.minRTT = int64(measurement.MinRTT)
	}
	if measurement.MaxRTT > time.Duration(m.maxRTT) {
		m.maxRTT = int64(measurement.MaxRTT)
	}
	m.avgSum += int64(measurement.AverageRTT) * replies
	m.avgWeight += replies
}

// renderRTT writes the accumulated round trip into the report fields.
func (m *Metrics) renderRTT() {
	if !m.haveRTT {
		m.RTT = RTT{}
		return
	}
	m.RTT = RTT{
		Min:     time.Duration(m.minRTT).String(),
		Max:     time.Duration(m.maxRTT).String(),
		Average: time.Duration(m.avgSum / m.avgWeight).String(),
	}
}

// phaseOf renders one measurement as a report phase.
func phaseOf(name string, measurement probe.Measurement) Phase {
	phase := Phase{
		Name:       name,
		Sent:       int64(measurement.Sent),
		Received:   int64(measurement.Received),
		Loss:       lossOf(measurement),
		Bytes:      measurement.Bytes,
		Throughput: measurement.Goodput,
		Duration:   measurement.Duration.String(),
	}
	if measurement.Replied() {
		phase.AverageRTT = measurement.AverageRTT.String()
	}
	return phase
}

// lossOf derives the loss of one measurement from its counts.
func lossOf(measurement probe.Measurement) float64 {
	return percentLoss(int64(measurement.Sent), int64(measurement.Received))
}

func percentLoss(sent, received int64) float64 {
	if sent <= 0 {
		return 0
	}
	return 100 * float64(sent-received) / float64(sent)
}
