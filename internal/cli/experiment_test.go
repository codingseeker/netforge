package cli

import (
	"context"
	"strings"
	"testing"
)

// The experiment and recovery commands only read: they compile a document and
// print a plan, or print the closed set of actions. Nothing here creates a
// namespace or opens a socket, which is what makes it safe for a command whose
// job is to show what a document would do before anybody runs it.

func TestExperimentPlanPrintsTheLifecycle(t *testing.T) {
	result := run(t, context.Background(), "",
		"experiment", "plan",
		"--namespace", "client",
		"--target", "10.42.0.3",
		"--port", "7000",
		testdata(t, "valid-full.yaml"))
	if result.err != nil {
		t.Fatalf("Run(experiment plan) = %v, want nil", result.err)
	}
	// Every state of the lifecycle appears, in order, because the whole point of
	// the command is that a reader can check the plan against the document
	// before anything is created.
	want := []string{
		"name:",
		"experiment_id:",
		"scope:",
		"faults:",
		"recoveries:",
		"budget:",
		"setup",
		"baseline",
		"fault",
		"observe",
		"diagnose",
		"recover",
		"verify",
		"report",
		"cleanup",
	}
	position := 0
	for _, fragment := range want {
		index := strings.Index(result.stdout[position:], fragment)
		if index < 0 {
			t.Fatalf("stdout %q is missing %q after position %d", result.stdout, fragment, position)
		}
		position += index
	}
	if result.stderr != "" {
		t.Errorf("stderr = %q, want empty", result.stderr)
	}
}

func TestExperimentPlanNamesAnUnauthorizedRecovery(t *testing.T) {
	// Naming an action is not consent. Without -authorize the plan must not
	// compile, and the refusal must say why rather than quietly dropping the
	// action: a plan that silently omitted a recovery would be read as a plan
	// that had nothing to recover.
	result := run(t, context.Background(), "",
		"experiment", "plan",
		"--namespace", "client",
		"--target", "10.42.0.3",
		"--port", "7000",
		"-recover", "restart_service:service-a",
		"-why", "the operator confirmed the service was killed",
		testdata(t, "valid-full.yaml"))
	if result.err == nil {
		t.Fatalf("Run(experiment plan) = nil, want a refusal: stdout %q", result.stdout)
	}
	if !strings.Contains(result.err.Error(), "explicit authorization") {
		t.Errorf("the refusal %q does not say authorization was required", result.err)
	}
	if result.stdout != "" {
		t.Errorf("stdout = %q, want nothing printed for a refused plan", result.stdout)
	}
}

func TestExperimentPlanAcceptsAnAuthorizedRecovery(t *testing.T) {
	result := run(t, context.Background(), "",
		"experiment", "plan",
		"--namespace", "client",
		"--target", "10.42.0.3",
		"--port", "7000",
		"-recover", "restart_service:service-a",
		"-recover", "remove_fault:service-b",
		"-why", "the operator confirmed the service was killed",
		"-authorize",
		testdata(t, "valid-full.yaml"))
	if result.err != nil {
		t.Fatalf("Run(experiment plan) = %v, want nil", result.err)
	}
	if !strings.Contains(result.stdout, "recoveries: 2") {
		t.Errorf("stdout %q does not count two authorized recoveries", result.stdout)
	}
	if !strings.Contains(result.stdout, "restart_service service-a") {
		t.Errorf("stdout %q does not name the restart it would perform", result.stdout)
	}
	// The plan says which action runs at which step, so a reader can see that the
	// recoveries happen after the diagnosis and before the verification.
	diagnose := strings.Index(result.stdout, "diagnose")
	recover := strings.Index(result.stdout, "remove_fault service-b")
	verify := strings.Index(result.stdout, "verify")
	if diagnose < 0 || recover < 0 || verify < 0 {
		t.Fatalf("stdout %q is missing a lifecycle step", result.stdout)
	}
	if !(diagnose < recover && recover < verify) {
		t.Errorf("the recoveries are not between the diagnosis and the verification in %q", result.stdout)
	}
}

func TestExperimentPlanRefusesAnOutOfScopeRecovery(t *testing.T) {
	result := run(t, context.Background(), "",
		"experiment", "plan",
		"--namespace", "client",
		"--target", "10.42.0.3",
		"--port", "7000",
		"-recover", "restart_service:service-z",
		"-authorize",
		"-why", "not in the document",
		testdata(t, "valid-full.yaml"))
	if result.err == nil {
		t.Fatal("a namespace outside the document was accepted")
	}
	if !strings.Contains(result.err.Error(), "not in the scope") {
		t.Errorf("the refusal %q does not say the namespace is out of scope", result.err)
	}
}

func TestExperimentPlanRefusesEveryProblemAtOnce(t *testing.T) {
	// A document with two problems and an observation with two more: one run
	// should say all four, because fixing them one run at a time is four runs.
	result := run(t, context.Background(), "",
		"experiment", "plan",
		"--namespace", "service-z",
		"--target", "10.99.0.9",
		"-recover", "restart_service:service-y",
		"-authorize",
		"-why", "two problems here",
		testdata(t, "valid-full.yaml"))
	if result.err == nil {
		t.Fatal("a plan with four problems compiled")
	}
	message := result.err.Error()
	for _, want := range []string{
		"observation.namespace",
		"observation.port",
		"outside scope.network",
		"not in the scope",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("the refusal %q is missing %q", message, want)
		}
	}
}

func TestExperimentPlanRefusesAnUnknownProtocol(t *testing.T) {
	result := run(t, context.Background(), "",
		"experiment", "plan",
		"--namespace", "client",
		"--target", "10.42.0.3",
		"--port", "7000",
		"--protocol", "carrier-pigeon",
		testdata(t, "valid-full.yaml"))
	if result.err == nil {
		t.Fatal("an unknown protocol was accepted")
	}
	if !strings.Contains(result.err.Error(), "carrier-pigeon") {
		t.Errorf("the refusal %q does not name the protocol", result.err)
	}
}

func TestExperimentPlanNeedsAConfiguration(t *testing.T) {
	result := run(t, context.Background(), "", "experiment", "plan", "--namespace", "client", "--target", "10.42.0.3")
	if result.err == nil {
		t.Fatal("a plan with no document was compiled")
	}
	if !strings.Contains(result.err.Error(), "configuration path is required") {
		t.Errorf("the refusal %q does not ask for a document", result.err)
	}
}

func TestExperimentRefusesAnUnknownSubcommand(t *testing.T) {
	result := run(t, context.Background(), "", "experiment", "frobnicate", testdata(t, "valid-full.yaml"))
	if result.err == nil {
		t.Fatal("an unknown subcommand was accepted")
	}
	if !strings.Contains(result.err.Error(), "unknown subcommand") {
		t.Errorf("the refusal %q does not say the subcommand is unknown", result.err)
	}
}

// The run subcommand shares the plan flags, and like the plan, everything about
// a run is checked while it is compiled, before a namespace or a socket exists.
// The refusals below are all compile-time or option-time refusals, which is what
// makes them testable without a kernel: a bad run must be refused before it
// would take the laboratory apart.

func TestExperimentRunRefusesAnUnknownReportFormat(t *testing.T) {
	result := run(t, context.Background(), "",
		"experiment", "run", "--output", "yaml",
		"--namespace", "client",
		"--target", "10.42.0.3",
		"--port", "7000",
		testdata(t, "valid-full.yaml"))
	if result.err == nil {
		t.Fatal("an unknown report format was accepted")
	}
	if !strings.Contains(result.err.Error(), "text or json") {
		t.Errorf("the refusal %q does not name the formats that exist", result.err)
	}
}

func TestExperimentRunRefusesAnEmptyLaboratory(t *testing.T) {
	result := run(t, context.Background(), "",
		"experiment", "run", "--clients", "0", "--servers", "2",
		"--namespace", "client",
		"--target", "10.42.0.3",
		"--port", "7000",
		testdata(t, "valid-full.yaml"))
	if result.err == nil {
		t.Fatal("a laboratory with no clients was accepted")
	}
	if !strings.Contains(result.err.Error(), "-clients and -servers") {
		t.Errorf("the refusal %q does not say the laboratory was empty", result.err)
	}
}

func TestExperimentRunIsRefusedBeforeTheLaboratoryExists(t *testing.T) {
	// The observation's target is outside the network the document grants. If
	// the run created anything before compiling, this test would be running in
	// a place without root and would fail for the wrong reason; what the run
	// must refuse is the plan, before any of it exists.
	result := run(t, context.Background(), "",
		"experiment", "run",
		"--namespace", "client",
		"--target", "10.99.0.9",
		"--port", "7000",
		testdata(t, "valid-full.yaml"))
	if result.err == nil {
		t.Fatal("a run whose observation is out of scope was accepted")
	}
	if !strings.Contains(result.err.Error(), "outside scope.network") {
		t.Errorf("the refusal %q does not say the target is out of the granted scope", result.err)
	}
	if result.stdout != "" {
		t.Errorf("stdout = %q, want nothing printed for a refused run", result.stdout)
	}
}

func TestExperimentRunRefusesAnUnknownCategoryOfFlag(t *testing.T) {
	// The observation needs a namespace and a target before the laboratory is
	// sized, so an empty observation must be refused by the compile while the
	// plan would still be refused for the same reason.
	result := run(t, context.Background(), "",
		"experiment", "run",
		testdata(t, "valid-full.yaml"))
	if result.err == nil {
		t.Fatal("a run with no observation was accepted")
	}
	for _, want := range []string{
		"observation.namespace",
		"observation.target",
		"observation.port",
	} {
		if !strings.Contains(result.err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", result.err, want)
		}
	}
}

func TestRecoveryActionsPrintsTheClosedSet(t *testing.T) {
	result := run(t, context.Background(), "", "recovery", "actions")
	if result.err != nil {
		t.Fatalf("Run(recovery actions) = %v, want nil", result.err)
	}
	for _, action := range []string{
		"remove_fault",
		"restore_route",
		"restart_service",
		"remove_backend",
		"restore_backend",
		"drain_connection",
		"re_enable_backend",
	} {
		if !strings.Contains(result.stdout, action) {
			t.Errorf("stdout %q is missing the action %q", result.stdout, action)
		}
	}
	// The chain is stated where someone reading it can see it, because the chain
	// is the security property and not an implementation detail.
	if !strings.Contains(result.stdout, "explicit authorization") ||
		!strings.Contains(result.stdout, "verification by measurement") {
		t.Errorf("stdout %q does not state what a recovery requires", result.stdout)
	}
	// Removal and restart change what is serving, and are marked as such.
	if !strings.Contains(result.stdout, "destructive") {
		t.Errorf("stdout %q does not mark the destructive actions", result.stdout)
	}
}

func TestRecoveryRefusesExtraArguments(t *testing.T) {
	result := run(t, context.Background(), "", "recovery", "actions", "extra")
	if result.err == nil {
		t.Fatal("an extra argument was accepted")
	}
	if !strings.Contains(result.err.Error(), "unexpected argument") {
		t.Errorf("the refusal %q does not name the unexpected argument", result.err)
	}
}

func TestTheNewCommandsAppearInHelp(t *testing.T) {
	result := run(t, context.Background(), "", "help")
	if result.err != nil {
		t.Fatalf("Run(help) = %v, want nil", result.err)
	}
	for _, command := range []string{"experiment", "recovery"} {
		if !strings.Contains(result.stdout, command) {
			t.Errorf("the root help %q does not mention %q", result.stdout, command)
		}
	}
	for command, subcommand := range map[string]string{"experiment": "plan", "recovery": "actions"} {
		group := run(t, context.Background(), "", "help", command)
		if group.err != nil {
			t.Fatalf("Run(help %s) = %v, want nil", command, group.err)
		}
		if !strings.Contains(group.stdout, subcommand) {
			t.Errorf("the %s help %q does not list the %s subcommand", command, group.stdout, subcommand)
		}
	}
}
