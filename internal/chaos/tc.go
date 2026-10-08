package chaos

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/codingseeker/netforge/internal/apperr"
	"github.com/codingseeker/netforge/internal/iface"
)

const (
	tcProgram = "tc"

	latencyUnit = time.Second

	burstFloor = 3000
)

var (
	replaceRoot = []string{"qdisc", "replace"}
	deleteRoot  = []string{"qdisc", "del"}
	showJSON    = []string{"-j", "qdisc", "show"}
)

type plan struct {
	clear []string

	root []string

	kind string
}

func buildPlan(device string, p Parameters) (plan, error) {
	const op = "chaos.buildPlan"

	if err := iface.ValidateName("chaos.buildPlan", "device", device); err != nil {
		return plan{}, apperr.Validation(op, "the device is not a valid interface name", err)
	}
	clear := append([]string{tcProgram}, deleteRoot...)
	clear = append(clear, "dev", device, "root")

	if p.Rate > 0 {

		root := append([]string{tcProgram}, replaceRoot...)
		root = append(root, "dev", device, "root")
		root = append(root, tokenBucketArgs(p.Rate)...)
		return plan{clear: clear, root: root, kind: "tbf"}, nil
	}

	args, err := netemArgs(p)
	if err != nil {
		return plan{}, err
	}
	root := append([]string{tcProgram}, replaceRoot...)
	root = append(root, "dev", device, "root")
	root = append(root, args...)
	return plan{clear: clear, root: root, kind: "netem"}, nil
}

func tokenBucketArgs(bitsPerSecond uint64) []string {
	bytesPerSecond := bitsPerSecond / 8
	burst := bytesPerSecond / 10
	if burst < burstFloor {
		burst = burstFloor
	}
	latency := uint64(latencyUnit / time.Millisecond)
	return []string{
		"tbf",
		"rate", Rate(bitsPerSecond).String(),
		"burst", strconv.FormatUint(burst, 10) + "b",
		"latency", strconv.FormatUint(latency, 10) + "ms",
	}
}

func netemArgs(p Parameters) ([]string, error) {
	const op = "chaos.netemArgs"
	if p.empty() {
		return nil, apperr.Validation(op, "a fault needs at least one parameter", nil)
	}
	args := []string{"netem"}
	if p.Queue > 0 {
		args = append(args, "limit", strconv.Itoa(p.Queue))
	}
	if p.Latency > 0 {
		args = append(args, "delay", p.Latency.String())
		if p.Jitter > 0 {
			args = append(args, p.Jitter.String())
			if p.Correlation > 0 {
				args = append(args, p.Correlation.String())
			}
		}
	}
	if p.Loss > 0 {
		args = append(args, "loss", p.Loss.String())
		if p.Correlation > 0 {
			args = append(args, p.Correlation.String())
		}
	}
	if p.Duplication > 0 {
		args = append(args, "duplicate", p.Duplication.String())
	}
	if p.Reordering > 0 {

		args = append(args, "reorder", p.Reordering.String(), p.Correlation.String(), "gap", strconv.Itoa(p.Gap))
	}
	return args, nil
}

func showArgs(device string) []string {
	args := []string{tcProgram}
	args = append(args, showJSON...)
	return append(args, "dev", device)
}

func delArgs(device string) []string {
	args := []string{tcProgram}
	args = append(args, deleteRoot...)
	return append(args, "dev", device, "root")
}

type qdiscState struct {
	Kind    string          `json:"kind"`
	Handle  string          `json:"handle"`
	Parent  string          `json:"parent"`
	Root    bool            `json:"root"`
	Options json.RawMessage `json:"options"`
}

type netemState struct {
	Limit int `json:"limit"`
	Delay *struct {
		Delay       float64 `json:"delay"`
		Jitter      float64 `json:"jitter"`
		Correlation float64 `json:"correlation"`
	} `json:"delay"`
	LossRandom *struct {
		Loss        float64 `json:"loss"`
		Correlation float64 `json:"correlation"`
	} `json:"loss-random"`
	Duplicate *struct {
		Duplicate float64 `json:"duplicate"`
	} `json:"duplicate"`
	Reorder *struct {
		Reorder     float64 `json:"reorder"`
		Correlation float64 `json:"correlation"`
	} `json:"reorder"`
	Gap int `json:"gap"`
}

type tbfState struct {
	Rate  uint64 `json:"rate"`
	Burst uint64 `json:"burst"`
	Lat   uint64 `json:"lat"`
}

func parseState(stdout string) ([]qdiscState, error) {
	const op = "chaos.parseState"
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		return nil, apperr.IO(op, "traffic control reported no state for the device", nil)
	}
	var states []qdiscState
	if err := json.Unmarshal([]byte(trimmed), &states); err != nil {
		return nil, apperr.IO(op, "the traffic control state is not valid JSON", err)
	}
	if len(states) == 0 {
		return nil, apperr.IO(op, "the traffic control state is empty", nil)
	}
	return states, nil
}

func installed(states []qdiscState) bool {
	for _, state := range states {
		if state.Kind == "netem" || state.Kind == "tbf" {
			return true
		}
	}
	return false
}

func rootOf(states []qdiscState) (qdiscState, bool) {
	for _, state := range states {
		if state.Root {
			return state, true
		}
	}
	return qdiscState{}, false
}

func observe(states []qdiscState, device string, p Parameters) (Observed, error) {
	const op = "chaos.observe"
	root, ok := rootOf(states)
	if !ok {
		return Observed{}, apperr.IO(op, fmt.Sprintf("the device %s reported no root discipline", device), nil)
	}
	seen := Observed{Kind: root.Kind, Handle: root.Handle}

	if p.Rate > 0 {
		if root.Kind != "tbf" {
			return Observed{}, apperr.Validation(op, fmt.Sprintf(
				"a bandwidth fault needs a token bucket on %s, the kernel reports %q", device, root.Kind), nil)
		}
		var bucket tbfState
		if err := json.Unmarshal(root.Options, &bucket); err != nil {
			return Observed{}, apperr.IO(op, "the token bucket state is not readable", err)
		}

		seen.Rate = bucket.Rate * 8
		seen.Burst = bucket.Burst
		if want := p.Rate / 8; bucket.Rate != want {
			return Observed{}, apperr.Validation(op, fmt.Sprintf(
				"the kernel limits %s to %s, the fault asked for %s",
				device, Rate(bucket.Rate*8), Rate(p.Rate)), nil)
		}
		return seen, nil
	}

	if root.Kind != "netem" {
		return Observed{}, apperr.Validation(op, fmt.Sprintf(
			"a %s fault needs a netem discipline on %s, the kernel reports %q", p.kindName(), device, root.Kind), nil)
	}
	netem, err := readNetem(root, device)
	if err != nil {
		return Observed{}, err
	}
	return mergeNetem(seen, netem), nil
}

func readNetem(state qdiscState, device string) (netemState, error) {
	const op = "chaos.readNetem"
	var netem netemState
	if err := json.Unmarshal(state.Options, &netem); err != nil {
		return netemState{}, apperr.IO(op, fmt.Sprintf("the netem state of %s is not readable", device), err)
	}
	return netem, nil
}

func mergeNetem(seen Observed, netem netemState) Observed {
	seen.Queue = netem.Limit
	if netem.Delay != nil {
		seen.Latency = Duration(seconds(netem.Delay.Delay))
		seen.Jitter = Duration(seconds(netem.Delay.Jitter))
		seen.Correlation = Percent(netem.Delay.Correlation * 100)
	}
	if netem.LossRandom != nil {
		seen.Loss = Percent(netem.LossRandom.Loss * 100)
		if netem.LossRandom.Correlation > 0 {
			seen.Correlation = Percent(netem.LossRandom.Correlation * 100)
		}
	}
	if netem.Duplicate != nil {
		seen.Duplication = Percent(netem.Duplicate.Duplicate * 100)
	}
	if netem.Reorder != nil {
		seen.Reordering = Percent(netem.Reorder.Reorder * 100)
		seen.Correlation = Percent(netem.Reorder.Correlation * 100)
		seen.Gap = netem.Gap
	}
	return seen
}

func verify(seen Observed, p Parameters) error {
	const op = "chaos.verify"

	if p.Rate > 0 && seen.Rate != p.Rate {
		return apperr.Validation(op, fmt.Sprintf(
			"the kernel reports a rate of %s, the fault asked for %s", Rate(seen.Rate), Rate(p.Rate)), nil)
	}
	if p.Latency > 0 && !equalDuration(seen.Latency, p.Latency) {
		return apperr.Validation(op, fmt.Sprintf(
			"the kernel reports a delay of %s, the fault asked for %s", seen.Latency, p.Latency), nil)
	}
	if p.Jitter > 0 && !equalDuration(seen.Jitter, p.Jitter) {
		return apperr.Validation(op, fmt.Sprintf(
			"the kernel reports a jitter of %s, the fault asked for %s", seen.Jitter, p.Jitter), nil)
	}
	if p.Loss > 0 && !equalPercent(seen.Loss, p.Loss) {
		return apperr.Validation(op, fmt.Sprintf(
			"the kernel reports a loss of %s, the fault asked for %s", seen.Loss, p.Loss), nil)
	}
	if p.Duplication > 0 && !equalPercent(seen.Duplication, p.Duplication) {
		return apperr.Validation(op, fmt.Sprintf(
			"the kernel reports a duplication of %s, the fault asked for %s", seen.Duplication, p.Duplication), nil)
	}
	if p.Reordering > 0 && !equalPercent(seen.Reordering, p.Reordering) {
		return apperr.Validation(op, fmt.Sprintf(
			"the kernel reports a reordering of %s, the fault asked for %s", seen.Reordering, p.Reordering), nil)
	}
	if p.Queue > 0 && seen.Queue != p.Queue {
		return apperr.Validation(op, fmt.Sprintf(
			"the kernel reports a queue of %d packets, the fault asked for %d", seen.Queue, p.Queue), nil)
	}
	return nil
}

func (p Parameters) kindName() string {
	switch {
	case p.Rate > 0:
		return string(KindBandwidth)
	case p.Loss > 0:
		return string(KindPacketLoss)
	case p.Duplication > 0:
		return string(KindDuplication)
	case p.Reordering > 0:
		return string(KindReordering)
	case p.Jitter > 0:
		return string(KindJitter)
	default:
		return string(KindLatency)
	}
}

func seconds(value float64) time.Duration {
	return time.Duration(value * float64(time.Second))
}

func equalDuration(got, want Duration) bool {
	const resolution = time.Microsecond
	difference := got.Std() - want.Std()
	if difference < 0 {
		difference = -difference
	}
	return difference <= resolution
}

func equalPercent(got, want Percent) bool {
	const resolution = 0.01
	difference := float64(got) - float64(want)
	if difference < 0 {
		difference = -difference
	}
	return difference <= resolution
}
