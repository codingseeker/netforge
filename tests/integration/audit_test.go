package integration

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/netforge/netforge/internal/namespace"
)

const (
	phase12LabID  = "nf-ph12"
	phase12Client = "nf-client0-" + phase12LabID
	phase12Server = "nf-server0-" + phase12LabID
	phase12Router = "nf-router-" + phase12LabID
	phase12Addr   = "10.42.0.6"
)

func phase12Document(t *testing.T, name string) string {
	t.Helper()
	return "name: " + name + "\n" +
		"scope:\n" +
		"  network: 10.42.0.0/24\n" +
		"  namespaces:\n" +
		"    - " + phase12Router + "\n" +
		"    - " + phase12Client + "\n" +
		"    - " + phase12Server + "\n" +
		"limits:\n" +
		"  max_namespaces: 8\n" +
		"  max_interfaces: 16\n" +
		"  max_duration: 5m\n" +
		"  max_bandwidth: 1Gbps\n" +
		"  max_packet_rate: 100000\n" +
		"  max_concurrent_probes: 64\n" +
		"faults:\n" +
		"  - target: " + phase12Server + "\n" +
		"    type: packet_loss\n" +
		"    value: 90%\n"
}

func aLongRun(t *testing.T, document string) (command *exec.Cmd, out, errOut *bytes.Buffer) {
	t.Helper()
	command = exec.Command(binaryPath,
		"experiment", "run",
		"--id", phase12LabID,
		"--namespace", phase12Client,
		"--target", phase12Addr,
		"--protocol", "icmp",
		"--count", "12",
		"--timeout", "300ms",
		"--deadline", "12s",
		"--after-fault", "300ms",
		"--output", "json",
		document)
	command.Stdin = strings.NewReader("")
	out = new(bytes.Buffer)
	errOut = new(bytes.Buffer)
	command.Stdout = out
	command.Stderr = errOut
	command.Env = append(os.Environ(), namespace.AgentEnv+"=")
	return command, out, errOut
}

func aQuickRun(t *testing.T, document string) runResult {
	t.Helper()
	return execNetforge(t, "",
		"experiment", "run",
		"--id", phase12LabID,
		"--namespace", phase12Client,
		"--target", phase12Addr,
		"--protocol", "icmp",
		"--count", "5",
		"--timeout", "300ms",
		"--deadline", "4s",
		"-recover", "remove_fault:"+phase12Server,
		"-authorize",
		"-why", "the audit's recovery removes the fault its own laboratory declared",
		"--output", "json",
		document)
}

func hostLeaks() (leaks []string) {
	for _, probe := range [][]string{
		{"ip", "-o", "link", "show"},
		{"ip", "-o", "route", "show"},
		{"ip", "netns", "list"},
		{"tc", "qdisc", "show"},
	} {
		out, err := exec.Command(probe[0], probe[1:]...).Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, phase12LabID) {
				leaks = append(leaks, strings.Join(probe, " ")+" => "+line)
			}
		}
	}
	return leaks
}

func interruptExperiment(t *testing.T, command *exec.Cmd, delay time.Duration, signal syscall.Signal) (exitCode int, stdout, stderr string) {
	t.Helper()
	if err := command.Start(); err != nil {
		t.Fatalf("start the interrupted run: %v", err)
	}
	time.Sleep(delay)
	if err := command.Process.Signal(signal); err != nil {
		_ = command.Wait()
		t.Fatalf("the run finished before the %s could land: %v", signal, err)
	}
	waitErr := command.Wait()
	if waitErr != nil {
		var exit *exec.ExitError
		if errors.As(waitErr, &exit) {
			exitCode = exit.ExitCode()
		}
	}
	if out, ok := command.Stdout.(*bytes.Buffer); ok {
		stdout = out.String()
	}
	if errOut, ok := command.Stderr.(*bytes.Buffer); ok {
		stderr = errOut.String()
	}
	return exitCode, stdout, stderr
}

func labProcesses() []string {
	var found []string
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	kinds := map[string]bool{
		"netforge-lab-agent":        true,
		"netforge-namespace-holder": true,
		"netforge-router":           true,
		"netforge-service":          true,
		"netforge-probe":            true,
	}
	for _, entry := range entries {
		pid := entry.Name()
		if _, err := strconv.Atoi(pid); err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", pid, "cmdline"))
		if err != nil {

			continue
		}
		for _, arg := range strings.Split(string(raw), "\x00") {
			if kinds[arg] {
				found = append(found, pid+" "+arg)
				break
			}
		}
	}
	sort.Strings(found)
	return found
}

func assertNoLaboratoryProcesses(t *testing.T, label string, before []string) {
	t.Helper()
	existing := make(map[string]bool, len(before))
	for _, entry := range before {
		existing[entry] = true
	}

	var leftover []string
	for attempt := 0; attempt < 50; attempt++ {
		leftover = nil
		for _, entry := range labProcesses() {
			if !existing[entry] {
				leftover = append(leftover, entry)
			}
		}
		if len(leftover) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("%s: %d laboratory processes outlived the run: %s",
		label, len(leftover), strings.Join(leftover, ", "))
}

func assertHostCameBack(t *testing.T, label string, before hostSnapshot) {
	t.Helper()
	after, err := hostState()
	if err != nil {
		t.Fatalf("%s: the host network state is not readable: %v", label, err)
	}
	if before.links != after.links || before.routes != after.routes || before.forwarding != after.forwarding {
		t.Errorf("%s: the host did not return to its prior state", label)
	}
	if leaks := hostLeaks(); len(leaks) > 0 {
		t.Errorf("%s: the laboratory left the host carrying its identity:\n%s", label, strings.Join(leaks, "\n"))
	}
}

func TestSIGINTInEveryPhaseLeavesTheLaboratoryClean(t *testing.T) {
	offsets := []struct {
		when  string
		after time.Duration
	}{
		{"setup", 150 * time.Millisecond},
		{"fault injection", 600 * time.Millisecond},
		{"recovery", 2500 * time.Millisecond},
		{"verification", 3800 * time.Millisecond},
	}
	document := writeFile(t, "phase12-sigint.yaml", phase12Document(t, "phase12-sigint"))
	beforeProcesses := labProcesses()

	for _, setting := range offsets {
		label := "SIGINT during " + setting.when
		before, err := hostState()
		if err != nil {
			t.Skipf("%s: the host network state is not readable: %v", label, err)
		}

		command, stdout, stderr := aLongRun(t, document)
		exitCode, _, _ := interruptExperiment(t, command, setting.after, syscall.SIGINT)
		if exitCode == 0 {
			t.Errorf("%s: the run completed cleanly instead of being interrupted", label)
		}
		_ = stdout
		_ = stderr

		again := aQuickRun(t, document)
		if again.exitCode != 0 || !strings.Contains(again.stdout, `"succeeded": true`) {
			t.Errorf("%s: a run after the interruption failed: exit %d\n%s",
				label, again.exitCode, strings.Join([]string{again.stderr, again.stdout}, "\n"))
		}
		assertHostCameBack(t, label, before)
	}
	assertNoLaboratoryProcesses(t, "after four interruptions", beforeProcesses)
}

func TestAKilledExperimentIsRecoveredByTheNextRun(t *testing.T) {
	document := writeFile(t, "phase12-kill.yaml", phase12Document(t, "phase12-kill"))
	before, err := hostState()
	if err != nil {
		t.Skipf("the host network state is not readable: %v", err)
	}
	beforeProcesses := labProcesses()

	for _, after := range []time.Duration{
		400 * time.Millisecond,
		1800 * time.Millisecond,
		3200 * time.Millisecond,
	} {
		command, _, _ := aLongRun(t, document)
		if err := command.Start(); err != nil {
			t.Fatalf("start the killed run: %v", err)
		}
		time.Sleep(after)
		if err := command.Process.Kill(); err != nil {
			_ = command.Wait()
			t.Fatalf("the run finished before the kill could land: %v", err)
		}
		_ = command.Wait()
	}

	again := aQuickRun(t, document)
	if again.exitCode != 0 || !strings.Contains(again.stdout, `"succeeded": true`) {
		t.Errorf("a run after three kills failed: exit %d\n%s",
			again.exitCode, strings.Join([]string{again.stderr, again.stdout}, "\n"))
	}
	assertHostCameBack(t, "after repeated kills", before)
	assertNoLaboratoryProcesses(t, "after three kills", beforeProcesses)
}

func TestRepeatedFailuresDoNotAccumulateState(t *testing.T) {
	document := writeFile(t, "phase12-repeat.yaml", phase12Document(t, "phase12-repeat"))
	before, err := hostState()
	if err != nil {
		t.Skipf("the host network state is not readable: %v", err)
	}
	beforeProcesses := labProcesses()

	for round := 0; round < 2; round++ {
		command, _, _ := aLongRun(t, document)
		interruptExperiment(t, command, 900*time.Millisecond, syscall.SIGINT)
		command, _, _ = aLongRun(t, document)
		if err := command.Start(); err != nil {
			t.Fatalf("start the killed run: %v", err)
		}
		time.Sleep(700 * time.Millisecond)
		if err := command.Process.Kill(); err != nil {
			_ = command.Wait()
			t.Fatalf("the run finished before the kill could land: %v", err)
		}
		_ = command.Wait()
	}
	assertHostCameBack(t, "after repeated rounds of interruption", before)
	assertNoLaboratoryProcesses(t, "after repeated rounds of interruption", beforeProcesses)
}
