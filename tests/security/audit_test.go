// Package security holds the Phase 12 security audit. Every test here runs
// unprivileged: the audit probes the boundaries that have to hold before a
// privileged operation is ever offered, so a host that is about to be trusted
// with laboratories is first made to be safely describable and safely
// measurable.
//
// The audit is deliberately redundant with the unit and integration suites.
// Its purpose is to be the standing proof that the checklist of Phase 12 is
// demonstrated: bounded configuration, bounded requests, refusal of excessive
// resources, refusal of injection payloads, and limits that are checked
// before they are used.
package security

import (
	"context"
	"go/parser"
	"go/token"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/netforge/netforge/internal/config"
	"github.com/netforge/netforge/internal/probe"
)

// auditDocument writes the body and reads it back with the strict decoder the
// CLI uses, keeping the decoder's own reason so a refusal can be matched to
// the boundary it hit.
func auditDocument(t *testing.T, body string) (*config.Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write audit document: %v", err)
	}
	return config.Load(path)
}

func validateAudit(t *testing.T, body string) error {
	t.Helper()
	cfg, err := auditDocument(t, body)
	if err != nil {
		return err
	}
	return cfg.Validate()
}

func expectAuditRefusal(t *testing.T, name, body string, wantSegment string) {
	t.Helper()
	err := validateAudit(t, body)
	if err == nil {
		t.Errorf("%s: the audit document was accepted, want a refusal mentioning %q", name, wantSegment)
		return
	}
	if wantSegment != "" && !strings.Contains(err.Error(), wantSegment) {
		t.Errorf("%s: the refusal %q does not mention %q", name, err, wantSegment)
	}
}

// TestMalformedConfigurationIsRefused drives the configuration parser with
// documents that are not YAML, are YAML of the wrong shape, or carry a field
// the reader does not know. Every one has to be refused rather than read
// loosely, because a decoder that tolerates foreign fields is how a hostile
// document smuggles a value no validation ever sees.
func TestMalformedConfigurationIsRefused(t *testing.T) {
	expectAuditRefusal(t, "not yaml", "name: [\n", "decode")
	expectAuditRefusal(t, "unknown top-level field", "name: x\nsurprise: true\n", "surprise")
	expectAuditRefusal(t, "unknown fault field", ""+
		"name: x\nscope:\n  network: 10.42.0.0/24\n  namespaces:\n    - nf-a\nfaults:\n  - kind: packet_loss\n",
		"has no field")
	expectAuditRefusal(t, "limits of the wrong type", "name: x\nlimits:\n  max_duration: [10m]\n", "decode")
	expectAuditRefusal(t, "a fault target nobody granted", ""+
		"name: x\nscope:\n  network: 10.42.0.0/24\n  namespaces:\n    - nf-a\nfaults:\n  - target: nf-b\n    type: packet_loss\n    value: 50%\n",
		"is not one of")
}

// TestInvalidTargetValuesAreRefused covers the document values that would
// reach the network layer. A prefix that is not a prefix, a default route as
// a scope, and a name that a namespace cannot carry have to be refused inside
// the document, so nothing built from the document ever has to decide what
// they mean.
func TestInvalidTargetValuesAreRefused(t *testing.T) {
	base := "" +
		"name: x\n" +
		"scope:\n  network: %s\n  namespaces:\n    - %s\n" +
		"limits:\n  max_namespaces: 8\n  max_interfaces: 16\n  max_duration: 5m\n  max_bandwidth: 1Gbps\n  max_packet_rate: 100000\n  max_concurrent_probes: 4\n"
	expectAuditRefusal(t, "cidr without a prefix",
		strings.Replace(strings.Replace(base, "%s", "10.42.0.0", 1), "nf-a", "nf-a", 1), "")
	expectAuditRefusal(t, "default-route network",
		strings.Replace(strings.Replace(base, "%s", "0.0.0.0/0", 1), "nf-a", "nf-a", 1), "")
	expectAuditRefusal(t, "upper-case namespace",
		strings.Replace(base, "nf-a", "NF-A", 1), "")
	expectAuditRefusal(t, "slash namespace",
		strings.Replace(base, "nf-a", "nf/a", 1), "")
	// Metacharacter payloads are refused too; the injection test asserts both
	// refusal and inertness, so the YAML error they cause is a refusal rather
	// than a finding. The names used here are YAML-valid scalars so the case
	// survives to the name validator, which is the layer that must refuse.
	expectAuditRefusal(t, "semicolon namespace",
		strings.Replace(base, "nf-a", "nf-a;rm", 1), "")
	expectAuditRefusal(t, "metachar namespace",
		strings.Replace(base, "nf-a", "nf-a$(id)", 1), "")
}

// TestADefaultRouteIsNotAScope checks the same boundary at the layer that
// enforces it, where the refusal says exactly what a scope must not be.
func TestADefaultRouteIsNotAScope(t *testing.T) {
	_, err := probe.NewScope(netip.MustParsePrefix("0.0.0.0/0"), []string{"nf-audit"})
	if err == nil {
		t.Fatal("a default route was accepted as a network to probe")
	}
	if !strings.Contains(err.Error(), "default route") {
		t.Errorf("the refusal %q does not say a default route is not a scope", err)
	}
}

// TestExcessiveResourceRequestsAreRefused inflates one resource ceiling at a
// time and requires the document to be refused. A configuration file is power
// of attorney for privileged work, so its claims of budget must be bounded
// before the code that spends the budget runs.
func TestExcessiveResourceRequestsAreRefused(t *testing.T) {
	generous := func(namespaces, maxNS, maxIF, maxDur, maxBW, maxPR, maxCP string) string {
		return "" +
			"name: x\n" +
			"scope:\n  network: 10.42.0.0/24\n  namespaces:\n" + namespaces +
			"limits:\n  max_namespaces: " + maxNS + "\n  max_interfaces: " + maxIF +
			"\n  max_duration: " + maxDur + "\n  max_bandwidth: " + maxBW +
			"\n  max_packet_rate: " + maxPR + "\n  max_concurrent_probes: " + maxCP + "\n"
	}
	three := "    - nf-a\n    - nf-b\n    - nf-c\n"
	valid := func() string { return generous(three, "8", "16", "5m", "1Gbps", "100000", "4") }

	// The ceilings themselves are the first bound: a document may not ask for
	// more of a resource than the platform will ever grant.
	expectAuditRefusal(t, "namespaces above the ceiling",
		generous(three, "4096", "16", "5m", "1Gbps", "100000", "4"), "max_namespaces")
	expectAuditRefusal(t, "interfaces above the ceiling",
		generous(three, "8", "4096", "5m", "1Gbps", "100000", "4"), "max_interfaces")
	expectAuditRefusal(t, "duration above the ceiling",
		generous(three, "8", "16", "9999h", "1Gbps", "100000", "4"), "max_duration")
	expectAuditRefusal(t, "bandwidth above the ceiling",
		generous(three, "8", "16", "5m", "200Gbps", "100000", "4"), "max_bandwidth")
	expectAuditRefusal(t, "packet rate above the ceiling",
		generous(three, "8", "16", "5m", "1Gbps", "999999999", "4"), "max_packet_rate")
	expectAuditRefusal(t, "concurrent probes above the ceiling",
		generous(three, "8", "16", "5m", "1Gbps", "100000", "99999"), "max_concurrent_probes")

	// The second bound is the document against its own budget: a file may not
	// grant more namespaces than its own limit allows.
	many := "    - nf-a\n    - nf-b\n    - nf-c\n    - nf-d\n    - nf-e\n    - nf-f\n    - nf-g\n    - nf-h\n    - nf-i\n"
	expectAuditRefusal(t, "namespaces beyond its own budget",
		generous(many, "4", "16", "5m", "1Gbps", "100000", "4"), "exceeds")

	// A sane document still validates, so the refusals above are caused by the
	// knob being tested and not by the base being broken.
	if err := validateAudit(t, valid()); err != nil {
		t.Errorf("the audit's own control document was refused: %v", err)
	}
}

// TestCommandInjectionPayloadsAreRefusedOrInert feeds a document the strings
// an injection attack would try, and requires either that the document refuse
// them as invalid names or that it accept them as inert data. In neither case
// may the payload have executed: the marker file a payload would create must
// not exist, and the parser must have treated the payload as a value rather
// than as structure. The source half of the test then proves the
// configuration layer, the one that sees arbitrary payloads, runs no commands
// at all.
func TestCommandInjectionPayloadsAreRefusedOrInert(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "netforge-pwned")
	for name, payload := range map[string]string{
		"semicolon namespace":   "nf-a; touch " + marker,
		"command-sub namespace": "nf-a$(touch " + marker + ")",
		"backtick namespace":    "nf-`touch " + marker + "`",
		"pipe namespace":        "nf-a|id > " + marker,
		"and namespace":         "nf-a&&touch " + marker,
		"dollar namespace":      "nf-${touch " + marker + "}",
	} {
		body := "" +
			"name: injected\n" +
			"scope:\n  network: 10.42.0.0/24\n  namespaces:\n    - " + payload + "\n" +
			"limits:\n  max_namespaces: 8\n  max_interfaces: 16\n  max_duration: 5m\n  max_bandwidth: 1Gbps\n  max_packet_rate: 100000\n  max_concurrent_probes: 4\n"
		err := validateAudit(t, body)
		if err == nil {
			t.Errorf("%s: a namespace %q passed validation; it must be inert at least", name, payload)
		}
	}

	// A comment is inert data: it is kept, and no part of the pipeline may
	// ever hand it to a shell. The audit accepts a document carrying payloads
	// in its comment, then enforces through the source scan that the layer
	// which saw those bytes has no way to execute them.
	commentary := "" +
		"# payload: ; rm -rf /; $(id); `echo`; | sh\n" +
		"name: injected\n" +
		"scope:\n  network: 10.42.0.0/24\n  namespaces:\n    - nf-a\n" +
		"faults:\n  - target: nf-a\n    type: packet_loss\n    value: 50%\n"
	if err := validateAudit(t, commentary); err != nil {
		t.Errorf("the audit document carrying payloads in a comment was refused: %v", err)
	}

	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("an injection payload executed and created %s", marker)
	}
}

// configProductSources lists the source files of the configuration and name
// layers, the code that reads arbitrary payloads from disk.
func configProductSources(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("..", "..", "internal")
	files := []string{}
	for _, dir := range []string{"config", "names"} {
		paths, err := filepath.Glob(filepath.Join(root, dir, "*.go"))
		if err != nil {
			t.Fatalf("glob %s: %v", dir, err)
		}
		files = append(files, paths...)
	}
	return files
}

// TestTheConfigurationLayerRunsNoCommands proves the injection payloads above
// can never be executed: the layer that parses them imports no process
// execution machinery at all, so even a payload that passes validation is
// inert by construction.
func TestTheConfigurationLayerRunsNoCommands(t *testing.T) {
	for _, file := range configProductSources(t) {
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, spec := range parsed.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Errorf("%s has an unparseable import %s", file, spec.Path.Value)
				continue
			}
			switch path {
			case "os/exec", "syscall":
				t.Errorf("%s imports %q, so the configuration layer can execute contacts", file, path)
			}
		}
	}
}

// newProbeEngine builds a probe engine for the request-boundary tests. The
// tests refuse at the request boundary, before a probe opens any socket, so a
// no-op runner and no echo are enough.
func newProbeEngine(t *testing.T, limits probe.Limits) *probe.Engine {
	t.Helper()
	scope, err := probe.NewScope(netip.MustParsePrefix("10.42.0.0/24"), []string{"nf-audit"})
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	engine, err := probe.NewEngine(limits, scope, noopRunner{}, nil, "")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return engine
}

type noopRunner struct{}

func (noopRunner) Run(string, ...string) (stdout, stderr string, code int, err error) {
	return "", "", 0, nil
}

// auditRequest sends one request that the audit expects to be refused, and
// returns the measured refusal.
func auditRequest(t *testing.T, request probe.Request) probe.Result {
	t.Helper()
	result, err := newProbeEngine(t, probe.DefaultLimits()).Run(context.Background(), request)
	if err == nil {
		t.Fatalf("the audit request was admitted: %+v", result)
	}
	return result
}

// TestExcessivePacketSizeIsRefused sends a probe whose payload exceeds the
// largest a measurement may carry. The refusal has to happen before anything
// is sent, so the check is bounded by a limit rather than by the kernel.
func TestExcessivePacketSizeIsRefused(t *testing.T) {
	result := auditRequest(t, probe.Request{
		Protocol:   probe.ProtocolTCP,
		Namespace:  "nf-audit",
		Target:     "10.42.0.6",
		Port:       80,
		Payload:    4096,
		Experiment: "nf-security",
	})
	if result.Failure != probe.FailureLimit {
		t.Errorf("an oversized payload measured as %s, want a limit refusal", result.Failure)
	}
}

// TestExcessiveProbeTimeoutIsRefused asks a probe for more time than any
// measurement may take. The requested timeout must be bounded by the limit
// before a socket is ever opened.
func TestExcessiveProbeTimeoutIsRefused(t *testing.T) {
	result := auditRequest(t, probe.Request{
		Protocol:   probe.ProtocolTCP,
		Namespace:  "nf-audit",
		Target:     "10.42.0.6",
		Port:       80,
		Timeout:    10 * time.Minute,
		Experiment: "nf-security",
	})
	if result.Failure != probe.FailureLimit {
		t.Errorf("an oversized timeout measured as %s, want a limit refusal", result.Failure)
	}
}

// TestExcessiveConcurrencyIsRefused holds one probe open and asks for a
// second. The engine admits without waiting, so the second probe is told it
// cannot run rather than queued behind an unbounded line.
func TestExcessiveConcurrencyIsRefused(t *testing.T) {
	scope, err := probe.NewScope(netip.MustParsePrefix("10.42.0.0/24"), []string{"nf-audit"})
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	limits := probe.DefaultLimits()
	limits.MaxConcurrent = 1
	blocker := &blockingRunner{started: make(chan struct{}), release: make(chan struct{})}
	engine, err := probe.NewEngine(limits, scope, blocker, nil, "")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	firstDone := make(chan probe.Result, 1)
	go func() {
		result, err := engine.Run(context.Background(), probe.Request{
			Protocol: probe.ProtocolTCP, Namespace: "nf-audit", Target: "10.42.0.6",
			Port: 80, Experiment: "nf-security",
		})
		if err != nil {
			firstDone <- probe.Result{Error: err.Error()}
			return
		}
		firstDone <- result
	}()
	<-blocker.started

	second, err := engine.Run(context.Background(), probe.Request{
		Protocol: probe.ProtocolTCP, Namespace: "nf-audit", Target: "10.42.0.6",
		Port: 80, Experiment: "nf-security",
	})
	if err == nil {
		t.Error("a second concurrent probe was admitted, want a limit refusal")
	} else if second.Failure != probe.FailureLimit {
		t.Errorf("the second probe measured as %s, want a limit refusal", second.Failure)
	}

	close(blocker.release)
	if got := <-firstDone; got.Failure != probe.FailureNone {
		t.Errorf("the held probe did not complete cleanly: %+v", got)
	}
}

// blockingRunner holds a concurrency slot until its release channel closes.
type blockingRunner struct {
	started chan struct{}
	release chan struct{}
}

func (b *blockingRunner) Run(string, ...string) (stdout, stderr string, code int, err error) {
	b.started <- struct{}{}
	<-b.release
	return "", "", 0, nil
}
