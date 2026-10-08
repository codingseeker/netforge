package security

import (
	"context"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codingseeker/netforge/internal/apperr"
	"github.com/codingseeker/netforge/internal/iface"
	"github.com/codingseeker/netforge/internal/probe"
)

func interfaceDocument(device string) string {
	return "name: audit\n" +
		"scope:\n  network: 10.42.0.0/24\n  namespaces:\n    - nf-audit\n" +
		"faults:\n" +
		"  - target: nf-audit\n" +
		"    type: packet_loss\n" +
		"    value: 50%\n" +
		"    interface: " + device + "\n"
}

func TestInvalidInterfaceNamesAreRefused(t *testing.T) {
	refused := []struct {
		name   string
		device string
	}{
		{"empty", `""`},
		{"slash", `nfr-s0/dev`},
		{"space", `"nfr s0"`},
		{"semicolon", `nfr-s0;id`},
		{"command substitution", "nfr-s0$(id)"},
		{"backtick", "nfr-s0`id`"},
		{"pipe", "nfr-s0|tee"},
		{"redirect", "nfr-s0>out"},
		{"escaped newline", "\"nfr-s0\\nid\""},
		{"escaped tab", "\"nfr-s0\\tid\""},
		{"parent directory", ".."},
		{"single dot", "."},
		{"sixteen characters", "nfr-s01234567890"},
		{"leading dash", "-nfr-s0"},
		{"double dash", "--nfr-s0"},
		{"equals", "nfr-s0=1"},
	}
	for _, test := range refused {
		t.Run(test.name, func(t *testing.T) {
			if err := iface.ValidateName("audit", "interface", test.device); err == nil {
				t.Errorf("iface.ValidateName accepted %q", test.device)
			}
			if err := validateAudit(t, interfaceDocument(test.device)); err == nil {
				t.Errorf("a document naming the device %q was accepted", test.device)
			}
		})
	}

	for _, device := range []string{"nfr-s0", "nfc0", "eth0.100", "a_b", strings.Repeat("i", 15)} {
		if err := iface.ValidateName("audit", "interface", device); err != nil {
			t.Errorf("iface.ValidateName rejected %q: %v", device, err)
		}
	}
	if err := validateAudit(t, interfaceDocument("nfr-s0")); err != nil {
		t.Errorf("the audit's own control document with a device name was refused: %v", err)
	}
}

func TestHostileTimeoutValuesAreRefused(t *testing.T) {
	scope, err := probe.NewScope(netip.MustParsePrefix("10.42.0.0/24"), []string{"nf-audit"})
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	engine, err := probe.NewEngine(probe.DefaultLimits(), scope, noopRunner{}, nil, "")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	request := func(timeout time.Duration) probe.Request {
		return probe.Request{
			Protocol:   probe.ProtocolICMP,
			Namespace:  "nf-audit",
			Target:     "10.42.0.6",
			Experiment: "nf-security",
			Timeout:    timeout,
		}
	}

	for _, timeout := range []time.Duration{-time.Second, -time.Nanosecond} {
		_, runErr := engine.Run(context.Background(), request(timeout))
		if runErr == nil {
			t.Fatalf("the timeout %s was admitted", timeout)
		}
		if kind, ok := apperr.KindOf(runErr); !ok || kind != apperr.KindInvalidArgument {
			t.Errorf("the timeout %s was refused with kind %q, want an invalid argument: %v",
				timeout, kind, runErr)
		}
	}
	for _, timeout := range []time.Duration{time.Hour, 24 * time.Hour} {
		result := auditRequest(t, request(timeout))
		if result.Failure != probe.FailureLimit {
			t.Errorf("the timeout %s measured as %q, want a limit refusal", timeout, result.Failure)
		}
	}
}

func TestACancelledProbeIsRefusedBeforeItRuns(t *testing.T) {
	scope, err := probe.NewScope(netip.MustParsePrefix("10.42.0.0/24"), []string{"nf-audit"})
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	runner := &countingRunner{}
	engine, err := probe.NewEngine(probe.DefaultLimits(), scope, runner, nil, "")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	started := time.Now()
	result, runErr := engine.Run(ctx, probe.Request{
		Protocol:   probe.ProtocolICMP,
		Namespace:  "nf-audit",
		Target:     "10.42.0.6",
		Experiment: "nf-security",
		Timeout:    30 * time.Second,
	})
	elapsed := time.Since(started)

	if elapsed > 2*time.Second {
		t.Errorf("a cancelled probe took %s to give up on a %s timeout, want it to refuse at once",
			elapsed, 30*time.Second)
	}
	if runErr == nil {
		t.Errorf("a cancelled probe reported success: %+v", result)
	}
	if result.OK {
		t.Error("a cancelled probe reported OK")
	}
	if runner.calls != 0 {
		t.Errorf("a cancelled probe still ran its command %d times, want none", runner.calls)
	}
}

func TestAMeasurementCarriesItsDeadlineAndRefusesToOverrunIt(t *testing.T) {
	const budget = 300 * time.Millisecond
	limits := probe.DefaultLimits()
	limits.MaxDuration = budget

	runner := &recordingRunner{delay: budget + 1200*time.Millisecond}
	measurer, err := probe.New(limits, runner)
	if err != nil {
		t.Fatalf("probe.New: %v", err)
	}

	_, err = measurer.Measure(context.Background(), "nf-audit", "10.42.0.6", probe.Options{
		Count:    2,
		Interval: 10 * time.Millisecond,
		Size:     32,
		Timeout:  100 * time.Millisecond,
		Deadline: 200 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("a measurement that overran its budget was recorded as one")
	}
	if kind, ok := apperr.KindOf(err); !ok || kind != apperr.KindLimitExceeded {
		t.Errorf("the overrun was refused with kind %q, want a limit refusal: %v", kind, err)
	}
	if !strings.Contains(err.Error(), "budget") {
		t.Errorf("the refusal %q does not say the measurement overran its budget", err)
	}

	deadline := argumentValue(runner.argv, "-w")
	if deadline == "" {
		t.Fatalf("the helper was run without a deadline: %v", runner.argv)
	}
	seconds, convErr := strconv.Atoi(deadline)
	if convErr != nil {
		t.Fatalf("the deadline argument %q is not a number of seconds: %v", deadline, convErr)
	}
	if wanted := int(budget / time.Second); seconds > wanted && seconds > 1 {
		t.Errorf("the helper was given a %d second deadline, past the %s budget", seconds, budget)
	}
}

func argumentValue(argv []string, flag string) string {
	for index, argument := range argv {
		if argument == flag && index+1 < len(argv) {
			return argv[index+1]
		}
	}
	return ""
}

type countingRunner struct {
	calls int
}

func (r *countingRunner) Run(string, ...string) (stdout, stderr string, code int, err error) {
	r.calls++
	return "", "", 0, nil
}

type recordingRunner struct {
	argv  []string
	delay time.Duration
}

func (r *recordingRunner) Run(_ string, argv ...string) (stdout, stderr string, code int, err error) {
	r.argv = argv
	time.Sleep(r.delay)
	return "", "", 0, nil
}
