package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/netforge/netforge/internal/apperr"
)

type runResult struct {
	stdout string
	stderr string
	err    error
}

func run(t *testing.T, ctx context.Context, stdin string, args ...string) runResult {
	t.Helper()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := &App{
		Env: Env{
			Args:   args,
			Stdin:  strings.NewReader(stdin),
			Stdout: stdout,
			Stderr: stderr,
			Getenv: func(string) string { return "" },
		},
		Version: "0.0.0-test",
		Commit:  "testcommit",
	}
	err := app.Run(ctx)
	return runResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

func testdata(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("..", "config", "testdata", name)
}

func TestVersionCommand(t *testing.T) {
	result := run(t, context.Background(), "", "version")
	if result.err != nil {
		t.Fatalf("Run(version) = %v, want nil", result.err)
	}
	for _, want := range []string{"netforge 0.0.0-test", "commit: testcommit", "platform: linux/amd64", "go: go"} {
		if !strings.Contains(result.stdout, want) {
			t.Fatalf("stdout %q is missing %q", result.stdout, want)
		}
	}
	if result.stderr != "" {
		t.Fatalf("stderr = %q, want empty", result.stderr)
	}
}

func TestVersionFlag(t *testing.T) {
	result := run(t, context.Background(), "", "--version")
	if result.err != nil {
		t.Fatalf("Run(--version) = %v, want nil", result.err)
	}
	if !strings.Contains(result.stdout, "netforge 0.0.0-test") {
		t.Fatalf("stdout = %q, want the version", result.stdout)
	}
}

func TestVersionRejectsExtraArguments(t *testing.T) {
	result := run(t, context.Background(), "", "version", "extra")
	if !apperr.IsKind(result.err, apperr.KindInvalidArgument) {
		t.Fatalf("Run(version extra) = %v, want invalid_argument", result.err)
	}
	if got := ExitCode(result.err); got != ExitUsage {
		t.Fatalf("ExitCode = %d, want %d", got, ExitUsage)
	}
}

func TestHelpOutput(t *testing.T) {
	result := run(t, context.Background(), "", "help")
	if result.err != nil {
		t.Fatalf("Run(help) = %v, want nil", result.err)
	}
	for _, want := range []string{
		"Usage:",
		"netforge [global flags] <command> [arguments]",
		"config",
		"limits",
		"version",
		"--log-level",
		"--log-format",
	} {
		if !strings.Contains(result.stdout, want) {
			t.Fatalf("help output %q is missing %q", result.stdout, want)
		}
	}
}

func TestHelpForExperimentRunNamesEveryLaboratoryFlag(t *testing.T) {
	result := run(t, context.Background(), "", "help", "experiment", "run")
	if result.err != nil {
		t.Fatalf("Run(help experiment run) = %v, want nil", result.err)
	}
	for _, want := range []string{
		"--routing",
		"--service-ports",
		"--service-base-port",
		"--clients",
		"--servers",
		"--id",
		"--output",
	} {
		if !strings.Contains(result.stdout, want) {
			t.Errorf("the help for experiment run %q does not mention %q", result.stdout, want)
		}
	}

	for _, want := range []string{"kernel or software", "needs a listener"} {
		if !strings.Contains(result.stdout, want) {
			t.Errorf("the help for experiment run %q does not say %q", result.stdout, want)
		}
	}
}

func TestHelpForCommand(t *testing.T) {
	tests := []struct {
		args []string
		want []string
	}{
		{args: []string{"help", "config"}, want: []string{"validate", "show", "Subcommands:"}},
		{args: []string{"help", "config", "validate"}, want: []string{"--show-effective", "Usage:"}},

		{args: []string{"help", "experiment", "run"}, want: []string{"netforge experiment run -", "--authorize"}},
		{args: []string{"experiment", "run", "--help"}, want: []string{"netforge experiment run -"}},
		{args: []string{"help", "limits"}, want: []string{"limits show"}},
		{args: []string{"help", "version"}, want: []string{"netforge version"}},
		{args: []string{"config", "--help"}, want: []string{"Subcommands:"}},
		{args: []string{"config", "validate", "--help"}, want: []string{"--show-effective"}},
		{args: []string{"--help"}, want: []string{"Commands:"}},
	}

	for _, tc := range tests {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			result := run(t, context.Background(), "", tc.args...)
			if result.err != nil {
				t.Fatalf("Run(%v) = %v, want nil", tc.args, result.err)
			}
			for _, want := range tc.want {
				if !strings.Contains(result.stdout, want) {
					t.Fatalf("stdout %q is missing %q", result.stdout, want)
				}
			}
		})
	}
}

func TestNoArgumentsIsUsageError(t *testing.T) {
	result := run(t, context.Background(), "")
	if !apperr.IsKind(result.err, apperr.KindInvalidArgument) {
		t.Fatalf("Run() = %v, want invalid_argument", result.err)
	}
	if !strings.Contains(result.stderr, "no command given") {
		t.Fatalf("stderr = %q, want a usage message", result.stderr)
	}
	if got := ExitCode(result.err); got != ExitUsage {
		t.Fatalf("ExitCode = %d, want %d", got, ExitUsage)
	}
}

func TestUnknownCommand(t *testing.T) {
	result := run(t, context.Background(), "", "teleport")
	if !apperr.IsKind(result.err, apperr.KindInvalidArgument) {
		t.Fatalf("Run(teleport) = %v, want invalid_argument", result.err)
	}
	if !strings.Contains(result.stderr, "unknown command") {
		t.Fatalf("stderr = %q, want an unknown command message", result.stderr)
	}
}

func TestUnknownSubcommand(t *testing.T) {
	for _, args := range [][]string{{"config", "teleport"}, {"limits", "teleport"}, {"config"}} {
		result := run(t, context.Background(), "", args...)
		if !apperr.IsKind(result.err, apperr.KindInvalidArgument) {
			t.Fatalf("Run(%v) = %v, want invalid_argument", args, result.err)
		}
	}
}

func TestUnknownGlobalFlag(t *testing.T) {
	result := run(t, context.Background(), "", "--unknown-flag", "version")
	if !apperr.IsKind(result.err, apperr.KindInvalidArgument) {
		t.Fatalf("Run() = %v, want invalid_argument", result.err)
	}
	if !strings.Contains(result.stderr, "invalid global flags") {
		t.Fatalf("stderr = %q, want a flag error", result.stderr)
	}
	if got := ExitCode(result.err); got != ExitUsage {
		t.Fatalf("ExitCode = %d, want %d", got, ExitUsage)
	}
}

func TestInvalidLogLevel(t *testing.T) {
	result := run(t, context.Background(), "", "--log-level=verbose", "version")
	if !apperr.IsKind(result.err, apperr.KindInvalidArgument) {
		t.Fatalf("Run() = %v, want invalid_argument", result.err)
	}
	if !strings.Contains(result.stderr, "unsupported log level") {
		t.Fatalf("stderr = %q, want a level error", result.stderr)
	}
}

func TestInvalidLogFormat(t *testing.T) {
	result := run(t, context.Background(), "", "--log-format=xml", "version")
	if !apperr.IsKind(result.err, apperr.KindInvalidArgument) {
		t.Fatalf("Run() = %v, want invalid_argument", result.err)
	}
}

func TestConfigValidateAcceptsValidFile(t *testing.T) {
	result := run(t, context.Background(), "", "config", "validate", testdata(t, "valid-full.yaml"))
	if result.err != nil {
		t.Fatalf("Run(config validate) = %v, want nil", result.err)
	}
	for _, want := range []string{
		"configuration: cascading-failure",
		"scope.network: 10.42.0.0/16",
		"scope.namespaces: client,router,lb,service-a,service-b,service-c",
		"experiment_id: nf-cascading-failure-",
	} {
		if !strings.Contains(result.stdout, want) {
			t.Fatalf("stdout %q is missing %q", result.stdout, want)
		}
	}
	if got := ExitCode(result.err); got != ExitSuccess {
		t.Fatalf("ExitCode = %d, want %d", got, ExitSuccess)
	}
}

func TestConfigValidateIsDeterministic(t *testing.T) {
	first := run(t, context.Background(), "", "config", "validate", testdata(t, "valid-full.yaml"))
	for i := 0; i < 5; i++ {
		next := run(t, context.Background(), "", "config", "validate", testdata(t, "valid-full.yaml"))
		if next.stdout != first.stdout {
			t.Fatalf("config validate output changed between runs:\n%s\n---\n%s", first.stdout, next.stdout)
		}
	}
}

func TestConfigValidateEmitsStructuredLog(t *testing.T) {
	result := run(t, context.Background(), "", "--log-format=json", "config", "validate", testdata(t, "valid-minimal.yaml"))
	if result.err != nil {
		t.Fatalf("Run() = %v, want nil", result.err)
	}

	var record map[string]any
	line := firstJSONLine(t, result.stderr)
	if err := json.Unmarshal([]byte(line), &record); err != nil {
		t.Fatalf("Unmarshal(%q) = %v, want valid JSON", line, err)
	}
	for _, key := range []string{"timestamp", "level", "event", "component", "experiment_id"} {
		if _, ok := record[key]; !ok {
			t.Fatalf("log record %v is missing %q", record, key)
		}
	}
	if got := record["event"]; got != "configuration validated" {
		t.Fatalf("event = %v, want %q", got, "configuration validated")
	}
	if got := record["component"]; got != "config" {
		t.Fatalf("component = %v, want %q", got, "config")
	}
}

func TestConfigValidateRejectsInvalidFile(t *testing.T) {
	tests := []struct {
		file  string
		kind  apperr.Kind
		code  int
		terms []string
	}{
		{file: "unknown-field.yaml", kind: apperr.KindInvalidConfig, code: ExitConfig, terms: []string{"allow_host_network"}},
		{file: "malformed.yaml", kind: apperr.KindInvalidConfig, code: ExitConfig},
		{file: "excessive-limits.yaml", kind: apperr.KindLimitExceeded, code: ExitLimit, terms: []string{"max_namespaces"}},
		{file: "invalid-cidr.yaml", kind: apperr.KindInvalidArgument, code: ExitUsage, terms: []string{"CIDR"}},
		{file: "negative-duration.yaml", kind: apperr.KindInvalidArgument, code: ExitUsage, terms: []string{"negative"}},
	}

	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			result := run(t, context.Background(), "", "config", "validate", testdata(t, tc.file))
			if !apperr.IsKind(result.err, tc.kind) {
				t.Fatalf("Run() = %v, want kind %q", result.err, tc.kind)
			}
			if got := ExitCode(result.err); got != tc.code {
				t.Fatalf("ExitCode = %d, want %d", got, tc.code)
			}
			if result.stdout != "" {
				t.Fatalf("stdout = %q, want empty on failure", result.stdout)
			}
			if !strings.HasPrefix(result.stderr, "netforge: ") {
				t.Fatalf("stderr = %q, want a netforge error prefix", result.stderr)
			}
			for _, term := range tc.terms {
				if !strings.Contains(result.stderr, term) {
					t.Fatalf("stderr %q is missing %q", result.stderr, term)
				}
			}
		})
	}
}

func TestConfigValidateMissingFile(t *testing.T) {
	result := run(t, context.Background(), "", "config", "validate", "does-not-exist.yaml")
	if !apperr.IsKind(result.err, apperr.KindNotFound) {
		t.Fatalf("Run() = %v, want not_found", result.err)
	}
	if got := ExitCode(result.err); got != ExitIO {
		t.Fatalf("ExitCode = %d, want %d", got, ExitIO)
	}
}

func TestConfigValidateRequiresPath(t *testing.T) {
	result := run(t, context.Background(), "", "config", "validate")
	if !apperr.IsKind(result.err, apperr.KindInvalidArgument) {
		t.Fatalf("Run() = %v, want invalid_argument", result.err)
	}
	if !strings.Contains(result.stderr, "configuration path is required") {
		t.Fatalf("stderr = %q, want a path requirement", result.stderr)
	}
}

func TestConfigValidateRejectsMultiplePaths(t *testing.T) {
	result := run(t, context.Background(), "", "config", "validate", "a.yaml", "b.yaml")
	if !apperr.IsKind(result.err, apperr.KindInvalidArgument) {
		t.Fatalf("Run() = %v, want invalid_argument", result.err)
	}
}

func TestConfigValidateReadsStdin(t *testing.T) {
	document := "name: piped\n\nscope:\n  network: 10.42.0.0/16\n  namespaces:\n    - client\n"
	result := run(t, context.Background(), document, "config", "validate", "-")
	if result.err != nil {
		t.Fatalf("Run(config validate -) = %v, want nil", result.err)
	}
	if !strings.Contains(result.stdout, "configuration: piped") {
		t.Fatalf("stdout = %q, want the piped configuration", result.stdout)
	}
}

func TestConfigValidateRejectsMalformedStdin(t *testing.T) {
	result := run(t, context.Background(), "name: [unterminated\n", "config", "validate", "-")
	if !apperr.IsKind(result.err, apperr.KindInvalidConfig) {
		t.Fatalf("Run() = %v, want invalid_config", result.err)
	}
}

func TestConfigValidateRejectsOversizeStdin(t *testing.T) {
	oversized := "name: big\n" + strings.Repeat("#\n", 1<<20)
	result := run(t, context.Background(), oversized, "config", "validate", "-")
	if !apperr.IsKind(result.err, apperr.KindLimitExceeded) {
		t.Fatalf("Run() = %v, want limit_exceeded", result.err)
	}
	if got := ExitCode(result.err); got != ExitLimit {
		t.Fatalf("ExitCode = %d, want %d", got, ExitLimit)
	}
}

func TestConfigShowPrintsCanonicalForm(t *testing.T) {
	result := run(t, context.Background(), "", "config", "show", testdata(t, "valid-minimal.yaml"))
	if result.err != nil {
		t.Fatalf("Run(config show) = %v, want nil", result.err)
	}
	for _, want := range []string{"name: minimal-lab", "max_duration: 10m0s", "max_bandwidth: 1Gbps", "max_namespaces: 10"} {
		if !strings.Contains(result.stdout, want) {
			t.Fatalf("stdout %q is missing %q", result.stdout, want)
		}
	}
	if strings.Contains(result.stdout, "seed: null") {
		t.Fatalf("stdout %q renders an absent seed", result.stdout)
	}
}

func TestConfigShowIsReproducible(t *testing.T) {
	first := run(t, context.Background(), "", "config", "show", testdata(t, "valid-full.yaml"))
	for i := 0; i < 3; i++ {
		next := run(t, context.Background(), "", "config", "show", testdata(t, "valid-full.yaml"))
		if next.stdout != first.stdout {
			t.Fatalf("config show output changed between runs:\n%s\n---\n%s", first.stdout, next.stdout)
		}
	}
}

func TestConfigShowRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shown.yaml")

	result := run(t, context.Background(), "", "config", "show", testdata(t, "valid-full.yaml"))
	if result.err != nil {
		t.Fatalf("Run(config show) = %v, want nil", result.err)
	}
	if err := os.WriteFile(path, []byte(result.stdout), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}

	validated := run(t, context.Background(), "", "config", "validate", path)
	if validated.err != nil {
		t.Fatalf("Run(config validate) = %v, want nil\n%s", validated.err, result.stdout)
	}
}

func TestLimitsShow(t *testing.T) {
	result := run(t, context.Background(), "", "limits", "show")
	if result.err != nil {
		t.Fatalf("Run(limits show) = %v, want nil", result.err)
	}
	for _, want := range []string{
		"max_config_bytes: 1048576",
		"default.max_namespaces: 10",
		"default.max_interfaces: 30",
		"default.max_duration: 10m0s",
		"default.max_bandwidth: 1Gbps",
		"default.max_packet_rate: 100000",
		"default.max_concurrent_probes: 100",
		"ceiling.max_namespaces: 64",
		"ceiling.max_bandwidth: 100Gbps",
	} {
		if !strings.Contains(result.stdout, want) {
			t.Fatalf("stdout %q is missing %q", result.stdout, want)
		}
	}
}

func TestCancelReportedInterrupt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := run(t, ctx, "", "config", "validate", testdata(t, "valid-full.yaml"))
	if !apperr.IsKind(result.err, apperr.KindCanceled) {
		t.Fatalf("Run(canceled) = %v, want canceled", result.err)
	}
	if got := ExitCode(result.err); got != ExitInterrupted {
		t.Fatalf("ExitCode = %d, want %d", got, ExitInterrupted)
	}
	if result.stdout != "" {
		t.Fatalf("stdout = %q, want empty after cancellation", result.stdout)
	}
}

func TestCancelDuringRunStopsWork(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	started := make(chan struct{})
	release := make(chan struct{})
	command := &Command{
		Name:  "blocking",
		Usage: "netforge blocking",
		Run: func(ctx context.Context, session *Session, options any, args []string) error {
			close(started)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return apperr.FromContext(ctx, "blocking")
			}
		},
	}

	original := commandRegistry
	defer func() { commandRegistry = original }()
	commandRegistry = func() map[string]*Command { return map[string]*Command{"blocking": command} }

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := &App{Env: Env{
		Args:   []string{"blocking"},
		Stdout: stdout,
		Stderr: stderr,
		Getenv: func(string) string { return "" },
	}}

	done := make(chan error, 1)
	go func() {
		done <- app.Run(ctx)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("command did not start")
	}

	select {
	case err := <-done:
		if !apperr.IsKind(err, apperr.KindTimeout) {
			t.Fatalf("Run() = %v, want timeout", err)
		}
		if got := ExitCode(err); got != ExitTimeout {
			t.Fatalf("ExitCode = %d, want %d", got, ExitTimeout)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return after the context deadline")
	}
}

func TestWithCommandInjectsRegistry(t *testing.T) {
	original := commandRegistry
	defer func() { commandRegistry = original }()

	commandRegistry = func() map[string]*Command {
		return map[string]*Command{"injected": {
			Name:  "injected",
			Usage: "netforge injected",
			Run: func(ctx context.Context, session *Session, options any, args []string) error {
				return nil
			},
		}}
	}

	result := run(t, context.Background(), "", "injected")
	if result.err != nil {
		t.Fatalf("Run(injected) = %v, want nil", result.err)
	}
}

func TestNilContextIsAccepted(t *testing.T) {

	result := run(t, nil, "", "version")
	if result.err != nil {
		t.Fatalf("Run(nil context) = %v, want nil", result.err)
	}
}

func TestNilWritersAreTolerated(t *testing.T) {
	app := &App{Env: Env{Args: []string{"version"}, Getenv: func(string) string { return "" }}}
	if err := app.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
}

func firstJSONLine(t *testing.T, output string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "{") {
			return line
		}
	}
	t.Fatalf("output %q contains no JSON line", output)
	return ""
}
