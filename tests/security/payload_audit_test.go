package security

import (
	"context"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/iface"
	"github.com/netforge/netforge/internal/probe"
)

// interfaceDocument is the audit's control document with a device name in it,
// so the interface boundary is exercised through the reader that sees untrusted
// bytes rather than only through the validator on its own.
func interfaceDocument(device string) string {
	return "name: audit\n" +
		"scope:\n  network: 10.42.0.0/24\n  namespaces:\n    - nf-audit\n" +
		"faults:\n" +
		"  - target: nf-audit\n" +
		"    type: packet_loss\n" +
		"    value: 50%\n" +
		"    interface: " + device + "\n"
}

// TestInvalidInterfaceNamesAreRefused refuses every device name the kernel
// would not accept, through the document reader as well as through the
// validator. A device name reaches `ip link` and `tc` as an argument of a
// privileged helper, so a name carrying a metacharacter is a name the helper
// would have to be trusted with. Refusing it before any laboratory exists is
// what keeps a document from ever reaching the point of running one.
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

	// The names a laboratory generates for itself stay valid, so the refusals
	// above are the validator working rather than a document shape that never
	// validates at all.
	for _, device := range []string{"nfr-s0", "nfc0", "eth0.100", "a_b", strings.Repeat("i", 15)} {
		if err := iface.ValidateName("audit", "interface", device); err != nil {
			t.Errorf("iface.ValidateName rejected %q: %v", device, err)
		}
	}
	if err := validateAudit(t, interfaceDocument("nfr-s0")); err != nil {
		t.Errorf("the audit's own control document with a device name was refused: %v", err)
	}
}

// TestHostileTimeoutValuesAreRefused covers the two ways a timeout can be made
// hostile rather than merely large. A negative timeout would put every deadline
// the request carries in the past, so it is refused as a request that cannot be
// read; a timeout past the limit would ask a measurement to outlast the
// experiment that requested it, so it is refused at the limit.
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

// TestACancelledProbeIsRefusedBeforeItRuns runs a probe whose context has
// already been cancelled. Cancellation has to end the wait where the request
// is admitted, not after the measurement has been made: a probe that ignores
// its context turns Ctrl-C into a hang, which is the same defect as an
// unbounded timeout seen from the other side. The runner counts its calls so a
// refusal that happened after the command ran could not pass for one that
// happened before it.
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

// TestAMeasurementCarriesItsDeadlineAndRefusesToOverrunIt checks both halves of
// the bound on one measurement. The deadline is handed to the helper as its own
// argument, so a helper that has stopped answering is given up on rather than
// waited for; and a measurement that comes back past the budget anyway is
// refused rather than recorded, because a measurement taken outside its budget
// describes a different experiment from the one that asked for it.
func TestAMeasurementCarriesItsDeadlineAndRefusesToOverrunIt(t *testing.T) {
	const budget = 300 * time.Millisecond
	limits := probe.DefaultLimits()
	limits.MaxDuration = budget

	// The measurement is given the budget to overrun rather than to keep:
	// `Measure` accepts a result that arrives late, so the delay has to clear
	// the budget by more than the second of slack the check allows before it
	// reports one.
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

	// The helper was told the deadline in its own argument vector rather than
	// left to be waited for, and it was told one that fits inside the budget.
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

// argumentValue returns the value that follows a flag in an argument vector.
func argumentValue(argv []string, flag string) string {
	for index, argument := range argv {
		if argument == flag && index+1 < len(argv) {
			return argv[index+1]
		}
	}
	return ""
}

// countingRunner records how many commands it was asked to run.
type countingRunner struct {
	calls int
}

func (r *countingRunner) Run(string, ...string) (stdout, stderr string, code int, err error) {
	r.calls++
	return "", "", 0, nil
}

// recordingRunner keeps the argument vector it was handed and answers after a
// delay, which is what a helper that has stopped responding looks like.
type recordingRunner struct {
	argv  []string
	delay time.Duration
}

func (r *recordingRunner) Run(_ string, argv ...string) (stdout, stderr string, code int, err error) {
	r.argv = argv
	time.Sleep(r.delay)
	return "", "", 0, nil
}
