package investigator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/probe"
)

type Limits struct {
	MaxProbes int

	MaxNameLength int

	Timeout time.Duration

	MaxLoss float64

	HighRTT time.Duration
}

func DefaultLimits() Limits {
	return Limits{
		MaxProbes:     12,
		MaxNameLength: 64,
		Timeout:       5 * time.Second,
		MaxLoss:       1,
		HighRTT:       100 * time.Millisecond,
	}
}

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

type Prober interface {
	Probe(ctx context.Context, request probe.Request) (probe.Result, error)
}

type Route struct {
	Found bool

	Via string

	Device string

	Source string

	Reason string
}

type RouteReader interface {
	Route(ctx context.Context, namespace, target string) (Route, error)
}

type Investigator struct {
	prober Prober
	routes RouteReader
	limits Limits
}

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

func (i *Investigator) Limits() Limits { return i.limits }

type Report struct {
	Observation Observation

	Diagnosis Classification

	Layer string

	FailureDomain string

	Summary string

	Rule string

	Evidence []Evidence

	Hypotheses []Hypothesis

	ProbesRun int

	NotInvestigated []string
}

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

func (r Report) EvidenceAt(indexes []int) []Evidence {
	out := make([]Evidence, 0, len(indexes))
	for _, index := range indexes {
		if index >= 0 && index < len(r.Evidence) {
			out = append(out, r.Evidence[index])
		}
	}
	return out
}

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

	route, routeErr := i.routes.Route(ctx, checked.Namespace, checked.Target)
	report.Evidence = append(report.Evidence, working.routeEvidence(route, routeErr))

	chain := chain(checked.Failed)
	for _, protocol := range chain {
		if err := working.measure(ctx, &report, protocol); err != nil {
			return Report{}, fmt.Errorf("%s: %w", op, err)
		}
	}

	repeated := working.measureRepeat(ctx, &report, checked.Failed)

	working.explain(&report, chain, route, routeErr, repeated)
	return report, nil
}

type work struct {
	investigator *Investigator
	observation  Observation
	budget       int

	reproduced bool
}

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

type repeat struct {
	Ran bool

	CameBack bool
}

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

		request.Name, request.Path = "", ""
		switch protocol {
		case probe.ProtocolICMP:

			request.Port = 0
			request.Payload = 32
		case probe.ProtocolDNS:

			request.Port = 0
			request.Name = ""
		case probe.ProtocolUDP:

			if failingUsesTCP(observation.Failed) {
				request.Port = 0
			}
		}
	}
	return request, nil
}

func failingUsesTCP(protocol probe.Protocol) bool {
	switch protocol {
	case probe.ProtocolTCP, probe.ProtocolTLS, probe.ProtocolHTTP:
		return true
	default:
		return false
	}
}

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

func chain(failed probe.Protocol) []probe.Protocol {
	switch failed {
	case probe.ProtocolICMP:
		return []probe.Protocol{probe.ProtocolICMP}
	case probe.ProtocolTCP:
		return []probe.Protocol{probe.ProtocolICMP, probe.ProtocolTCP}
	case probe.ProtocolUDP:
		return []probe.Protocol{probe.ProtocolICMP, probe.ProtocolUDP}
	case probe.ProtocolDNS:

		return []probe.Protocol{probe.ProtocolICMP, probe.ProtocolDNS}
	case probe.ProtocolTLS:
		return []probe.Protocol{probe.ProtocolICMP, probe.ProtocolTCP, probe.ProtocolTLS}
	case probe.ProtocolHTTP:
		return []probe.Protocol{probe.ProtocolICMP, probe.ProtocolTCP, probe.ProtocolHTTP}
	default:
		return []probe.Protocol{failed}
	}
}

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
