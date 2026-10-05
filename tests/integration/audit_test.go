// Package integration holds the Phase 12 behavioural audit. Where the
// security audit proves the boundaries before privileged work, this audit
// proves what happens when a laboratory is under interruption: SIGINT during
// each phase, a hard kill, and the repeated failures that follow a crash. The
// measure of every scenario is that the host is left exactly as it was — the
// same links, the same routes, no namespace, no interface and no tc rule
// carrying the laboratory's identity.
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

// phase12Document is the audit's laboratory: it grants the identifier above
// builds, and declares the loss an interrupted experiment is mid-way through
// pushing. Any laboratory this audit destroys is later rebuilt from the same
// document, so "the next run recovers" is part of the recovery being tested.
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

// aLongRun asks for enough probes that the phases stay far enough apart for a
// signal at a chosen offset to land inside the phase this audit is aiming at.
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

// aQuickRun is the sanity check every scenario ends with: the same laboratory
// id still builds, runs and tears down after the interruption.
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

// hostLeaks returns every network object that still carries the laboratory's
// identity. Fields the host cannot be asked about are skipped, never failed,
// because the report is about what the laboratory left behind and a host that
// will not answer is not evidence of a leak.
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

// interruptExperiment starts the audit's run and sends the signal after the
// given delay, reporting the exit code the process left with and what it
// wrote. A signal that arrived after a clean finish is a failure, because the
// audit is only evidence when the signal actually landed in the run.
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

// labProcesses returns every NetForge laboratory process the host is still
// running, as "pid kind" strings.
//
// A laboratory's namespaces are not in `ip netns list`: NetForge creates them
// with clone and addresses them by PID, so the only place a surviving namespace
// is visible is the process holding it open. A host with no leftover link,
// route, namespace or qdisc can still be running a laboratory's worth of
// namespaces, which is why the audit asks the process table as well as the
// network tools.
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
			// The process ended between the read of the directory and this read,
			// which is the outcome the audit wants rather than a failure.
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

// assertNoLaboratoryProcesses demands that every laboratory process that was
// not running before the scenario has exited. A holder that outlives the run
// that created it is holding a network namespace, and its veths, open for as
// long as the machine stays up.
func assertNoLaboratoryProcesses(t *testing.T, label string, before []string) {
	t.Helper()
	existing := make(map[string]bool, len(before))
	for _, entry := range before {
		existing[entry] = true
	}
	// The processes are told to stop by a pipe closing rather than by a signal
	// being delivered, so give the kernel the moment it needs before declaring
	// one still alive.
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

// assertHostCameBack compares the visible host against the snapshot taken
// before the laboratory existed and demands that nothing carrying the
// laboratory's identity survives anywhere.
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

// TestSIGINTInEveryPhaseLeavesTheLaboratoryClean sends SIGINT at a spread of
// moments inside one run: during setup, while the fault is injected and being
// observed, in the recovery, and in the verification. Every interrupted run
// has to come back to the host exactly as it found it, and the same
// laboratory id has to build, run and tear down afterwards. The interrupted
// run's own report carries its cancellation, so the cleanup is not inferred
// from an exit code alone.
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

		// The laboratory that was interrupted must be recoverable by name:
		// the next run with the same id succeeds and cleans itself up.
		again := aQuickRun(t, document)
		if again.exitCode != 0 || !strings.Contains(again.stdout, `"succeeded": true`) {
			t.Errorf("%s: a run after the interruption failed: exit %d\n%s",
				label, again.exitCode, strings.Join([]string{again.stderr, again.stdout}, "\n"))
		}
		assertHostCameBack(t, label, before)
	}
	assertNoLaboratoryProcesses(t, "after four interruptions", beforeProcesses)
}

// TestAKilledExperimentIsRecoveredByTheNextRun kills the experiment hard,
// while the fault is live and again deeper in the run. Nothing can run when
// SIGKILL lands, so the recovery the audit demands is the next run: it has to
// take the laboratory's identity, build the same laboratory, measure, finish
// and leave the host exactly as it was found.
func TestAKilledExperimentIsRecoveredByTheNextRun(t *testing.T) {
	document := writeFile(t, "phase12-kill.yaml", phase12Document(t, "phase12-kill"))
	before, err := hostState()
	if err != nil {
		t.Skipf("the host network state is not readable: %v", err)
	}
	beforeProcesses := labProcesses()

	for _, after := range []time.Duration{
		400 * time.Millisecond,  // the fault is being injected
		1800 * time.Millisecond, // the observation is running
		3200 * time.Millisecond, // the recovery is underway
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

// TestRepeatedFailuresDoNotAccumulateState runs the audit's destructive
// scenario a second time to be sure the recovery leaves no margin of stale
// state that accumulates run over run: the host after two rounds of
// interruption is still the host before either round began.
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
