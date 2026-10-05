package metrics

import (
	"time"

	"github.com/netforge/netforge/internal/experiment"
	"github.com/netforge/netforge/internal/probe"
)

type Metrics struct {
	PacketsSent int64 `json:"packets_sent"`

	Bytes int64 `json:"bytes"`

	RTT RTT `json:"rtt"`

	PacketLoss float64 `json:"packet_loss"`

	Retransmissions int64 `json:"retransmissions"`

	Throughput uint64 `json:"throughput"`

	ConnectionFailures int64 `json:"connection_failures"`

	Availability float64 `json:"availability"`

	RecoveryTime string `json:"recovery_time"`

	DiagnosticDuration string `json:"diagnostic_duration"`

	Phases []Phase `json:"phases"`

	minRTT    int64
	maxRTT    int64
	avgSum    int64
	avgWeight int64
	haveRTT   bool
}

type RTT struct {
	Min string `json:"min"`

	Average string `json:"average"`

	Max string `json:"max"`
}

type Phase struct {
	Name string `json:"name"`

	Sent     int64 `json:"sent"`
	Received int64 `json:"received"`

	Loss float64 `json:"loss"`

	Bytes int64 `json:"bytes"`

	Throughput uint64 `json:"throughput"`

	AverageRTT string `json:"average_rtt"`

	Duration string `json:"duration"`
}

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

func lossOf(measurement probe.Measurement) float64 {
	return percentLoss(int64(measurement.Sent), int64(measurement.Received))
}

func percentLoss(sent, received int64) float64 {
	if sent <= 0 {
		return 0
	}
	return 100 * float64(sent-received) / float64(sent)
}
