package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/codingseeker/netforge/internal/experiment"
	"github.com/codingseeker/netforge/internal/investigator"
	"github.com/codingseeker/netforge/internal/probe"
	"github.com/codingseeker/netforge/internal/recovery"
	"github.com/codingseeker/netforge/internal/report"
)

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

	if !strings.Contains(result.stdout, "explicit authorization") ||
		!strings.Contains(result.stdout, "verification by measurement") {
		t.Errorf("stdout %q does not state what a recovery requires", result.stdout)
	}

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

func TestTheRenderedRecommendationIsAFlagTheCommandLineAccepts(t *testing.T) {
	diagnosed := investigator.Report{
		Diagnosis: investigator.Probable,
		Layer:     "network",
		Summary:   "the path to the target is impaired",
		Observation: investigator.Observation{
			Namespace: "service-a",
			Target:    "10.42.0.3",
		},
	}
	var finished experiment.Report
	finished.Name = "cascading-failure"
	finished.ExperimentID = "nf-cascading-failure-a5ed9ba6"
	finished.Diagnosis = diagnosed
	finished.Observed = []probe.Measurement{{Namespace: "service-a", Target: "10.42.0.3"}}
	finished.Recommendation = recovery.Propose(diagnosed)
	if finished.Recommendation.Action == "" {
		t.Fatal("the diagnosis proposes no action, so the test would prove nothing")
	}

	rendered := report.FromExperiment(finished, nil).Recommendation.Flag
	value, found := strings.CutPrefix(rendered, "-recover ")
	if !found {
		t.Fatalf("the recommendation %q is not a -recover flag", rendered)
	}

	result := run(t, context.Background(), "",
		"experiment", "plan",
		"--namespace", "service-a",
		"--target", "10.42.0.3",
		"--port", "7000",
		"-recover", value,
		"-authorize",
		"-why", "the report's own recommendation, followed to the letter",
		testdata(t, "valid-full.yaml"))
	if result.err != nil {
		t.Fatalf("the recommendation %q does not compile: %v", rendered, result.err)
	}
	if !strings.Contains(result.stdout, "recoveries: 1") {
		t.Errorf("stdout %q does not count the recommendation as one authorized recovery", result.stdout)
	}
	if !strings.Contains(result.stdout, finished.Recommendation.Action.String()+" service-a") {
		t.Errorf("stdout %q does not name the action the report recommended", result.stdout)
	}
}

func TestAReportWithNoRecommendationRendersNoFlag(t *testing.T) {
	var empty experiment.Report
	empty.Diagnosis = investigator.Report{Diagnosis: investigator.Unknown}
	empty.Recommendation = recovery.Propose(empty.Diagnosis)

	if flag := report.FromExperiment(empty, nil).Recommendation.Flag; flag != "" {
		t.Errorf("a run that diagnosed nothing still recommends %q", flag)
	}
}
