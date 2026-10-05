package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBinaryExistsAndIsExecutable(t *testing.T) {
	info, err := os.Stat(binaryPath)
	if err != nil {
		t.Fatalf("Stat(%q) = %v, want nil", binaryPath, err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("binary mode %v is not executable", info.Mode())
	}
}

func TestVersionCommand(t *testing.T) {
	got := execNetforge(t, "", "version")
	if got.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", got.exitCode, got.stdout, got.stderr)
	}
	for _, want := range []string{"netforge", "go: go", "platform: linux/amd64"} {
		if !strings.Contains(got.stdout, want) {
			t.Fatalf("stdout %q is missing %q", got.stdout, want)
		}
	}
	if got.stderr != "" {
		t.Fatalf("stderr = %q, want empty", got.stderr)
	}
}

func TestHelpCommandExitsZero(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"help"}, {"config", "--help"}, {"limits", "show"}} {
		got := execNetforge(t, "", args...)
		if got.exitCode != 0 {
			t.Fatalf("netforge %v exit code = %d, want 0\nstderr: %s", args, got.exitCode, got.stderr)
		}
		if got.stdout == "" {
			t.Fatalf("netforge %v produced no output", args)
		}
	}
}

func TestExitCodes(t *testing.T) {
	tests := []struct {
		name  string
		stdin string
		args  []string
		want  int
	}{
		{name: "success", args: []string{"config", "validate", testdata("valid-full.yaml")}, want: 0},
		{name: "invalid config", args: []string{"config", "validate", testdata("unknown-field.yaml")}, want: 3},
		{name: "invalid argument", args: []string{"config", "validate", testdata("invalid-cidr.yaml")}, want: 2},
		{name: "limit exceeded", args: []string{"config", "validate", testdata("excessive-limits.yaml")}, want: 4},
		{name: "limit exceeded from stream", args: []string{"config", "validate", "-"}, want: 4, stdin: "name: big\n" + strings.Repeat("#\n", 1<<20)},
		{name: "missing file", args: []string{"config", "validate", "missing.yaml"}, want: 5},
		{name: "unreadable file", args: []string{"config", "validate", unreadableFile(t)}, want: 5},
		{name: "unknown command", args: []string{"teleport"}, want: 2},
		{name: "no command", args: nil, want: 2},
		{name: "unknown flag", args: []string{"--nope", "version"}, want: 2},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := execNetforge(t, tc.stdin, tc.args...)
			if got.exitCode != tc.want {
				t.Fatalf("netforge %v exit code = %d, want %d\nstdout: %s\nstderr: %s", tc.args, got.exitCode, tc.want, got.stdout, got.stderr)
			}
		})
	}
}

func unreadableFile(t *testing.T) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root, permission bits are not enforced")
	}

	path := filepath.Join(t.TempDir(), "unreadable.yaml")
	if err := os.WriteFile(path, []byte("name: locked\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("Chmod() = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatalf("Chmod(restore) = %v, want nil", err)
		}
	})
	return path
}

func TestConfigValidateAcceptsRepoExample(t *testing.T) {
	example := repoFile("examples", "simple-line", "experiment.yaml")
	if _, err := os.Stat(example); err != nil {
		t.Fatalf("the repository example is missing: %v", err)
	}

	got := execNetforge(t, "", "config", "validate", example)
	if got.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", got.exitCode, got.stderr)
	}
	if !strings.Contains(got.stdout, "configuration: simple-line") {
		t.Fatalf("stdout %q is missing the experiment name", got.stdout)
	}
}

func TestConfigValidateRejectsMalformedRepoExamples(t *testing.T) {
	invalid := repoFile("examples", "invalid")
	if _, err := os.Stat(invalid); err == nil {
		entries, err := os.ReadDir(invalid)
		if err != nil {
			t.Fatalf("ReadDir() = %v, want nil", err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			path := filepath.Join(invalid, entry.Name())
			got := execNetforge(t, "", "config", "validate", path)
			if got.exitCode == 0 {
				t.Fatalf("netforge accepted the invalid example %q", path)
			}
			if !strings.HasPrefix(got.stderr, "netforge: ") {
				t.Fatalf("stderr = %q, want a netforge error", got.stderr)
			}
		}
	}
}

func TestConfigShowIsStableAcrossProcesses(t *testing.T) {
	first := execNetforge(t, "", "config", "show", testdata("valid-full.yaml"))
	if first.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", first.exitCode, first.stderr)
	}
	second := execNetforge(t, "", "config", "show", testdata("valid-full.yaml"))
	if first.stdout != second.stdout {
		t.Fatalf("config show is not reproducible across processes:\n%s\n---\n%s", first.stdout, second.stdout)
	}
}

func TestConfigValidateExperimentIDStable(t *testing.T) {
	first := execNetforge(t, "", "config", "validate", testdata("valid-full.yaml"))
	second := execNetforge(t, "", "config", "validate", testdata("valid-full.yaml"))
	if first.stdout != second.stdout {
		t.Fatalf("experiment id is not reproducible:\n%s\n---\n%s", first.stdout, second.stdout)
	}
}

func TestConfigRoundTripThroughStdin(t *testing.T) {
	shown := execNetforge(t, "", "config", "show", testdata("valid-minimal.yaml"))
	if shown.exitCode != 0 {
		t.Fatalf("config show exit code = %d, want 0\nstderr: %s", shown.exitCode, shown.stderr)
	}

	validated := execNetforge(t, shown.stdout, "config", "validate", "-")
	if validated.exitCode != 0 {
		t.Fatalf("config validate exit code = %d, want 0\nstderr: %s", validated.exitCode, validated.stderr)
	}
}

func TestStructuredJSONLogs(t *testing.T) {
	got := execNetforge(t, "", "--log-format=json", "config", "validate", testdata("valid-minimal.yaml"))
	if got.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", got.exitCode, got.stderr)
	}
	if !strings.HasPrefix(strings.TrimSpace(got.stderr), "{") {
		t.Fatalf("stderr %q is not JSON", got.stderr)
	}
	for _, want := range []string{`"level":"info"`, `"event":"configuration validated"`, `"component":"config"`, `"experiment_id":"nf-`} {
		if !strings.Contains(got.stderr, want) {
			t.Fatalf("stderr %q is missing %q", got.stderr, want)
		}
	}
}

func TestLogLevelFiltering(t *testing.T) {
	verbose := execNetforge(t, "", "--log-level=debug", "config", "validate", testdata("valid-minimal.yaml"))
	if !strings.Contains(verbose.stderr, "command dispatch") {
		t.Fatalf("stderr %q is missing the debug record", verbose.stderr)
	}

	quiet := execNetforge(t, "", "--log-level=error", "config", "validate", testdata("valid-minimal.yaml"))
	if strings.Contains(quiet.stderr, "configuration validated") {
		t.Fatalf("stderr %q contains records below the configured level", quiet.stderr)
	}
}

func TestLogLevelFromEnv(t *testing.T) {
	verbose := execNetforgeEnv(t, "", map[string]string{"NETFORGE_LOG_LEVEL": "debug"},
		"config", "validate", testdata("valid-minimal.yaml"))
	if verbose.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", verbose.exitCode, verbose.stderr)
	}
	if !strings.Contains(verbose.stderr, "command dispatch") {
		t.Fatalf("stderr %q is missing the debug record from the environment", verbose.stderr)
	}

	quiet := execNetforgeEnv(t, "", map[string]string{"NETFORGE_LOG_LEVEL": "error"},
		"config", "validate", testdata("valid-minimal.yaml"))
	if quiet.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", quiet.exitCode)
	}
	if strings.Contains(quiet.stderr, "configuration validated") {
		t.Fatalf("stderr %q contains records below the configured level", quiet.stderr)
	}
}

func TestFlagOverridesEnv(t *testing.T) {
	got := execNetforgeEnv(t, "", map[string]string{"NETFORGE_LOG_FORMAT": "json"},
		"--log-format=text", "config", "validate", testdata("valid-minimal.yaml"))
	if got.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", got.exitCode, got.stderr)
	}
	if strings.HasPrefix(strings.TrimSpace(got.stderr), "{") {
		t.Fatalf("stderr %q is JSON, want text from the flag", got.stderr)
	}
	if !strings.Contains(got.stderr, "command dispatch") && !strings.Contains(got.stderr, "configuration validated") {
		t.Fatalf("stderr %q is empty", got.stderr)
	}
}

func TestLogFormatFromEnv(t *testing.T) {
	got := execNetforgeEnv(t, "", map[string]string{"NETFORGE_LOG_FORMAT": "json"},
		"config", "validate", testdata("valid-minimal.yaml"))
	if got.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", got.exitCode, got.stderr)
	}
	if !strings.HasPrefix(strings.TrimSpace(got.stderr), "{") {
		t.Fatalf("stderr %q is not JSON", got.stderr)
	}
}

func TestInvalidLogSettingFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		args []string
	}{
		{name: "level from flag", args: []string{"--log-level=verbose", "version"}},
		{name: "level from environment", env: map[string]string{"NETFORGE_LOG_LEVEL": "verbose"}, args: []string{"version"}},
		{name: "format from flag", args: []string{"--log-format=xml", "version"}},
		{name: "format from environment", env: map[string]string{"NETFORGE_LOG_FORMAT": "xml"}, args: []string{"version"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := execNetforgeEnv(t, "", tc.env, tc.args...)
			if got.exitCode != 2 {
				t.Fatalf("exit code = %d, want 2\nstderr: %s", got.exitCode, got.stderr)
			}
		})
	}
}

func TestConfigParsingHasNoSideEffects(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "side-effects")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}

	document := "name: side-effect-check\n\nscope:\n  network: 10.42.0.0/16\n  namespaces:\n    - client\n"
	path := filepath.Join(dir, "experiment.yaml")
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}

	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() = %v, want nil", err)
	}
	if len(before) != 2 {
		t.Fatalf("directory contains %d entries before validation, want 2", len(before))
	}

	for i := 0; i < 3; i++ {
		if got := execNetforge(t, "", "config", "validate", path); got.exitCode != 0 {
			t.Fatalf("exit code = %d, want 0\nstderr: %s", got.exitCode, got.stderr)
		}
	}

	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() = %v, want nil", err)
	}
	if len(after) != len(before) {
		t.Fatalf("directory contains %d entries after validation, want %d", len(after), len(before))
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatalf("ReadDir(target) = %v, want nil", err)
	}
	if len(entries) != 0 {
		t.Fatalf("configuration validation created %d files", len(entries))
	}
}

func TestRepeatedInvocationsAreIdempotent(t *testing.T) {
	document := "name: repeated\n\nscope:\n  network: 10.42.0.0/16\n  namespaces:\n    - client\n"
	path := writeFile(t, "experiment.yaml", document)

	var reference string
	for i := 0; i < 5; i++ {
		got := execNetforge(t, "", "config", "validate", path)
		if got.exitCode != 0 {
			t.Fatalf("run %d exit code = %d, want 0\nstderr: %s", i, got.exitCode, got.stderr)
		}
		if i == 0 {
			reference = got.stdout
			continue
		}
		if got.stdout != reference {
			t.Fatalf("run %d output changed:\n%s\n---\n%s", i, reference, got.stdout)
		}
	}
}

func TestUnprivilegedExecution(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Fatal("this suite has to run unprivileged, otherwise it proves nothing about a rootless run")
	}

	for _, args := range [][]string{{"version"}, {"help"}, {"limits", "show"}, {"config", "validate", testdata("valid-minimal.yaml")}} {
		got := execNetforge(t, "", args...)
		if got.exitCode != 0 {
			t.Fatalf("netforge %v exit code = %d, want 0 without privileges\nstderr: %s", args, got.exitCode, got.stderr)
		}
	}
}
