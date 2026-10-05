package investigator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/probe"
)

// Limits bound what one investigation may cost. An investigation runs several
// probes and asks the kernel a question, so it is bounded like anything else
// that originates traffic.
type Limits struct {
	// MaxProbes is how many probes one investigation may run, including the
	// repeat of the failing one.
	MaxProbes int
	// MaxNameLength is the longest experiment name an observation may carry.
	MaxNameLength int
	// Timeout bounds each probe and each routing question.
	Timeout time.Duration
	// MaxLoss is the measured loss, as a percentage, at or above which the
	// path counts as impaired rather than working.
	MaxLoss float64
	// HighRTT is the round trip above which the path counts as degraded even
	// when nothing was lost.
	HighRTT time.Duration
}

// DefaultLimits returns the limits used when a caller configures none.
func DefaultLimits() Limits {
	return Limits{
		MaxProbes:     12,
		MaxNameLength: 64,
		Timeout:       5 * time.Second,
		MaxLoss:       1,
		HighRTT:       100 * time.Millisecond,
	}
}

// check validates the limits.
func (l Limits) check() (Limits, error) {
	fallback := DefaultLimits()
	if l.MaxProbes <= 0 {
		l.MaxProbes = fallback.MaxProbes
	}
	if l.MaxNameLength <= 0 {
		l.MaxNameLength = fallback.MaxNameLength
	}
	if l.Timeout <= 0 {
		l.Timeout = fallback.Timeout
	}
	if l.MaxLoss <= 0 {
		l.MaxLoss = fallback.MaxLoss
	}
	if l.HighRTT <= 0 {
		l.HighRTT = fallback.HighRTT
	}
	return l, nil
}

// Prober measures one protocol from inside a laboratory namespace. The probe
// engine satisfies it, which is what keeps an investigation inside the same
// scope, timeout, concurrency and size bounds as any other measurement.
type Prober interface {
	Probe(ctx context.Context, request probe.Request) (probe.Result, error)
}

// Route is what the kernel says it would do to reach a target.
type Route struct {
	// Found reports whether the kernel has a route at all.
	Found bool
	// Via is the next hop, when there is one.
	Via string
	// Device is the interface the packet would leave by.
	Device string
	// Source is the address the packet would be sent from.
	Source string
	// Reason is why there is no route, when there is none.
	Reason string
}

// RouteReader asks the kernel about a route from one namespace. The laboratory
// satisfies it by running the kernel's own route lookup in that namespace, so
// the answer is the routing table's and not a reconstruction.
type RouteReader interface {
	Route(ctx context.Context, namespace, target string) (Route, error)
}

// Investigator collects evidence and explains a failure.
type Investigator struct {
	prober Prober
	routes RouteReader
	limits Limits
}

// New builds an investigator. Both a prober and a route reader are required:
// without either, the investigator would have to guess about a layer it cannot
// measure, which is the one thing it must never do.
func New(prober Prober, routes RouteReader, limits Limits) (*Investigator, error) {
	const op = "investigator.New"
	if prober == nil {
		return nil, apperr.InvalidArgument(op, "an investigator needs something to measure with", nil)
	}
	if routes == nil {
		return nil, apperr.InvalidArgument(op, "an investigator needs a way to ask about routes", nil)
	}
	checked, err := limits.check()
	if err != nil {
		return nil, err
	}
	return &Investigator{prober: prober, routes: routes, limits: checked}, nil
}

// Limits returns the limits the investigator enforces.
func (i *Investigator) Limits() Limits { return i.limits }

// Report is what an investigation concluded.
type Report struct {
	// Observation is the failure that was investigated.
	Observation Observation
	// Diagnosis is how far the evidence supports the conclusion.
	Diagnosis Classification
	// Layer is the layer the failure is attributed to.
	Layer string
	// FailureDomain names the path the failure is on.
	FailureDomain string
	// Summary is the conclusion in a sentence, citing the evidence it rests
	// on by index.
	Summary string
	// Rule is the name of the rule that produced the classification, so a
	// reader can see the reasoning rather than only its result.
	Rule string
	// Evidence is everything that was measured, in the order it was taken.
	Evidence []Evidence
	// Hypotheses are the explanations considered, supported or not.
	Hypotheses []Hypothesis
	// ProbesRun is how many probes the investigation took.
	ProbesRun int
	// NotInvestigated names the layers that were left out and why.
	NotInvestigated []string
}

// Cite renders the evidence indexes a statement rests on, so every claim in a
// report can be traced to a measurement.
func (r Report) Cite(indexes []int) string {
	if len(indexes) == 0 {
		return "no evidence"
	}
	parts := make([]string, 0, len(indexes))
	for _, index := range indexes {
		if index >= 0 && index < len(r.Evidence) {
			parts = append(parts, fmt.Sprintf("%d", index))
		}
	}
	if len(parts) == 0 {
		return "no evidence"
	}
	return "evidence " + strings.Join(parts, ", ")
}

// EvidenceAt returns the evidence a hypothesis cites.
func (r Report) EvidenceAt(indexes []int) []Evidence {
	out := make([]Evidence, 0, len(indexes))
	for _, index := range indexes {
		if index >= 0 && index < len(r.Evidence) {
			out = append(out, r.Evidence[index])
		}
	}
	return out
}

// Investigate runs the pipeline: take the observation, collect evidence from
// the layer that failed and every layer below it, generate the explanations
// that evidence supports, repeat the failing measurement to see whether it
// reproduces, and report only what all of that supports.
func (i *Investigator) Investigate(ctx context.Context, observation Observation) (Report, error) {
	const op = "investigator.Investigate"
	checked, err := observation.Validate(i.limits)
	if err != nil {
		return Report{}, err
	}
	working := &work{
		investigator: i,
		observation:  checked,
		budget:       i.limits.MaxProbes,
	}
	report := Report{
		Observation:   checked,
		FailureDomain: domain(checked),
		Diagnosis:     Unknown,
		Rule:          ruleInconclusive,
	}

	// Step one: what does the kernel think it would do? A missing route
	// explains every failure above it, so it is asked before anything is
	// measured.
	route, routeErr := i.routes.Route(ctx, checked.Namespace, checked.Target)
	report.Evidence = append(report.Evidence, working.routeEvidence(route, routeErr))

	// Step two: measure the failing layer and everything below it, lowest
	// first, so a failure at one layer is read against the layers that work.
	chain := chain(checked.Failed)
	for _, protocol := range chain {
		if err := working.measure(ctx, &report, protocol); err != nil {
			return Report{}, fmt.Errorf("%s: %w", op, err)
		}
	}
	// The failing layer is measured again. One failure can be a blip; the same
	// failure twice is a behaviour.
	repeated := working.measureRepeat(ctx, &report, checked.Failed)

	// Step three and four: the explanations the evidence supports, and how far
	// it carries each of them.
	working.explain(&report, chain, route, routeErr, repeated)
	return report, nil
}

// work carries the state of one investigation.
type work struct {
	investigator *Investigator
	observation  Observation
	budget       int
	// reproduced records whether the failing measurement came back the same
	// way twice, which is what separates a steady fault from a moment.
	reproduced bool
}

// measure runs one probe and records the evidence it produced.
func (w *work) measure(ctx context.Context, report *Report, protocol probe.Protocol) error {
	request, err := w.request(protocol)
	if err != nil {
		return err
	}
	if w.budget <= 0 {
		report.NotInvestigated = append(report.NotInvestigated,
			string(protocol)+" (the probe budget was spent)")
		return nil
	}
	w.budget--
	report.ProbesRun++

	probeCtx, cancel := context.WithTimeout(ctx, w.investigator.limits.Timeout)
	defer cancel()
	result, runErr := w.investigator.prober.Probe(probeCtx, request)
	if runErr != nil {
		// A laboratory problem is recorded as such rather than as a
		// measurement of the network.
		report.Evidence = append(report.Evidence, Evidence{
			Layer:     protocol.Layer(),
			What:      string(protocol),
			Attempted: true,
			Note:      "the measurement could not be taken: " + runErr.Error(),
			At:        time.Now(),
		})
		return nil
	}
	report.Evidence = append(report.Evidence, evidenceFrom(protocol, result))
	return nil
}

// repeat is what the second measurement of the failing layer showed. Three
// outcomes, not two: a fault that came back, a fault that went away, and a
// measurement that never happened. "It did not reproduce" and "it was not
// measured twice" are different, and only the first of them makes a fault
// intermittent.
type repeat struct {
	// Ran reports whether the second measurement was taken at all.
	Ran bool
	// CameBack reports whether it failed the same way the first one did, which
	// is what tells a steady fault from a moment.
	CameBack bool
}

// measureRepeat measures the failing layer a second time.
func (w *work) measureRepeat(ctx context.Context, report *Report, protocol probe.Protocol) repeat {
	before := len(report.Evidence)
	if err := w.measure(ctx, report, protocol); err != nil {
		return repeat{}
	}
	if len(report.Evidence) == before {
		return repeat{}
	}
	again := report.Evidence[len(report.Evidence)-1]
	first := report.Evidence[len(report.Evidence)-2]
	return repeat{
		Ran:      true,
		CameBack: first.Refutes() && again.Refutes() && first.Result.Failure == again.Result.Failure,
	}
}

// request builds the probe request for a protocol from the observation. The
// port, name and path come from what failed, so the investigator measures the
// same thing that was observed failing rather than a convenient neighbour.
func (w *work) request(protocol probe.Protocol) (probe.Request, error) {
	observation := w.observation
	request := probe.Request{
		Protocol:   protocol,
		Namespace:  observation.Namespace,
		Target:     observation.Target,
		Port:       observation.Port,
		Name:       observation.Name,
		Path:       observation.Path,
		Experiment: observation.Experiment + "/investigation",
		Timeout:    w.investigator.limits.Timeout,
	}
	if protocol != observation.Failed {
		// A layer below the failure has no opinion about the name, the path or
		// the status the layer above expects. It does care about the port,
		// though: the transport under a failed TLS or HTTP exchange is the same
		// connection, on the same port, and measuring a different one would say
		// nothing about the failure.
		request.Name, request.Path = "", ""
		switch protocol {
		case probe.ProtocolICMP:
			// Nothing at the IP layer has a port, and the echo probe needs a
			// payload to measure.
			request.Port = 0
			request.Payload = 32
		case probe.ProtocolDNS:
			// A resolver is reached on its own port, and a failure above it
			// carries no resolver to ask.
			request.Port = 0
			request.Name = ""
		case probe.ProtocolUDP:
			// A datagram path is only the same path when the layer above it was
			// a datagram layer.
			if failingUsesTCP(observation.Failed) {
				request.Port = 0
			}
		}
	}
	return request, nil
}

// failingUsesTCP reports whether a failure happened over a stream, which is what
// makes the transport below it the same connection.
func failingUsesTCP(protocol probe.Protocol) bool {
	switch protocol {
	case probe.ProtocolTCP, probe.ProtocolTLS, probe.ProtocolHTTP:
		return true
	default:
		return false
	}
}

// routeEvidence records what the kernel said about the route.
func (w *work) routeEvidence(route Route, err error) Evidence {
	at := time.Now()
	if err != nil {
		return Evidence{
			Layer:  layerRouting,
			What:   "route",
			Note:   "the routing table could not be asked: " + err.Error(),
			At:     at,
			Probed: false,
		}
	}
	if !route.Found {
		note := "the kernel has no route to the target"
		if route.Reason != "" {
			note += ": " + route.Reason
		}
		return Evidence{Layer: layerRouting, What: "route", Note: note, At: at}
	}
	note := "the kernel routes to the target"
	if route.Device != "" {
		note += " through " + route.Device
	}
	if route.Via != "" {
		note += " via " + route.Via
	}
	return Evidence{Layer: layerRouting, What: "route", Note: note, At: at}
}

// evidenceFrom writes what a probe result showed, in words taken from the
// measurement.
func evidenceFrom(protocol probe.Protocol, result probe.Result) Evidence {
	note := result.Detail
	if result.Succeeded() {
		if result.RTT > 0 {
			note = strings.TrimSpace(note + ", " + result.RTT.Round(100*time.Microsecond).String())
		}
	} else {
		failure := string(result.Failure)
		if failure == "" {
			failure = "failed"
		}
		note = strings.TrimSpace(failure + ": " + result.Error)
	}
	if protocol == probe.ProtocolICMP && result.Loss > 0 {
		note = strings.TrimSpace(note)
	}
	return Evidence{
		Layer:  protocol.Layer(),
		What:   string(protocol),
		Probed: true,
		Result: result,
		Note:   note,
		At:     result.At,
	}
}

// chain is the protocols to measure for a failure at the given layer, lowest
// first. It is the stack below and including the failure, and nothing above it,
// because a layer above the failure cannot say anything about why it failed.
func chain(failed probe.Protocol) []probe.Protocol {
	switch failed {
	case probe.ProtocolICMP:
		return []probe.Protocol{probe.ProtocolICMP}
	case probe.ProtocolTCP:
		return []probe.Protocol{probe.ProtocolICMP, probe.ProtocolTCP}
	case probe.ProtocolUDP:
		return []probe.Protocol{probe.ProtocolICMP, probe.ProtocolUDP}
	case probe.ProtocolDNS:
		// A resolver is reached over a datagram, so the resolver probe is
		// also the transport evidence for it. A separate echo probe would
		// prove nothing a resolver probe does not.
		return []probe.Protocol{probe.ProtocolICMP, probe.ProtocolDNS}
	case probe.ProtocolTLS:
		return []probe.Protocol{probe.ProtocolICMP, probe.ProtocolTCP, probe.ProtocolTLS}
	case probe.ProtocolHTTP:
		return []probe.Protocol{probe.ProtocolICMP, probe.ProtocolTCP, probe.ProtocolHTTP}
	default:
		return []probe.Protocol{failed}
	}
}

// domain names the path a failure is on.
func domain(observation Observation) string {
	address := observation.Target
	if observation.Port != 0 {
		address = fmt.Sprintf("%s:%d", observation.Target, observation.Port)
	}
	if observation.Name != "" {
		address += " (" + observation.Name + ")"
	}
	return observation.Namespace + " -> " + address
}
