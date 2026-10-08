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

	"github.com/codingseeker/netforge/internal/config"
	"github.com/codingseeker/netforge/internal/probe"
)

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

	expectAuditRefusal(t, "semicolon namespace",
		strings.Replace(base, "nf-a", "nf-a;rm", 1), "")
	expectAuditRefusal(t, "metachar namespace",
		strings.Replace(base, "nf-a", "nf-a$(id)", 1), "")
}

func TestADefaultRouteIsNotAScope(t *testing.T) {
	_, err := probe.NewScope(netip.MustParsePrefix("0.0.0.0/0"), []string{"nf-audit"})
	if err == nil {
		t.Fatal("a default route was accepted as a network to probe")
	}
	if !strings.Contains(err.Error(), "default route") {
		t.Errorf("the refusal %q does not say a default route is not a scope", err)
	}
}

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

	many := "    - nf-a\n    - nf-b\n    - nf-c\n    - nf-d\n    - nf-e\n    - nf-f\n    - nf-g\n    - nf-h\n    - nf-i\n"
	expectAuditRefusal(t, "namespaces beyond its own budget",
		generous(many, "4", "16", "5m", "1Gbps", "100000", "4"), "exceeds")

	if err := validateAudit(t, valid()); err != nil {
		t.Errorf("the audit's own control document was refused: %v", err)
	}
}

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

func auditRequest(t *testing.T, request probe.Request) probe.Result {
	t.Helper()
	result, err := newProbeEngine(t, probe.DefaultLimits()).Run(context.Background(), request)
	if err == nil {
		t.Fatalf("the audit request was admitted: %+v", result)
	}
	return result
}

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

type blockingRunner struct {
	started chan struct{}
	release chan struct{}
}

func (b *blockingRunner) Run(string, ...string) (stdout, stderr string, code int, err error) {
	b.started <- struct{}{}
	<-b.release
	return "", "", 0, nil
}
