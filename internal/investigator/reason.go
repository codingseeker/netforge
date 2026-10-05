package investigator

import (
	"fmt"
	"strings"

	"github.com/netforge/netforge/internal/probe"
)

const (
	layerNetwork   = "network"
	layerRouting   = "routing"
	layerTransport = "transport"
	layerDNS       = "dns"
	layerTLS       = "tls"
	layerHTTP      = "http"
)

// The names of the rules, so a report says which reasoning produced its
// classification rather than only what the classification was.
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

// explain generates the candidate explanations from the evidence, decides which
// one the evidence supports, and classifies how far it goes.
func (w *work) explain(report *Report, chain []probe.Protocol, route Route, routeErr error, repeated repeat) {
	limits := w.investigator.limits
	failing := w.observation.Failed
	// Every rule below names the hypothesis it concludes from and writes the
	// body of its conclusion. The clamp then settles the classification against
	// that hypothesis and the summary is written, so a report can neither claim
	// a level its own evidence does not carry nor contradict itself about how
	// sure it is.
	body := ""
	w.reproduced = repeated.CameBack
	defer func() {
		clamp(report)
		report.Summary = summarise(report, body)
	}()

	// Rule one: the kernel itself says there is no route. That is not an
	// inference, so it can be confirmed, and it is asked before the probes are
	// read because the routing table answers whether or not a probe could run.
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

	// Rule two: a measurement that could not be taken is not a measurement of
	// the network, so nothing can be concluded from it.
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

	// Rule three: nothing answered at all. A host that is down and a path that
	// drops look identical from the client, and this evidence cannot separate
	// them, so no layer is named.
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

	// Rule four: the path is impaired rather than broken. Loss or a round trip
	// far above the healthy measurement is a degradation, and it is reported as
	// one rather than as a layer that is simply broken. It is asked before any
	// layer is blamed, because a path that answers badly is not a path that is
	// broken, whatever else the evidence shows.
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

	// Rule five: something failed, and the lowest layer that failed is the one
	// to blame. Blaming the layer that was observed failing is not the same
	// thing: a TLS handshake that fails because the connection was refused is a
	// transport failure, and a report that named TLS would send a reader after
	// certificates when the port was never open.
	blame, below, named := blameLayer(report, chain)
	if !named {
		// Everything measured above the highest layer that works came back
		// working, so nothing failed where the evidence could see it.
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

	// Rule six: the blamed layer failed with a specific answer, and every layer
	// under it answered. What that answer is decides how far the evidence
	// carries the conclusion.
	confidence := Probable
	rule := ruleSpecific
	switch {
	case blame.Result.Failure == probe.FailureTimeout:
		// A timeout could be a slow server and could be a broken one, and this
		// evidence cannot tell the two apart.
		confidence = Possible
		rule = ruleTimeout
	case repeated.CameBack:
		// A refusal is a positive answer from a host that answered, and a
		// resolver code or a status line is a specific answer. Measured twice,
		// nothing else fits.
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
		// The failure was seen at one layer and happened at another. Saying so
		// is the difference between a usable report and a misleading one.
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

// blameLayer finds the layer to blame, and the evidence of the layers under it
// that is working.
//
// It is the lowest layer that failed, not the layer that was observed failing.
// A TLS handshake refused by a closed port failed at the transport, and an
// ICMP probe that gets no reply from a host that answers TCP is not a broken
// network. A layer is only blameable when every layer above the highest working
// one, and below it, was measured working: otherwise something below the
// failure could be its cause, and saying otherwise would be a guess.
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
	// Everything between the highest working layer and the blamed one has to
	// have been measured, or the gap in the evidence sits exactly where the
	// blame would go.
	for position := highestWorking + 1; position < blame; position++ {
		if lastIndexOfLayer(report.Evidence, chain[position].Layer()) < 0 {
			return Evidence{}, nil, false
		}
	}
	index := lastIndexOfLayer(report.Evidence, chain[blame].Layer())
	return report.Evidence[index], workingIndexes(report.Evidence), true
}

// workingIndexes lists the evidence that shows a layer answering.
func workingIndexes(evidence []Evidence) []int {
	var out []int
	for index, item := range evidence {
		if item.Supports() {
			out = append(out, index)
		}
	}
	return out
}

// hypothesesFor builds the explanation for each layer, marking the ones the
// evidence refutes. A refuted hypothesis stays in the report with its
// refutation: a reader can see what was considered and why it was dropped,
// which is what makes the surviving explanation worth anything.
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

// statementFor writes what a failing measurement showed, in the words of the
// measurement rather than in the words of an assumption about it.
func (w *work) statementFor(evidence Evidence) string {
	if evidence.Result.Failure == probe.FailureRefused {
		// A refusal is a positive answer, so it can name the port that gave it.
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

// statementForLayer is the explanation a healthy layer offers, which is that
// it is not the cause.
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

// brokenEvidence finds the evidence that says a measurement could not be
// taken, and its index.
func brokenEvidence(evidence []Evidence) (int, bool) {
	for index, item := range evidence {
		if item.Attempted && !item.Probed {
			return index, true
		}
	}
	return -1, false
}

// summarise writes the conclusion, opening with the classification the clamp
// actually allowed. The prefix comes from the settled level rather than from the
// level a rule reached for, so the sentence and the level cannot disagree.
func summarise(report *Report, body string) string {
	if body == "" {
		return fmt.Sprintf("%s: the evidence does not support a conclusion.", report.Diagnosis)
	}
	return report.Diagnosis.String() + ": " + body
}

// order is how the layers stack, lowest first.
var order = map[string]int{
	layerNetwork:   0,
	layerRouting:   1,
	layerTransport: 2,
	layerDNS:       3,
	layerTLS:       4,
	layerHTTP:      5,
}

// belowLayer reports whether one layer is beneath another.
func belowLayer(lower, higher string) bool {
	low, ok := order[lower]
	if !ok {
		return false
	}
	high, ok := order[higher]
	if !ok {
		return false
	}
	return low < high
}

// impairment finds evidence that the path is degraded rather than broken.
// impairment finds the measurement that says the path is carrying some traffic
// badly rather than not at all.
func impairment(evidence []Evidence, limits Limits) *Evidence {
	for index := range evidence {
		item := evidence[index]
		if item.Layer != layerNetwork || !item.Probed {
			continue
		}
		// A probe that received nothing has not measured how much the path drops.
		// It has measured that nothing arrived, which a host that is down, a path
		// that is gone and a path that drops every packet all produce, and its
		// loss figure is then the hundred percent the probe assumes rather than
		// the hundred percent the wire showed. Reading that as a degradation
		// would name a layer for evidence that cannot name one, and it would put
		// a number in the summary that looks measured and is not. A probe that
		// got some replies back, on the other hand, has measured the path: the
		// traffic that arrived did, and the difference is the loss.
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

// impairmentStatement describes what the degradation was, from the measurement.
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

// anyWorking reports whether any probe in the evidence succeeded.
func anyWorking(evidence []Evidence) bool {
	for _, item := range evidence {
		if item.Supports() {
			return true
		}
	}
	return false
}

// refutingIndexes lists the evidence that shows something failing.
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

// citing renders a list of supporting indexes for a sentence.
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
