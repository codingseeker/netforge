package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/codingseeker/netforge/internal/apperr"
)

func TestAppAppliesEnvDefaults(t *testing.T) {
	var stdout, stderr bytes.Buffer
	app := &App{Env: Env{Args: []string{"version"}, Stdout: &stdout, Stderr: &stderr}}

	if err := app.Run(nil); err != nil {
		t.Fatalf("Run(version) = %v, want nil", err)
	}
	if !strings.HasPrefix(stdout.String(), "netforge ") {
		t.Fatalf("stdout = %q, want the default program name", stdout.String())
	}
	if !strings.Contains(stdout.String(), "commit: ") {
		t.Fatalf("stdout = %q, want a commit line", stdout.String())
	}
}

func TestAppWithoutStreamsDoesNotPanic(t *testing.T) {
	app := &App{Env: Env{Args: []string{"limits", "show"}}}

	if err := app.Run(context.Background()); err != nil {
		t.Fatalf("Run(limits show) = %v, want nil", err)
	}
}

func TestAppTreatsMissingStdinAsEmpty(t *testing.T) {
	app := &App{Env: Env{Args: []string{"config", "validate", "-"}}}

	err := app.Run(context.Background())
	if !apperr.IsKind(err, apperr.KindInvalidConfig) {
		t.Fatalf("Run(config validate -) = %v, want invalid_config", err)
	}
}

func TestAppReadsLogSettingsFromEnv(t *testing.T) {
	var stdout, stderr bytes.Buffer
	app := &App{Env: Env{
		Args:   []string{"--log-format", "json", "config", "validate", testdata(t, "valid-minimal.yaml")},
		Stdout: &stdout,
		Stderr: &stderr,
		Getenv: func(name string) string {
			if name == "NETFORGE_LOG_LEVEL" {
				return "debug"
			}
			return ""
		},
	}}

	if err := app.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want nil\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), `"event":"dispatch"`) {
		t.Fatalf("stderr = %q, want the debug dispatch event", stderr.String())
	}
}

func TestFlagOverridesEnv(t *testing.T) {
	var stdout, stderr bytes.Buffer
	app := &App{Env: Env{
		Args:   []string{"--log-format=text", "config", "validate", testdata(t, "valid-minimal.yaml")},
		Stdout: &stdout,
		Stderr: &stderr,
		Getenv: func(name string) string {
			if name == "NETFORGE_LOG_FORMAT" {
				return "json"
			}
			return ""
		},
	}}

	if err := app.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if strings.HasPrefix(strings.TrimSpace(stderr.String()), "{") {
		t.Fatalf("stderr = %q, want the text format from the flag", stderr.String())
	}
	if !strings.Contains(stderr.String(), "configuration validated") {
		t.Fatalf("stderr = %q, want the info record", stderr.String())
	}
}

func TestAppRejectsInvalidEnvLogLevel(t *testing.T) {
	var stdout, stderr bytes.Buffer
	app := &App{Env: Env{
		Args:   []string{"version"},
		Stdout: &stdout,
		Stderr: &stderr,
		Getenv: func(name string) string {
			if name == "NETFORGE_LOG_LEVEL" {
				return "verbose"
			}
			return ""
		},
	}}

	if err := app.Run(context.Background()); !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("Run(version) = %v, want invalid_argument", err)
	}
}

func TestAppPrintsHelpCommandFlagRequests(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "root dash h", args: []string{"-h"}, want: "Usage:"},
		{name: "root long help", args: []string{"--help"}, want: "Commands:"},
		{name: "command dash h", args: []string{"config", "-h"}, want: "config"},
		{name: "subcommand dash h", args: []string{"config", "validate", "-h"}, want: "--show-effective"},
		{name: "subcommand long help", args: []string{"limits", "show", "--help"}, want: "limits show"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := run(t, context.Background(), "", tc.args...)
			if result.err != nil {
				t.Fatalf("Run(%v) = %v, want nil", tc.args, result.err)
			}
			if !strings.Contains(result.stdout, tc.want) {
				t.Fatalf("stdout = %q, want it to contain %q", result.stdout, tc.want)
			}
		})
	}
}

func TestAppRejectsInvalidCommandFlag(t *testing.T) {
	result := run(t, context.Background(), "", "config", "validate", "--strict", testdata(t, "valid-minimal.yaml"))
	if !apperr.IsKind(result.err, apperr.KindInvalidArgument) {
		t.Fatalf("Run(config validate --strict) = %v, want invalid_argument", result.err)
	}
	if !strings.Contains(result.err.Error(), "invalid flags for command") {
		t.Fatalf("err = %q, want an invalid flag message", result.err)
	}
}

func TestHelpCommandRejectsUnknownTargets(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "unknown command", args: []string{"help", "teleport"}, want: "unknown command"},
		{name: "command without subcommands", args: []string{"help", "version", "extra"}, want: "has no subcommands"},
		{name: "too many arguments", args: []string{"help", "config", "validate", "extra"}, want: "too many help arguments"},
		{name: "unknown subcommand", args: []string{"help", "config", "destroy"}, want: "unknown subcommand"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := run(t, context.Background(), "", tc.args...)
			if !apperr.IsKind(result.err, apperr.KindInvalidArgument) {
				t.Fatalf("Run(%v) = %v, want invalid_argument", tc.args, result.err)
			}
			if !strings.Contains(result.err.Error(), tc.want) {
				t.Fatalf("err = %q, want it to contain %q", result.err, tc.want)
			}
		})
	}
}

func TestConfigValidatePrintsEffectiveConfig(t *testing.T) {
	result := run(t, context.Background(), "", "config", "validate", "--show-effective", testdata(t, "valid-minimal.yaml"))
	if result.err != nil {
		t.Fatalf("Run() = %v, want nil\nstderr: %s", result.err, result.stderr)
	}
	if !strings.Contains(result.stdout, "effective configuration:") {
		t.Fatalf("stdout = %q, want the effective configuration", result.stdout)
	}
	if !strings.Contains(result.stdout, "max_duration: 10m") {
		t.Fatalf("stdout = %q, want the defaulted duration", result.stdout)
	}
}

func TestConfigShowPropagateLoadFailure(t *testing.T) {
	result := run(t, context.Background(), "", "config", "show", testdata(t, "unknown-field.yaml"))
	if !apperr.IsKind(result.err, apperr.KindInvalidConfig) {
		t.Fatalf("Run(config show invalid) = %v, want invalid_config", result.err)
	}
	if result.stdout != "" {
		t.Fatalf("stdout = %q, want empty", result.stdout)
	}
}

func TestConfigShowRequiresExactlyOnePath(t *testing.T) {
	for _, args := range [][]string{
		{"config", "show"},
		{"config", "show", "a.yaml", "b.yaml"},
	} {
		result := run(t, context.Background(), "", args...)
		if !apperr.IsKind(result.err, apperr.KindInvalidArgument) {
			t.Fatalf("Run(%v) = %v, want invalid_argument", args, result.err)
		}
	}
}

func TestConfigShowReportsWriteFailure(t *testing.T) {
	writeErr := errors.New("disk full")
	app := &App{Env: Env{
		Args:   []string{"config", "show", testdata(t, "valid-minimal.yaml")},
		Stdout: failingWriter{err: writeErr},
		Stderr: &bytes.Buffer{},
	}}

	err := app.Run(context.Background())
	if !apperr.IsKind(err, apperr.KindIO) {
		t.Fatalf("Run(config show) = %v, want io", err)
	}
	if !errors.Is(err, writeErr) {
		t.Fatalf("Run(config show) = %v, want it to wrap %v", err, writeErr)
	}
}

type failingWriter struct {
	err error
}

func (w failingWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func TestConfigRejectsMissingSubcommand(t *testing.T) {
	result := run(t, context.Background(), "", "config")
	if !apperr.IsKind(result.err, apperr.KindInvalidArgument) {
		t.Fatalf("Run(config) = %v, want invalid_argument", result.err)
	}
	if !strings.Contains(result.err.Error(), "missing subcommand") {
		t.Fatalf("err = %q, want a missing subcommand message", result.err)
	}
	if !strings.Contains(result.stderr, "Usage:") {
		t.Fatalf("stderr = %q, want the group help", result.stderr)
	}
}

func TestLimitsRejectsMissingAndUnknownSubCmds(t *testing.T) {
	for _, args := range [][]string{{"limits"}, {"limits", "raise"}} {
		result := run(t, context.Background(), "", args...)
		if !apperr.IsKind(result.err, apperr.KindInvalidArgument) {
			t.Fatalf("Run(%v) = %v, want invalid_argument", args, result.err)
		}
	}
}

func TestLimitsShowRejectsExtraArguments(t *testing.T) {
	result := run(t, context.Background(), "", "limits", "show", "extra")
	if !apperr.IsKind(result.err, apperr.KindInvalidArgument) {
		t.Fatalf("Run(limits show extra) = %v, want invalid_argument", result.err)
	}
}

func TestGroupHelpListsEverySubcommand(t *testing.T) {
	for _, args := range [][]string{{"help", "config"}, {"help", "limits"}} {
		result := run(t, context.Background(), "", args...)
		if result.err != nil {
			t.Fatalf("Run(%v) = %v, want nil", args, result.err)
		}
		if !strings.Contains(result.stdout, "Subcommands:") {
			t.Fatalf("stdout = %q, want a subcommand section", result.stdout)
		}
	}
}

func TestEnvDefaults(t *testing.T) {
	env := Env{}
	if got := env.program(); got != ProgramName {
		t.Fatalf("program() = %q, want %q", got, ProgramName)
	}
	if got := env.lookupEnv("NETFORGE_LOG_LEVEL"); got != "" {
		t.Fatalf("lookupEnv() = %q, want empty", got)
	}
	if env.stdin() == nil {
		t.Fatal("stdin() = nil, want a reader")
	}
	if env.stdout() == nil || env.stderr() == nil {
		t.Fatal("stdout()/stderr() = nil, want a writer")
	}
}

func TestFirstNonEmptyReturnsEmptyForNoValues(t *testing.T) {
	if got := firstNonEmpty("", "", ""); got != "" {
		t.Fatalf("firstNonEmpty() = %q, want empty", got)
	}
	if got := firstNonEmpty("", "warn", "info"); got != "warn" {
		t.Fatalf("firstNonEmpty() = %q, want %q", got, "warn")
	}
}

func TestFailIgnoresNilError(t *testing.T) {
	stderr := &bytes.Buffer{}
	if err := fail(Env{Stderr: stderr}, nil); err != nil {
		t.Fatalf("fail(nil) = %v, want nil", err)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestWriteVersionUsesAppVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	app := &App{
		Env:     Env{Args: []string{"version"}, Stdout: &stdout, Stderr: &stderr},
		Version: "1.2.3",
		Commit:  "abc123",
	}
	if err := app.Run(context.Background()); err != nil {
		t.Fatalf("Run(version) = %v, want nil", err)
	}
	if !strings.Contains(stdout.String(), "netforge 1.2.3") {
		t.Fatalf("stdout = %q, want the app version", stdout.String())
	}
	if !strings.Contains(stdout.String(), "commit: abc123") {
		t.Fatalf("stdout = %q, want the app commit", stdout.String())
	}
}

func TestNotifySignalsAppliesDefaults(t *testing.T) {

	watcher := NotifySignals(nil)
	defer watcher.Stop()

	ctx := watcher.Context()
	if ctx == nil {
		t.Fatal("NotifySignals() returned a nil context")
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("ctx.Err() = %v, want nil", err)
	}
	if watcher.Received() != nil {
		t.Fatalf("Received() = %v, want nil", watcher.Received())
	}
}

func TestNotifySignalsStopIsIdempotentWithoutSignal(t *testing.T) {
	watcher := NotifySignals(context.Background(), os.Interrupt)
	watcher.Stop()
	watcher.Stop()

	if err := watcher.Context().Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("ctx.Err() = %v, want context.Canceled", err)
	}
	if got := watcher.ExitCode(nil); got != ExitSuccess {
		t.Fatalf("ExitCode(nil) = %d, want %d", got, ExitSuccess)
	}
}
