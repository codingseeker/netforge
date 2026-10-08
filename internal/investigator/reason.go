package investigator

import (
	"fmt"
	"strings"

	"github.com/codingseeker/netforge/internal/probe"
)

const (
	layerNetwork   = "network"
	layerRouting   = "routing"
	layerTransport = "transport"
	layerDNS       = "dns"
	layerTLS       = "tls"
	layerHTTP      = "http"
)

const (
	ruleNoRoute         = "no-route"
	ruleRefused         = "connection-refused"
	ruleSpecific        = "specific-failure"
	ruleTimeout         = "timeout-at-layer"
	ruleDegraded        = "degraded-path"
	ruleUnreachable     = "unreachable"
	ruleInconclusive    = "inconclusive"
	ruleDidNotReproduce = "did-not-reproduce"
	ruleLabProblem      = "measurement-failed"
)

func (w *work) explain(report *Report, chain []probe.Protocol, route Route, routeErr error, repeated repeat) {
	limits := w.investigator.limits
	failing := w.observation.Failed

	body := ""
	w.reproduced = repeated.CameBack
	defer func() {
		clamp(report)
		report.Summary = summarise(report, body)
	}()

	if routeErr == nil && !route.Found {
		index := indexOfLayer(report.Evidence, layerRouting)
		report.Layer = layerRouting
		report.Rule = ruleNoRoute
		report.Diagnosis = Confirmed
		report.Hypotheses = w.hypothesesFor(report, chain, nil)
		report.Hypotheses = append(report.Hypotheses, Hypothesis{
			Layer:      layerRouting,
			Statement:  "the kernel has no route to the target, so nothing above it can be reached",
			Evidence:   []int{index},
			Confidence: Confirmed,
			Concludes:  true,
		})
		body = fmt.Sprintf(
			"the failure is in the %s layer. The kernel has no route to %s (%s), which explains "+
				"every failure above it.",
			layerRouting, w.observation.Target, report.Cite([]int{index}))
		return
	}

	if broken, ok := brokenEvidence(report.Evidence); ok {
		note := report.Evidence[broken].Note
		report.Diagnosis = Unknown
		report.Layer = ""
		report.Rule = ruleLabProblem
		report.Hypotheses = append(report.Hypotheses, Hypothesis{
			Statement:  "the failure could not be investigated because a measurement could not be taken",
			Evidence:   []int{broken},
			Refutation: "a measurement that was not taken says nothing about the network",
			Confidence: Unknown,
			Concludes:  true,
		})
		body = fmt.Sprintf(
			"nothing about the network can be concluded from a measurement that was not "+
				"taken (%s: %s).", report.Cite([]int{broken}), note)
		return
	}

	if !anyWorking(report.Evidence) {
		indexes := refutingIndexes(report.Evidence)
		report.Layer = layerNetwork
		report.Rule = ruleUnreachable
		report.Diagnosis = Possible
		report.Hypotheses = w.hypothesesFor(report, chain, nil)
		report.Hypotheses = append(report.Hypotheses,
			Hypothesis{
				Layer: layerNetwork,
				Statement: "the target did not answer at the IP layer, so the failure is in the " +
					"network or below it; whether the host is down or the path drops cannot be " +
					"separated by this evidence",
				Evidence:   indexes,
				Confidence: Possible,
				Concludes:  true,
			},
			Hypothesis{
				Layer:      layerRouting,
				Statement:  "a route exists but does not carry traffic to the target",
				RefutedBy:  []int{indexOfLayer(report.Evidence, layerRouting)},
				Refutation: "the kernel reports a route, so the routing table is not the whole story",
				Confidence: Possible,
			})
		body = fmt.Sprintf(
			"nothing answered on the path to the target. Every layer from the network up either "+
				"timed out or was refused, and a host that is down and a path that drops look "+
				"identical from here, so whether the host is down or the path drops cannot be "+
				"separated by this evidence (%s).", report.Cite(indexes))
		return
	}

	if impaired := impairment(report.Evidence, limits); impaired != nil {
		index := indexOf(report.Evidence, *impaired)
		report.Layer = impaired.Layer
		report.Rule = ruleDegraded
		report.Diagnosis = Probable
		report.Hypotheses = w.hypothesesFor(report, chain, impaired)
		report.Hypotheses = append(report.Hypotheses, Hypothesis{
			Layer:      impaired.Layer,
			Statement:  impairmentStatement(*impaired),
			Evidence:   []int{index},
			Confidence: Probable,
			Concludes:  true,
		})
		body = fmt.Sprintf(
			"the path to the target is impaired rather than broken. %s (%s).",
			impairmentStatement(*impaired), report.Cite([]int{index}))
		return
	}

	blame, below, named := blameLayer(report, chain)
	if !named {

		report.Layer = failing.Layer()
		report.Rule = ruleDidNotReproduce
		report.Diagnosis = Possible
		report.Hypotheses = w.hypothesesFor(report, chain, nil)
		report.Hypotheses = append(report.Hypotheses, Hypothesis{
			Layer: layerNetwork,
			Statement: "the failure was intermittent: the layers that were measured came back " +
				"working, so no layer can be named as broken",
			Confidence: Possible,
			Concludes:  true,
		})
		body = fmt.Sprintf(
			"the failure did not reproduce. The layers that were measured came back working, "+
				"so this is an intermittent fault and no layer can be named (%s).",
			report.Cite(workingIndexes(report.Evidence)))
		return
	}

	confidence := Probable
	rule := ruleSpecific
	switch {
	case blame.Result.Failure == probe.FailureTimeout:

		confidence = Possible
		rule = ruleTimeout
	case repeated.CameBack:

		confidence = Confirmed
		rule = ruleSpecific
		if blame.Result.Failure == probe.FailureRefused {
			rule = ruleRefused
		}
	}
	report.Layer = blame.Layer
	report.Rule = rule
	report.Diagnosis = confidence
	report.Hypotheses = w.hypothesesFor(report, chain, &blame)
	statement := w.statementFor(blame)
	if blame.Layer != failing.Layer() {

		statement += fmt.Sprintf(
			"; it was observed at the %s layer, but the %s layer below it is the one that failed, "+
				"so the %s layer was never really reached",
			failing.Layer(), blame.Layer, failing.Layer())
	}
	report.Hypotheses = append(report.Hypotheses, Hypothesis{
		Layer:      blame.Layer,
		Statement:  statement,
		Evidence:   append([]int{indexOf(report.Evidence, blame)}, below...),
		Confidence: confidence,
		Concludes:  true,
	})
	body = fmt.Sprintf(
		"the failure is in the %s layer. %s (%s%s).",
		blame.Layer, statement,
		report.Cite([]int{indexOf(report.Evidence, blame)}), citing(below))
}

func blameLayer(report *Report, chain []probe.Protocol) (Evidence, []int, bool) {
	highestWorking := -1
	var failedAt []int
	for position, protocol := range chain {
		index := lastIndexOfLayer(report.Evidence, protocol.Layer())
		if index < 0 {
			continue
		}
		switch {
		case report.Evidence[index].Supports():
			highestWorking = position
		case report.Evidence[index].Refutes():
			failedAt = append(failedAt, position)
		}
	}
	var blame int
	found := false
	for _, position := range failedAt {
		if position > highestWorking {
			blame, found = position, true
			break
		}
	}
	if !found {
		return Evidence{}, nil, false
	}

	for position := highestWorking + 1; position < blame; position++ {
		if lastIndexOfLayer(report.Evidence, chain[position].Layer()) < 0 {
			return Evidence{}, nil, false
		}
	}
	index := lastIndexOfLayer(report.Evidence, chain[blame].Layer())
	return report.Evidence[index], workingIndexes(report.Evidence), true
}

func workingIndexes(evidence []Evidence) []int {
	var out []int
	for index, item := range evidence {
		if item.Supports() {
			out = append(out, index)
		}
	}
	return out
}

func (w *work) hypothesesFor(report *Report, chain []probe.Protocol, blame *Evidence) []Hypothesis {
	var out []Hypothesis
	for _, protocol := range chain {
		layer := protocol.Layer()
		index := lastIndexOfLayer(report.Evidence, layer)
		hypothesis := Hypothesis{Layer: layer, Statement: statementForLayer(protocol)}
		if index < 0 {
			hypothesis.Confidence = Unknown
			out = append(out, hypothesis)
			continue
		}
		evidence := report.Evidence[index]
		switch {
		case blame != nil && evidence.Layer == blame.Layer && evidence.Refutes():
			hypothesis.Evidence = []int{index}
			hypothesis.Confidence = Possible
		case evidence.Supports():
			hypothesis.RefutedBy = []int{index}
			hypothesis.Refutation = "this layer was measured working"
			hypothesis.Confidence = Unknown
		case evidence.Probed:
			hypothesis.Evidence = []int{index}
			hypothesis.Confidence = Possible
		case evidence.Attempted:
			hypothesis.RefutedBy = []int{index}
			hypothesis.Refutation = "this layer was not measured, so nothing is known about it"
			hypothesis.Confidence = Unknown
		default:
			hypothesis.Confidence = Unknown
		}
		out = append(out, hypothesis)
	}
	return out
}

func (w *work) statementFor(evidence Evidence) string {
	if evidence.Result.Failure == probe.FailureRefused {

		statement := fmt.Sprintf("the host answered and refused the connection, so the port %d "+
			"is not accepting traffic", w.observation.Port)
		if !w.reproduced {
			statement += "; this was measured once, so a momentary refusal has not been excluded"
		}
		return statement
	}
	if evidence.Result.Failure == probe.FailureTimeout {
		return fmt.Sprintf("the %s layer did not answer in time", evidence.Layer)
	}
	return fmt.Sprintf("the %s layer answered with a failure: %s", evidence.Layer, evidence.Note)
}

func statementForLayer(protocol probe.Protocol) string {
	switch protocol {
	case probe.ProtocolICMP:
		return "the target is not answering at the IP layer"
	case probe.ProtocolTCP:
		return "the port is not accepting connections"
	case probe.ProtocolUDP:
		return "the port is not answering datagrams"
	case probe.ProtocolDNS:
		return "the resolver is not answering correctly"
	case probe.ProtocolTLS:
		return "the TLS handshake is failing"
	case probe.ProtocolHTTP:
		return "the server is not answering the request correctly"
	default:
		return "the " + string(protocol) + " layer is failing"
	}
}

func brokenEvidence(evidence []Evidence) (int, bool) {
	for index, item := range evidence {
		if item.Attempted && !item.Probed {
			return index, true
		}
	}
	return -1, false
}

func summarise(report *Report, body string) string {
	if body == "" {
		return fmt.Sprintf("%s: the evidence does not support a conclusion.", report.Diagnosis)
	}
	return report.Diagnosis.String() + ": " + body
}

func impairment(evidence []Evidence, limits Limits) *Evidence {
	for index := range evidence {
		item := evidence[index]
		if item.Layer != layerNetwork || !item.Probed {
			continue
		}

		if item.Result.Received == 0 {
			continue
		}
		if item.Result.Loss >= limits.MaxLoss {
			return &evidence[index]
		}
		if item.Result.Sent > 0 && item.Result.RTT >= limits.HighRTT {
			return &evidence[index]
		}
	}
	return nil
}

func impairmentStatement(evidence Evidence) string {
	switch {
	case evidence.Result.Loss >= 1:
		return fmt.Sprintf("measured loss of %.1f%% on the path to the target",
			evidence.Result.Loss)
	case evidence.Result.RTT > 0:
		return fmt.Sprintf("the path to the target answers in %s, which is far above a healthy round trip",
			evidence.Result.RTT.Round(1000000))
	default:
		return "the path to the target is degraded"
	}
}

func anyWorking(evidence []Evidence) bool {
	for _, item := range evidence {
		if item.Supports() {
			return true
		}
	}
	return false
}

func refutingIndexes(evidence []Evidence) []int {
	var out []int
	for index, item := range evidence {
		if item.Refutes() {
			out = append(out, index)
		}
	}
	return out
}

func indexOf(evidence []Evidence, want Evidence) int {
	for index, item := range evidence {
		if item == want {
			return index
		}
	}
	return -1
}

func indexOfLayer(evidence []Evidence, layer string) int {
	return lastIndexOfLayer(evidence, layer)
}

func lastIndexOfLayer(evidence []Evidence, layer string) int {
	for index := len(evidence) - 1; index >= 0; index-- {
		if evidence[index].Layer == layer {
			return index
		}
	}
	return -1
}

func citing(indexes []int) string {
	if len(indexes) == 0 {
		return ""
	}
	parts := make([]string, 0, len(indexes))
	for _, index := range indexes {
		parts = append(parts, fmt.Sprintf("%d", index))
	}
	return ", supported by evidence " + strings.Join(parts, ", ")
}
