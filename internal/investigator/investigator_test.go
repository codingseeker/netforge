package investigator

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/netforge/netforge/internal/probe"
)

type scriptedProber struct {
	mu      sync.Mutex
	script  map[probe.Protocol][]probe.Result
	calls   []probe.Request
	failing error
}

func (p *scriptedProber) Probe(_ context.Context, request probe.Request) (probe.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, request)
	if p.failing != nil {
		return probe.Result{}, p.failing
	}
	results := p.script[request.Protocol]
	if len(results) == 0 {
		return probe.Result{}, fmt.Errorf("no script for %s", request.Protocol)
	}
	result := results[0]
	if len(results) > 1 {
		p.script[request.Protocol] = results[1:]
	}
	result.Protocol = request.Protocol
	result.Namespace = request.Namespace
	result.Target = request.Target
	if result.At.IsZero() {
		result.At = time.Now()
	}
	return result, nil
}

func (p *scriptedProber) requested() []probe.Protocol {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]probe.Protocol, 0, len(p.calls))
	for _, request := range p.calls {
		out = append(out, request.Protocol)
	}
	return out
}

func (p *scriptedProber) count(protocol probe.Protocol) int {
	total := 0
	for _, asked := range p.requested() {
		if asked == protocol {
			total++
		}
	}
	return total
}

func ok(protocol probe.Protocol) probe.Result {
	result := probe.Result{Protocol: protocol, OK: true, RTT: 3 * time.Millisecond}
	switch protocol {
	case probe.ProtocolICMP:
		result.Sent, result.Received, result.Loss = 3, 3, 0
		result.Detail = "3 of 3 replies, 0.0% loss"
	case probe.ProtocolHTTP:
		result.Detail = "HTTP 200"
	case probe.ProtocolDNS:
		result.Detail = "service.lab = 10.60.0.2"
	default:
		result.Detail = "answered"
	}
	return result
}

func failed(protocol probe.Protocol, failure probe.Failure, message string) probe.Result {
	return probe.Result{
		Protocol: protocol,
		Failure:  failure,
		Error:    message,
		Detail:   string(failure),
	}
}

type scriptedRoutes struct {
	routes map[string]Route
	err    error
	calls  int
}

func (r *scriptedRoutes) Route(_ context.Context, _, target string) (Route, error) {
	r.calls++
	if r.err != nil {
		return Route{}, r.err
	}
	route, ok := r.routes[target]
	if !ok {
		return Route{Found: true, Device: "eth0"}, nil
	}
	return route, nil
}

func observation(failedProtocol probe.Protocol, port uint16) Observation {
	return Observation{
		Experiment: "unit",
		Namespace:  "nf-client0",
		Target:     "10.60.0.2",
		Port:       port,
		Name:       "service.lab",
		Path:       "/health",
		Failed:     failedProtocol,
		Failure:    probe.FailureTimeout,
		Error:      "the first measurement failed",
		At:         time.Now(),
	}
}

func newInvestigator(t *testing.T, prober Prober, routes RouteReader, limits Limits) *Investigator {
	t.Helper()
	engine, err := New(prober, routes, limits)
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	return engine
}

func TestAMissingRouteIsConfirmed(t *testing.T) {
	t.Parallel()
	prober := &scriptedProber{script: map[probe.Protocol][]probe.Result{
		probe.ProtocolICMP: {failed(probe.ProtocolICMP, probe.FailureTimeout, "no reply")},
		probe.ProtocolTCP:  {failed(probe.ProtocolTCP, probe.FailureTimeout, "no reply")},
	}}
	routes := &scriptedRoutes{routes: map[string]Route{
		"10.60.0.2": {Found: false, Reason: "unreachable"},
	}}
	engine := newInvestigator(t, prober, routes, Limits{})

	report, err := engine.Investigate(context.Background(), observation(probe.ProtocolTCP, 9100))
	if err != nil {
		t.Fatalf("Investigate() = %v, want nil", err)
	}
	if report.Diagnosis != Confirmed {
		t.Errorf("Diagnosis = %s, want %s", report.Diagnosis, Confirmed)
	}
	if report.Layer != layerRouting {
		t.Errorf("Layer = %q, want %q", report.Layer, layerRouting)
	}
	if report.Rule != ruleNoRoute {
		t.Errorf("Rule = %q, want %q", report.Rule, ruleNoRoute)
	}
	if !strings.Contains(report.Summary, "no route") {
		t.Errorf("Summary = %q, want it to say there is no route", report.Summary)
	}
	if !strings.Contains(report.Summary, "evidence 0") {
		t.Errorf("Summary = %q, want it to cite the routing evidence", report.Summary)
	}
}

func TestARefusedPortIsConfirmedWhenItReproduces(t *testing.T) {
	t.Parallel()
	prober := &scriptedProber{script: map[probe.Protocol][]probe.Result{
		probe.ProtocolICMP: {ok(probe.ProtocolICMP)},
		probe.ProtocolTCP: {
			failed(probe.ProtocolTCP, probe.FailureRefused, "connect: connection refused"),
			failed(probe.ProtocolTCP, probe.FailureRefused, "connect: connection refused"),
		},
	}}
	engine := newInvestigator(t, prober, &scriptedRoutes{}, Limits{})

	report, err := engine.Investigate(context.Background(), observation(probe.ProtocolTCP, 9100))
	if err != nil {
		t.Fatalf("Investigate() = %v, want nil", err)
	}
	if report.Diagnosis != Confirmed {
		t.Errorf("Diagnosis = %s, want %s", report.Diagnosis, Confirmed)
	}
	if report.Layer != layerTransport {
		t.Errorf("Layer = %q, want %q", report.Layer, layerTransport)
	}
	if report.Rule != ruleRefused {
		t.Errorf("Rule = %q, want %q", report.Rule, ruleRefused)
	}
	if !strings.Contains(report.Summary, "9100") {
		t.Errorf("Summary = %q, want it to name the port", report.Summary)
	}
	if prober.count(probe.ProtocolTCP) != 2 {
		t.Errorf("the failing layer was measured %d times, want 2",
			prober.count(probe.ProtocolTCP))
	}

	if !hasWorking(report, layerNetwork) {
		t.Error("the network layer was not measured working, so the refusal proves nothing")
	}
}

func TestARefusedPortMeasuredOnceIsOnlyProbable(t *testing.T) {
	t.Parallel()
	prober := &scriptedProber{script: map[probe.Protocol][]probe.Result{
		probe.ProtocolICMP: {ok(probe.ProtocolICMP)},
		probe.ProtocolTCP: {
			failed(probe.ProtocolTCP, probe.FailureRefused, "connect: connection refused"),
			ok(probe.ProtocolTCP),
		},
	}}
	engine := newInvestigator(t, prober, &scriptedRoutes{}, Limits{})

	report, err := engine.Investigate(context.Background(), observation(probe.ProtocolTCP, 9100))
	if err != nil {
		t.Fatalf("Investigate() = %v, want nil", err)
	}
	if report.Diagnosis == Confirmed {
		t.Errorf("Diagnosis = %s, want less than %s for a single sample", report.Diagnosis, Confirmed)
	}
}

func TestAResolverFailureIsADNSFailure(t *testing.T) {
	t.Parallel()
	prober := &scriptedProber{script: map[probe.Protocol][]probe.Result{
		probe.ProtocolICMP: {ok(probe.ProtocolICMP)},
		probe.ProtocolDNS: {
			failed(probe.ProtocolDNS, probe.FailureDNS, "the resolver answered nxdomain"),
			failed(probe.ProtocolDNS, probe.FailureDNS, "the resolver answered nxdomain"),
		},
	}}
	engine := newInvestigator(t, prober, &scriptedRoutes{}, Limits{})

	report, err := engine.Investigate(context.Background(), observation(probe.ProtocolDNS, 53))
	if err != nil {
		t.Fatalf("Investigate() = %v, want nil", err)
	}
	if report.Layer != layerDNS {
		t.Errorf("Layer = %q, want %q", report.Layer, layerDNS)
	}
	if report.Diagnosis != Confirmed {
		t.Errorf("Diagnosis = %s, want %s for a reproduced resolver failure", report.Diagnosis, Confirmed)
	}
	if !strings.Contains(report.Summary, "nxdomain") {
		t.Errorf("Summary = %q, want it to carry what the resolver said", report.Summary)
	}
	if prober.count(probe.ProtocolTCP) != 0 {
		t.Error("a dns failure was measured with a tcp probe, which proves nothing about a datagram path")
	}
}

func TestATimeoutAtALayerIsNotConfirmed(t *testing.T) {
	t.Parallel()
	prober := &scriptedProber{script: map[probe.Protocol][]probe.Result{
		probe.ProtocolICMP: {ok(probe.ProtocolICMP)},
		probe.ProtocolTCP:  {ok(probe.ProtocolTCP)},
		probe.ProtocolHTTP: {
			failed(probe.ProtocolHTTP, probe.FailureTimeout, "the request timed out"),
			failed(probe.ProtocolHTTP, probe.FailureTimeout, "the request timed out"),
		},
	}}
	engine := newInvestigator(t, prober, &scriptedRoutes{}, Limits{})

	report, err := engine.Investigate(context.Background(), observation(probe.ProtocolHTTP, 9104))
	if err != nil {
		t.Fatalf("Investigate() = %v, want nil", err)
	}
	if report.Layer != layerHTTP {
		t.Errorf("Layer = %q, want %q", report.Layer, layerHTTP)
	}
	if report.Diagnosis != Possible {
		t.Errorf("Diagnosis = %s, want %s for a timeout: a slow server and a broken one look the same",
			report.Diagnosis, Possible)
	}
}

func TestAnUnexpectedStatusIsConfirmed(t *testing.T) {
	t.Parallel()
	bad := failed(probe.ProtocolHTTP, probe.FailureHTTP, "HTTP 503")
	bad.Detail = "HTTP 503"
	prober := &scriptedProber{script: map[probe.Protocol][]probe.Result{
		probe.ProtocolICMP: {ok(probe.ProtocolICMP)},
		probe.ProtocolTCP:  {ok(probe.ProtocolTCP)},
		probe.ProtocolHTTP: {bad, bad},
	}}
	engine := newInvestigator(t, prober, &scriptedRoutes{}, Limits{})

	report, err := engine.Investigate(context.Background(), observation(probe.ProtocolHTTP, 9104))
	if err != nil {
		t.Fatalf("Investigate() = %v, want nil", err)
	}
	if report.Diagnosis != Confirmed {
		t.Errorf("Diagnosis = %s, want %s for a reproduced specific answer", report.Diagnosis, Confirmed)
	}
	if !strings.Contains(report.Summary, "503") {
		t.Errorf("Summary = %q, want it to carry the status", report.Summary)
	}
}

func TestATLSHandshakeFailureIsATLSFailure(t *testing.T) {
	t.Parallel()
	handshake := failed(probe.ProtocolTLS, probe.FailureTLS,
		"x509: certificate signed by unknown authority")
	prober := &scriptedProber{script: map[probe.Protocol][]probe.Result{
		probe.ProtocolICMP: {ok(probe.ProtocolICMP)},
		probe.ProtocolTCP:  {ok(probe.ProtocolTCP)},
		probe.ProtocolTLS:  {handshake, handshake},
	}}
	engine := newInvestigator(t, prober, &scriptedRoutes{}, Limits{})

	report, err := engine.Investigate(context.Background(), observation(probe.ProtocolTLS, 9103))
	if err != nil {
		t.Fatalf("Investigate() = %v, want nil", err)
	}
	if report.Layer != layerTLS {
		t.Errorf("Layer = %q, want %q", report.Layer, layerTLS)
	}
	if report.Diagnosis != Confirmed {
		t.Errorf("Diagnosis = %s, want %s", report.Diagnosis, Confirmed)
	}
	if !strings.Contains(report.Summary, "certificate") {
		t.Errorf("Summary = %q, want it to carry what the handshake said", report.Summary)
	}
}

func TestMeasuredLossIsADegradation(t *testing.T) {
	t.Parallel()
	lossy := ok(probe.ProtocolICMP)
	lossy.Sent, lossy.Received, lossy.Loss = 20, 14, 30
	lossy.Detail = "14 of 20 replies, 30.0% loss"
	prober := &scriptedProber{script: map[probe.Protocol][]probe.Result{
		probe.ProtocolICMP: {lossy, lossy},
		probe.ProtocolTCP:  {ok(probe.ProtocolTCP)},
		probe.ProtocolHTTP: {ok(probe.ProtocolHTTP)},
	}}
	engine := newInvestigator(t, prober, &scriptedRoutes{}, Limits{})

	report, err := engine.Investigate(context.Background(), observation(probe.ProtocolHTTP, 9104))
	if err != nil {
		t.Fatalf("Investigate() = %v, want nil", err)
	}
	if report.Rule != ruleDegraded {
		t.Errorf("Rule = %q, want %q", report.Rule, ruleDegraded)
	}
	if report.Diagnosis != Probable {
		t.Errorf("Diagnosis = %s, want %s", report.Diagnosis, Probable)
	}
	if !strings.Contains(report.Summary, "30.0%") {
		t.Errorf("Summary = %q, want it to carry the measured loss", report.Summary)
	}
	if !strings.Contains(report.Summary, "impaired") {
		t.Errorf("Summary = %q, want it to say the path is impaired rather than broken", report.Summary)
	}
}

func TestNoRepliesIsNotAMeasuredDegradation(t *testing.T) {
	t.Parallel()
	blackout := failed(probe.ProtocolICMP, probe.FailureTimeout, "no reply to any of the 10 echo requests")
	blackout.Sent, blackout.Received, blackout.Loss = 10, 0, 100
	blackout.Detail = "0 of 10 replies, 100.0% loss"
	prober := &scriptedProber{script: map[probe.Protocol][]probe.Result{
		probe.ProtocolICMP: {blackout, blackout},

		probe.ProtocolTCP:  {ok(probe.ProtocolTCP)},
		probe.ProtocolHTTP: {ok(probe.ProtocolHTTP)},
	}}
	engine := newInvestigator(t, prober, &scriptedRoutes{}, Limits{})

	report, err := engine.Investigate(context.Background(), observation(probe.ProtocolHTTP, 9104))
	if err != nil {
		t.Fatalf("Investigate() = %v, want nil", err)
	}
	if report.Rule == ruleDegraded {
		t.Errorf("Rule = %q and Summary = %q, want silence not reported as a measured loss",
			report.Rule, report.Summary)
	}
	if strings.Contains(report.Summary, "measured loss") {
		t.Errorf("Summary = %q, want it not to carry a loss the probe never measured",
			report.Summary)
	}
	if report.Diagnosis == Confirmed {
		t.Errorf("Diagnosis = %s, want less than confirmed when no reply arrived", report.Diagnosis)
	}
}

func TestNothingAnsweringIsNotResolvedIntoOneLayer(t *testing.T) {
	t.Parallel()
	silent := func(protocol probe.Protocol) probe.Result {
		return failed(protocol, probe.FailureTimeout, "the request timed out")
	}
	prober := &scriptedProber{script: map[probe.Protocol][]probe.Result{
		probe.ProtocolICMP: {silent(probe.ProtocolICMP), silent(probe.ProtocolICMP)},
		probe.ProtocolTCP:  {silent(probe.ProtocolTCP), silent(probe.ProtocolTCP)},
	}}
	engine := newInvestigator(t, prober, &scriptedRoutes{}, Limits{})

	report, err := engine.Investigate(context.Background(), observation(probe.ProtocolTCP, 9100))
	if err != nil {
		t.Fatalf("Investigate() = %v, want nil", err)
	}
	if report.Diagnosis == Confirmed {
		t.Fatalf("Diagnosis = %s, want less than %s: a down host and a dropping path look the same",
			report.Diagnosis, Confirmed)
	}
	if !strings.Contains(report.Summary, "cannot be") {
		t.Errorf("Summary = %q, want it to say what could not be separated", report.Summary)
	}

	if !mentionsLayer(report, layerNetwork) || !mentionsLayer(report, layerRouting) {
		t.Errorf("the hypotheses are %v, want both a network and a routing explanation considered",
			layersOf(report))
	}
}

func TestAFailureThatDoesNotReproduceNamesNoLayer(t *testing.T) {
	t.Parallel()
	prober := &scriptedProber{script: map[probe.Protocol][]probe.Result{
		probe.ProtocolICMP: {ok(probe.ProtocolICMP)},
		probe.ProtocolTCP:  {ok(probe.ProtocolTCP)},
		probe.ProtocolHTTP: {
			failed(probe.ProtocolHTTP, probe.FailureTimeout, "the request timed out"),
			ok(probe.ProtocolHTTP),
		},
	}}
	engine := newInvestigator(t, prober, &scriptedRoutes{}, Limits{})

	report, err := engine.Investigate(context.Background(), observation(probe.ProtocolHTTP, 9104))
	if err != nil {
		t.Fatalf("Investigate() = %v, want nil", err)
	}
	if report.Rule != "did-not-reproduce" {
		t.Errorf("Rule = %q, want did-not-reproduce", report.Rule)
	}
	if !strings.Contains(report.Summary, "intermittent") {
		t.Errorf("Summary = %q, want it to call the fault intermittent", report.Summary)
	}
}

func TestARefusedMeasurementIsNotAWorkingLayerOrAFailingOne(t *testing.T) {
	t.Parallel()
	given := Evidence{Attempted: true, Note: "the measurement could not be taken: refused"}
	if given.Supports() {
		t.Error("Supports() = true, want false: nothing was measured")
	}
	if given.Refutes() {
		t.Error("Refutes() = true, want false: a missing measurement refutes nothing")
	}
	if given.Taken() {
		t.Error("Taken() = true, want false")
	}
	measured := Evidence{Probed: true, Result: failed(probe.ProtocolTCP, probe.FailureRefused, "refused")}
	if !measured.Refutes() {
		t.Error("Refutes() = false, want true: a refused connection is a real answer")
	}
	if measured.Attempted {
		t.Error("Attempted = true, want the flag to mean attempted and not measured")
	}
}

func TestAMeasurementThatCouldNotBeTakenConcludesNothing(t *testing.T) {
	t.Parallel()
	prober := &scriptedProber{
		script:  map[probe.Protocol][]probe.Result{probe.ProtocolICMP: {ok(probe.ProtocolICMP)}},
		failing: fmt.Errorf("the probe agent could not be run"),
	}
	engine := newInvestigator(t, prober, &scriptedRoutes{}, Limits{})

	report, err := engine.Investigate(context.Background(), observation(probe.ProtocolHTTP, 9104))
	if err != nil {
		t.Fatalf("Investigate() = %v, want nil", err)
	}
	if report.Diagnosis != Unknown {
		t.Errorf("Diagnosis = %s, want %s", report.Diagnosis, Unknown)
	}
	if report.Layer != "" {
		t.Errorf("Layer = %q, want no layer named", report.Layer)
	}
	if !strings.Contains(report.Summary, "could not be taken") {
		t.Errorf("Summary = %q, want it to say a measurement was missing", report.Summary)
	}
}

func TestEveryClaimCitesEvidenceThatExists(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		script  map[probe.Protocol][]probe.Result
		failed  probe.Protocol
		port    uint16
		routes  map[string]Route
		wantErr bool
	}{
		{
			name: "a refusal",
			script: map[probe.Protocol][]probe.Result{
				probe.ProtocolICMP: {ok(probe.ProtocolICMP)},
				probe.ProtocolTCP: {
					failed(probe.ProtocolTCP, probe.FailureRefused, "refused"),
					failed(probe.ProtocolTCP, probe.FailureRefused, "refused"),
				},
			},
			failed: probe.ProtocolTCP, port: 9100,
		},
		{
			name: "a resolver failure",
			script: map[probe.Protocol][]probe.Result{
				probe.ProtocolICMP: {ok(probe.ProtocolICMP)},
				probe.ProtocolDNS: {
					failed(probe.ProtocolDNS, probe.FailureDNS, "servfail"),
					failed(probe.ProtocolDNS, probe.FailureDNS, "servfail"),
				},
			},
			failed: probe.ProtocolDNS, port: 53,
		},
		{
			name: "a missing route",
			script: map[probe.Protocol][]probe.Result{
				probe.ProtocolICMP: {failed(probe.ProtocolICMP, probe.FailureTimeout, "no reply")},
				probe.ProtocolTCP:  {failed(probe.ProtocolTCP, probe.FailureTimeout, "no reply")},
			},
			failed: probe.ProtocolTCP, port: 9100,
			routes: map[string]Route{"10.60.0.2": {Found: false, Reason: "unreachable"}},
		},
		{
			name: "an http failure",
			script: map[probe.Protocol][]probe.Result{
				probe.ProtocolICMP: {ok(probe.ProtocolICMP)},
				probe.ProtocolTCP:  {ok(probe.ProtocolTCP)},
				probe.ProtocolHTTP: {
					failed(probe.ProtocolHTTP, probe.FailureHTTP, "HTTP 500"),
					failed(probe.ProtocolHTTP, probe.FailureHTTP, "HTTP 500"),
				},
			},
			failed: probe.ProtocolHTTP, port: 9104,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			engine := newInvestigator(t,
				&scriptedProber{script: test.script},
				&scriptedRoutes{routes: test.routes},
				Limits{})
			report, err := engine.Investigate(context.Background(), observation(test.failed, test.port))
			if err != nil {
				t.Fatalf("Investigate() = %v, want nil", err)
			}
			if len(report.Evidence) == 0 {
				t.Fatal("the report has no evidence, so nothing can be cited")
			}
			for _, hypothesis := range report.Hypotheses {
				for _, index := range append(append([]int{}, hypothesis.Evidence...), hypothesis.RefutedBy...) {
					if index < 0 || index >= len(report.Evidence) {
						t.Errorf("a hypothesis cites evidence %d, but the report has %d pieces",
							index, len(report.Evidence))
					}
				}
			}
			for _, cited := range citedIndexes(report.Summary) {
				if cited < 0 || cited >= len(report.Evidence) {
					t.Errorf("the summary cites evidence %d, but the report has %d pieces",
						cited, len(report.Evidence))
				}
			}
			if !strings.HasPrefix(report.Summary, string(report.Diagnosis)) {
				t.Errorf("Summary = %q, want it to open with the classification %s",
					report.Summary, report.Diagnosis)
			}
		})
	}
}

func TestAnInvestigationIsBounded(t *testing.T) {
	t.Parallel()
	prober := &scriptedProber{script: map[probe.Protocol][]probe.Result{
		probe.ProtocolICMP: {ok(probe.ProtocolICMP), ok(probe.ProtocolICMP), ok(probe.ProtocolICMP)},
		probe.ProtocolTCP:  {ok(probe.ProtocolTCP), ok(probe.ProtocolTCP), ok(probe.ProtocolTCP)},
		probe.ProtocolHTTP: {
			ok(probe.ProtocolHTTP), ok(probe.ProtocolHTTP), ok(probe.ProtocolHTTP),
			ok(probe.ProtocolHTTP), ok(probe.ProtocolHTTP), ok(probe.ProtocolHTTP),
		},
	}}
	engine := newInvestigator(t, prober, &scriptedRoutes{}, Limits{MaxProbes: 3})

	report, err := engine.Investigate(context.Background(), observation(probe.ProtocolHTTP, 9104))
	if err != nil {
		t.Fatalf("Investigate() = %v, want nil", err)
	}
	if report.ProbesRun > 3 {
		t.Errorf("ProbesRun = %d, want at most the 3 the limits allow", report.ProbesRun)
	}
	if len(report.NotInvestigated) == 0 {
		t.Error("NotInvestigated is empty, want the layers the budget could not reach named")
	}
}

func TestAnUnusableObservationIsRefused(t *testing.T) {
	t.Parallel()
	prober := &scriptedProber{}
	routes := &scriptedRoutes{}
	engine := newInvestigator(t, prober, routes, Limits{})

	cases := []struct {
		name   string
		change func(*Observation)
	}{
		{"an unknown protocol", func(o *Observation) { o.Failed = "smtp" }},
		{"no namespace", func(o *Observation) { o.Namespace = "" }},
		{"no target", func(o *Observation) { o.Target = "" }},
		{"no experiment", func(o *Observation) { o.Experiment = "" }},
		{"a name past the limit", func(o *Observation) { o.Experiment = strings.Repeat("x", 500) }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			given := observation(probe.ProtocolHTTP, 9104)
			test.change(&given)
			if _, err := engine.Investigate(context.Background(), given); err == nil {
				t.Errorf("Investigate(%s) = nil error, want a refusal", test.name)
			}
		})
	}
	if len(prober.requested()) != 0 {
		t.Errorf("%d probes ran for refused observations, want 0", len(prober.requested()))
	}
	if routes.calls != 0 {
		t.Errorf("the routing table was asked %d times for refused observations, want 0", routes.calls)
	}
}

func TestAnInvestigatorNeedsBothTools(t *testing.T) {
	t.Parallel()
	if _, err := New(nil, &scriptedRoutes{}, Limits{}); err == nil {
		t.Error("New() with no prober = nil error, want a refusal")
	}
	if _, err := New(&scriptedProber{}, nil, Limits{}); err == nil {
		t.Error("New() with no route reader = nil error, want a refusal")
	}
}

func TestProbesStayInsideTheObservation(t *testing.T) {
	t.Parallel()
	prober := &scriptedProber{script: map[probe.Protocol][]probe.Result{
		probe.ProtocolICMP: {ok(probe.ProtocolICMP), ok(probe.ProtocolICMP)},
		probe.ProtocolTCP:  {ok(probe.ProtocolTCP), ok(probe.ProtocolTCP)},
		probe.ProtocolHTTP: {
			failed(probe.ProtocolHTTP, probe.FailureTimeout, "timed out"),
			failed(probe.ProtocolHTTP, probe.FailureTimeout, "timed out"),
		},
	}}
	engine := newInvestigator(t, prober, &scriptedRoutes{}, Limits{})
	given := observation(probe.ProtocolHTTP, 9104)

	if _, err := engine.Investigate(context.Background(), given); err != nil {
		t.Fatalf("Investigate() = %v, want nil", err)
	}
	prober.mu.Lock()
	defer prober.mu.Unlock()
	if len(prober.calls) == 0 {
		t.Fatal("no probes ran")
	}
	for _, request := range prober.calls {
		if request.Namespace != given.Namespace {
			t.Errorf("a probe ran in %q, want %q", request.Namespace, given.Namespace)
		}
		if request.Target != given.Target {
			t.Errorf("a probe aimed at %q, want %q", request.Target, given.Target)
		}
		if request.Experiment != given.Experiment+"/investigation" {
			t.Errorf("a probe belonged to %q, want it to be marked as an investigation of %q",
				request.Experiment, given.Experiment)
		}
	}

	for _, request := range prober.calls {
		if request.Protocol == probe.ProtocolICMP {
			if request.Name != "" || request.Path != "" || request.Port != 0 {
				t.Errorf("the echo probe carried %q %q %d, want no name, path or port",
					request.Name, request.Path, request.Port)
			}
		}
	}
}

func TestTheClampRefusesToOverclaim(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		give Report
		want Classification
	}{
		{
			name: "a conclusion stronger than its evidence is lowered",
			give: Report{Diagnosis: Confirmed, Hypotheses: []Hypothesis{
				{Confidence: Probable, Concludes: true, Evidence: []int{0}},
			}},
			want: Probable,
		},
		{
			name: "evidence that no hypothesis concludes from proves nothing",
			give: Report{Diagnosis: Confirmed, Hypotheses: []Hypothesis{
				{Confidence: Confirmed, Evidence: []int{0}},
				{Confidence: Confirmed, Evidence: []int{1}},
			}},
			want: Unknown,
		},
		{
			name: "a conclusion citing nothing is unknown however sure it felt",
			give: Report{Diagnosis: Confirmed, Hypotheses: []Hypothesis{
				{Confidence: Confirmed, Concludes: true},
			}},
			want: Unknown,
		},
		{
			name: "a report with no hypotheses at all is unknown",
			give: Report{Diagnosis: Confirmed},
			want: Unknown,
		},
		{
			name: "weaker alternatives in the list do not weaken the conclusion",
			give: Report{Diagnosis: Confirmed, Hypotheses: []Hypothesis{
				{Confidence: Confirmed, Concludes: true, Evidence: []int{0}},
				{Layer: layerNetwork, Confidence: Possible, RefutedBy: []int{1}},
			}},
			want: Confirmed,
		},
		{
			name: "a conclusion the evidence supports is left alone",
			give: Report{Diagnosis: Probable, Hypotheses: []Hypothesis{
				{Confidence: Probable, Concludes: true, Evidence: []int{0}},
			}},
			want: Probable,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			clamp(&test.give)
			if test.give.Diagnosis != test.want {
				t.Errorf("Diagnosis = %s, want %s", test.give.Diagnosis, test.want)
			}
		})
	}
}

func TestASummaryNeverDisagreesWithItsClassification(t *testing.T) {
	t.Parallel()
	report := Report{
		Diagnosis: Possible,
		Evidence:  []Evidence{{Layer: layerNetwork, Probed: true, Note: "no reply"}},
		Hypotheses: []Hypothesis{
			{Layer: layerNetwork, Confidence: Possible, Concludes: true, Evidence: []int{0}},
		},
	}
	body := "the target did not answer at the IP layer (evidence 0)."
	report.Summary = summarise(&report, body)

	if !strings.HasPrefix(report.Summary, string(report.Diagnosis)+":") {
		t.Errorf("Summary = %q, want it to open with %s", report.Summary, report.Diagnosis)
	}

	empty := Report{Diagnosis: Unknown}
	if got := summarise(&empty, ""); !strings.Contains(got, string(Unknown)) {
		t.Errorf("summarise() = %q, want it to say the evidence supports nothing", got)
	}
}

func hasWorking(report Report, layer string) bool {
	for _, item := range report.Evidence {
		if item.Layer == layer && item.Supports() {
			return true
		}
	}
	return false
}

func mentionsLayer(report Report, layer string) bool {
	for _, hypothesis := range report.Hypotheses {
		if hypothesis.Layer == layer {
			return true
		}
	}
	return false
}

func layersOf(report Report) []string {
	var out []string
	for _, hypothesis := range report.Hypotheses {
		out = append(out, hypothesis.Layer)
	}
	return out
}

func citedIndexes(summary string) []int {
	var out []int
	for _, part := range strings.Split(summary, ",") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, "evidence ") {
			continue
		}
		for _, field := range strings.Fields(strings.TrimPrefix(part, "evidence ")) {
			var index int
			if _, err := fmt.Sscanf(field, "%d", &index); err == nil {
				out = append(out, index)
			}
		}
	}
	return out
}
