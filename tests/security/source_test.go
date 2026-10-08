package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var neverAllowed = []string{
	`"sh"`,
	`"bash"`,
	`"zsh"`,
	`"dash"`,
	`"ksh"`,
	`"sh -c"`,
	`"bash -c"`,
	`syscall.Exec`,
	`syscall.ForkExec`,
	`os.StartProcess`,
	`/bin/sh`,
	`/bin/bash`,
	`/usr/bin/env`,
}

var processBoundary = map[string]map[string]bool{
	"internal/cli/exit.go":              {`"syscall"`: true},
	"internal/cli/signals.go":           {`"syscall"`: true, `"os/signal"`: true},
	"internal/loadbalancer/agent.go":    {`"syscall"`: true, `"os/signal"`: true},
	"internal/namespace/agent.go":       {`"syscall"`: true, `"os/signal"`: true, `"os/exec"`: true},
	"internal/namespace/lab.go":         {`"syscall"`: true, `"os/exec"`: true},
	"internal/namespace/setns_linux.go": {`"syscall"`: true},
	"internal/network/balancer.go":      {`"syscall"`: true},
	"internal/network/router.go":        {`"syscall"`: true},
	"internal/network/service.go":       {`"syscall"`: true},
	"internal/service/service.go":       {`"syscall"`: true, `"os/signal"`: true},
	"internal/router/forward.go":        {`"syscall"`: true},
	"internal/router/router.go":         {`"syscall"`: true, `"os/signal"`: true},
}

func TestNoShellExecution(t *testing.T) {
	for _, file := range productSources(t) {
		content := read(t, file)
		for _, pattern := range neverAllowed {
			if strings.Contains(content, pattern) {
				t.Errorf("%s contains the forbidden process execution construct %q", relative(t, file), pattern)
			}
		}
		if !strings.Contains(content, "exec.Command(") {
			continue
		}
		if !processBoundary[relative(t, file)][`"os/exec"`] {
			t.Errorf("%s runs helper commands outside the process boundary", relative(t, file))
		}
	}
}

func TestProcessExecutionIsIsolated(t *testing.T) {
	allowedImports := processBoundary

	for _, file := range productSources(t) {
		relative := relative(t, file)
		content := read(t, file)

		for _, imported := range []string{`"os/exec"`, `"syscall"`, `"os/signal"`, `"os/user"`} {
			if !strings.Contains(content, imported) {
				continue
			}
			if !allowedImports[relative][imported] {
				t.Errorf("%s imports %s; only the process boundary may", relative, imported)
			}
		}
	}

	for path, imports := range allowedImports {
		content := read(t, repoFile(t, path))
		for imported := range imports {
			if !strings.Contains(content, imported) {
				t.Errorf("%s no longer imports %s; update the boundary check", path, imported)
			}
		}
	}
}

func TestProcessBoundaryIsMinimal(t *testing.T) {
	used := map[string]bool{}
	for _, file := range productSources(t) {
		content := read(t, file)
		for _, imported := range []string{`"os/exec"`, `"syscall"`, `"os/signal"`, `"os/user"`} {
			if strings.Contains(content, imported) {
				used[relative(t, file)] = true
			}
		}
	}
	for path := range processBoundary {
		if _, ok := used[path]; !ok {
			t.Errorf("%s is listed in the process boundary but uses nothing from it", path)
		}
	}
	for path := range used {
		if _, ok := processBoundary[path]; !ok {
			t.Errorf("%s reaches the process boundary without being listed", path)
		}
	}
}

func TestOnlyTheNamespaceBoundaryRunsHelpers(t *testing.T) {
	for _, file := range productSources(t) {
		content := read(t, file)
		if !strings.Contains(content, `exec.Command(`) {
			continue
		}
		path := relative(t, file)
		if !strings.HasPrefix(path, "internal/namespace/") {
			t.Errorf("%s runs a helper command outside internal/namespace", path)
		}
		if strings.Contains(content, `"sh"`) || strings.Contains(content, `"bash"`) {
			t.Errorf("%s runs a shell", path)
		}
	}
}

func TestAgentRefusesToRunCommandsFromTheOutside(t *testing.T) {
	content := read(t, repoFile(t, "internal/namespace/agent.go"))
	if !strings.Contains(content, "stdin is not a pipe") {
		t.Error("the agent no longer refuses to run without a pipe on standard input")
	}
	for _, pair := range [][2]string{
		{"write", "/proc/sys/net/"},
	} {
		if !strings.Contains(content, pair[0]) || !strings.Contains(content, pair[1]) {
			t.Errorf("the agent no longer restricts %s to %s", pair[0], pair[1])
		}
	}
}

func TestIOReadsAreBounded(t *testing.T) {
	unbounded := []string{
		"io.ReadAll(",
		"io.Copy(",
	}

	for _, file := range productSources(t) {
		relative := relative(t, file)
		content := read(t, file)
		for _, pattern := range unbounded {
			if !strings.Contains(content, pattern) {
				continue
			}
			if strings.Contains(content, "io.LimitReader(") {
				continue
			}
			t.Errorf("%s reads with %s and no io.LimitReader bound", relative, pattern)
		}
	}
}

func TestNoPanicsInCore(t *testing.T) {
	forbidden := []string{
		"panic(",
		"log.Fatal",
		"log.Panic",
		"os.Exit(",
	}

	for _, file := range productSources(t) {
		relative := relative(t, file)
		if !strings.HasPrefix(relative, "internal/") {
			continue
		}
		content := read(t, file)
		for _, pattern := range forbidden {
			if strings.Contains(content, pattern) {
				t.Errorf("%s uses %q, which is reserved for the process entry point", relative, pattern)
			}
		}
	}
}

func TestSecretsAreNotConfigurable(t *testing.T) {
	forbidden := []string{
		"password",
		"secret",
		"token",
		"credential",
		"apikey",
		"api_key",
		"private_key",
		"authorization",
	}

	root := repoRoot(t)
	for _, file := range collect(t, filepath.Join(root, "internal", "config"), ".go") {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		content := strings.ToLower(read(t, file))
		for _, pattern := range forbidden {
			if strings.Contains(content, pattern) {
				t.Errorf("%s mentions the sensitive field %q; the configuration model must not carry secrets", relative(t, file), pattern)
			}
		}
	}
}

func TestNoEmbeddedNetworkTargets(t *testing.T) {
	forbidden := []string{
		"http://",
		"https://",
		"8.8.8.8",
		"1.1.1.1",
		"example.com",
	}

	for _, file := range productSources(t) {
		relative := relative(t, file)
		content := read(t, file)
		for _, pattern := range forbidden {
			if strings.Contains(content, pattern) {
				t.Errorf("%s contains the hard coded target %q", relative, pattern)
			}
		}
	}
}

func TestRequiredDocumentationExists(t *testing.T) {
	required := []string{
		"README.md",
		"docs/SECURITY.md",
		"docs/THREAT_MODEL.md",
		"docs/PROTOCOL.md",
		"docs/ARCHITECTURE.md",
		"Makefile",
		".github/workflows/ci.yml",
		"go.mod",
		"examples/simple-line/experiment.yaml",
	}

	root := repoRoot(t)
	for _, relative := range required {
		info, err := os.Stat(filepath.Join(root, relative))
		if err != nil {
			t.Errorf("required file %q is missing: %v", relative, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("required file %q is empty", relative)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() = %v, want nil", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found in any parent directory")
		}
		dir = parent
	}
}

func repoFile(t *testing.T, relative string) string {
	t.Helper()
	return filepath.Join(repoRoot(t), filepath.FromSlash(relative))
}

func productSources(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)
	var files []string
	for _, dir := range []string{"cmd", "internal"} {
		for _, file := range collect(t, filepath.Join(root, dir), ".go") {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			files = append(files, file)
		}
	}
	return files
}

func collect(t *testing.T, dir, suffix string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "testdata", "vendor", "bin", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(entry.Name(), suffix) {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("cannot walk %q: %v", dir, err)
	}
	if len(files) == 0 {
		t.Fatalf("no sources found in %q", dir)
	}
	return files
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) = %v, want nil", path, err)
	}
	return string(data)
}

func relative(t *testing.T, path string) string {
	t.Helper()
	root := repoRoot(t)
	trimmed, err := filepath.Rel(root, path)
	if err != nil {
		t.Fatalf("Rel(%q, %q) = %v, want nil", root, path, err)
	}
	return filepath.ToSlash(trimmed)
}
