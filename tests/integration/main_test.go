package integration

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/netforge/netforge/internal/namespace"
	"github.com/netforge/netforge/internal/probe"
	"github.com/netforge/netforge/internal/router"
	"github.com/netforge/netforge/internal/service"
)

var (
	repoRootPath string
	binaryPath   string
)

type runResult struct {
	exitCode int
	stdout   string
	stderr   string
}

func TestMain(m *testing.M) {
	if handled, code := namespace.Dispatch(); handled {
		os.Exit(code)
	}
	if handled, code := router.Dispatch(os.Args[1:]); handled {
		os.Exit(code)
	}
	if handled, code := service.Dispatch(os.Args[1:]); handled {
		os.Exit(code)
	}
	if handled, code := probe.Dispatch(os.Args[1:]); handled {
		os.Exit(code)
	}

	root, err := findRepoRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration: %v\n", err)
		os.Exit(1)
	}
	repoRootPath = root
	binaryPath = filepath.Join(root, "bin", "netforge")

	if !benchmarkChild() {
		if err := buildBinary(root); err != nil {
			fmt.Fprintf(os.Stderr, "integration: %v\n", err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

func benchmarkChild() bool {
	const child = "-test.run=^TestTransportBenchmarkChild$"
	for _, arg := range os.Args[1:] {
		if arg == child {
			return true
		}
	}
	return false
}

func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("the repository root holding go.mod was not found")
		}
		dir = parent
	}
}

func buildBinary(root string) error {
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
		return err
	}
	command := exec.Command("go", "build", "-o", binaryPath, "./cmd/netforge")
	command.Dir = root
	command.Env = append(os.Environ(), namespace.AgentEnv+"=")
	command.Stderr = os.Stderr
	return command.Run()
}

func execNetforge(t *testing.T, stdin string, args ...string) runResult {
	t.Helper()
	return execNetforgeEnv(t, stdin, nil, args...)
}

func execNetforgeEnv(t *testing.T, stdin string, env map[string]string, args ...string) runResult {
	t.Helper()
	command := exec.Command(binaryPath, args...)
	command.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	command.Env = os.Environ()
	command.Env = append(command.Env, namespace.AgentEnv+"=")
	for key, value := range env {
		command.Env = append(command.Env, key+"="+value)
	}
	err := command.Run()
	result := runResult{stdout: stdout.String(), stderr: stderr.String()}
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("running netforge %v: %v", args, err)
		}
		result.exitCode = exit.ExitCode()
	}
	return result
}

func testdata(name string) string {
	return filepath.Join(repoRootPath, "internal", "config", "testdata", name)
}

func repoFile(parts ...string) string {
	return filepath.Join(append([]string{repoRootPath}, parts...)...)
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}
	return path
}
