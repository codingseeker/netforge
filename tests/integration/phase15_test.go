package integration

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/netforge/netforge/internal/namespace"
)

const (
	phase15LabID  = "nfp15"
	phase15Client = "nf-client0-" + phase15LabID
	phase15Server = "nf-server0-" + phase15LabID
	phase15Router = "nf-router-" + phase15LabID
	phase15IP     = "10.42.0.6"
)

func phase15Document(t *testing.T) string {
	t.Helper()
	return writeFile(t, "phase15.yaml", "name: phase15-reconcile\n"+
		"seed: 123456\n"+
		"scope:\n"+
		"  network: 10.42.0.0/24\n"+
		"  namespaces:\n"+
		"    - "+phase15Client+"\n"+
		"    - "+phase15Server+"\n"+
		"    - "+phase15Router+"\n"+
		"limits:\n"+
		"  max_namespaces: 8\n"+
		"  max_interfaces: 16\n"+
		"  max_duration: 5m\n"+
		"  max_bandwidth: 1Gbps\n"+
		"  max_packet_rate: 100000\n"+
		"  max_concurrent_probes: 64\n"+
		"faults:\n"+
		"  - target: "+phase15Server+"\n"+
		"    type: packet_loss\n"+
		"    value: 80%\n")
}

func phase15Env(root string) map[string]string {
	return map[string]string{"NETFORGE_STATE_ROOT": root}
}

func phase15List(t *testing.T, root string) runResult {
	t.Helper()
	result := execNetforgeEnv(t, "", phase15Env(root), "reconcile", "list")
	if result.exitCode != 0 {
		t.Fatalf("reconcile list failed: exit %d\n%s", result.exitCode, result.stderr)
	}
	return result
}

type phase15Entry struct {
	Name string
	Kind string
	ID   string
	Line string
}

func phase15Entries(listing string) []phase15Entry {
	var out []phase15Entry
	for _, line := range strings.Split(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		name := fields[0]
		if !strings.HasPrefix(name, "netforge-") {
			continue
		}
		entry := phase15Entry{Name: name, Line: line}
		if len(fields) > 1 {
			entry.Kind = fields[1]
		}
		if len(fields) > 2 {
			entry.ID = fields[2]
		}
		out = append(out, entry)
	}
	return out
}

func TestAKilledRunLeavesIdentifiableState(t *testing.T) {
	root := t.TempDir()
	document := phase15Document(t)
	before := phase15List(t, root)
	if !strings.Contains(before.stdout, "nothing is there") {
		t.Fatalf("the state root was not empty at the start:\n%s", before.stdout)
	}

	command, _, _ := phase15KilledRun(root, document)
	if err := command.Start(); err != nil {
		t.Fatalf("start the killed run: %v", err)
	}
	time.Sleep(1200 * time.Millisecond)
	if err := command.Process.Kill(); err != nil {
		_ = command.Wait()
		t.Fatalf("the run finished before the kill could land: %v", err)
	}
	_ = command.Wait()

	listing := phase15List(t, root)
	entries := phase15Entries(listing.stdout)
	if len(entries) == 0 {
		t.Fatalf("a killed run left nothing for the reconciler to find, or nothing was identified:\n%s", listing.stdout)
	}

	kinds := map[string]int{}
	for _, entry := range entries {
		if entry.Kind == "unidentified" {
			t.Errorf("a directory NetForge created was not identified: %q", entry.Line)
			continue
		}
		if entry.ID != phase15LabID {
			t.Errorf("the entry %q carries the identifier %q, want %q", entry.Name, entry.ID, phase15LabID)
		}
		kinds[entry.Kind]++
		if !strings.Contains(entry.Line, "idle") {
			t.Errorf("the entry %q is not reported as reclaimable: %q", entry.Name, entry.Line)
		}
	}
	if kinds["laboratory"] == 0 {
		t.Errorf("no laboratory state directory was found:\n%s", listing.stdout)
	}
	if kinds["router"] == 0 {
		t.Errorf("no router state directory was found:\n%s", listing.stdout)
	}
	if !strings.Contains(listing.stdout, "netforge reconcile reclaim") {
		t.Errorf("the listing does not say how to reclaim what it found:\n%s", listing.stdout)
	}
}

func TestReclaimingWhatAKilledRunLeftIsBoundedReportedAndIdempotent(t *testing.T) {
	root := t.TempDir()
	document := phase15Document(t)
	command, _, _ := phase15KilledRun(root, document)
	if err := command.Start(); err != nil {
		t.Fatalf("start the killed run: %v", err)
	}
	time.Sleep(1200 * time.Millisecond)
	if err := command.Process.Kill(); err != nil {
		_ = command.Wait()
		t.Fatalf("the run finished before the kill could land: %v", err)
	}
	_ = command.Wait()

	names := phase15Names(t, root)
	if len(names) == 0 {
		t.Fatal("the killed run left nothing to reclaim")
	}
	args := append([]string{
		"reconcile", "reclaim",
		"--authorize",
		"--why", "the run that owned this state was killed and the host is being cleared",
	}, nameArgs(names)...)

	first := execNetforgeEnv(t, "", phase15Env(root), args...)
	if first.exitCode != 0 {
		t.Fatalf("the reclaim failed: exit %d\n%s\n%s", first.exitCode, first.stdout, first.stderr)
	}
	if !strings.Contains(first.stdout, "removed "+strconv.Itoa(len(names))+", refused 0") {
		t.Errorf("the reclaim did not report removing what it was asked to:\n%s", first.stdout)
	}
	if !strings.Contains(first.stdout, "the run that owned this state was killed") {
		t.Errorf("the report does not carry the reason:\n%s", first.stdout)
	}
	after := phase15List(t, root)
	if !strings.Contains(after.stdout, "nothing is there") {
		t.Errorf("the state root still holds state after the reclaim:\n%s", after.stdout)
	}

	second := execNetforgeEnv(t, "", phase15Env(root), args...)
	if second.exitCode != 0 {
		t.Fatalf("reconciling twice failed: exit %d\n%s\n%s", second.exitCode, second.stdout, second.stderr)
	}
	if !strings.Contains(second.stdout, "removed 0, refused 0, already gone "+strconv.Itoa(len(names))) {
		t.Errorf("the second reclaim did something: %s\n%s", second.stdout, second.stderr)
	}
}

func TestTheReconcilerWillNotTouchAStateRootItWasNotGiven(t *testing.T) {

	ours := t.TempDir()
	theirs := t.TempDir()
	document := phase15Document(t)
	command, _, _ := phase15KilledRun(ours, document)
	if err := command.Start(); err != nil {
		t.Fatalf("start the killed run: %v", err)
	}
	time.Sleep(1200 * time.Millisecond)
	if err := command.Process.Kill(); err != nil {
		_ = command.Wait()
		t.Fatalf("the run finished before the kill could land: %v", err)
	}
	_ = command.Wait()

	names := phase15Names(t, ours)
	if len(names) == 0 {
		t.Fatal("the killed run left nothing to reclaim")
	}

	forged := filepath.Join(theirs, names[0])
	if err := os.Mkdir(forged, 0o700); err != nil {
		t.Fatal(err)
	}
	bystander := filepath.Join(theirs, "someone-elses-file.txt")
	if err := os.WriteFile(bystander, []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}

	result := execNetforgeEnv(t, "", phase15Env(theirs), "reconcile", "reclaim",
		"--id", names[0], "--authorize", "--why", "the name looked like one of ours")
	if result.exitCode == 0 {
		t.Fatalf("a directory with no marker was reclaimed:\n%s", result.stdout)
	}
	if !strings.Contains(result.stdout, "no ownership marker") &&
		!strings.Contains(result.stdout, "refused") {
		t.Errorf("the report does not say why it refused:\n%s", result.stdout)
	}
	if _, err := os.Stat(forged); err != nil {
		t.Error("a directory with no valid marker was removed")
	}
	if _, err := os.Stat(bystander); err != nil {
		t.Error("the reconciler removed a file that is not NetForge's")
	}

	if listing := phase15List(t, ours); len(phase15Names(t, ours)) != len(names) {
		t.Errorf("our state root was changed by a reclaim pointed elsewhere:\n%s", listing.stdout)
	}
}

func TestAReconcileNeedsAuthorizationAndAReason(t *testing.T) {
	root := t.TempDir()
	document := phase15Document(t)
	command, _, _ := phase15KilledRun(root, document)
	if err := command.Start(); err != nil {
		t.Fatalf("start the killed run: %v", err)
	}
	time.Sleep(1200 * time.Millisecond)
	if err := command.Process.Kill(); err != nil {
		_ = command.Wait()
		t.Fatalf("the run finished before the kill could land: %v", err)
	}
	_ = command.Wait()
	names := phase15Names(t, root)
	if len(names) == 0 {
		t.Fatal("the killed run left nothing to reclaim")
	}

	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "no authorization",
			args: []string{"--id", names[0], "--why", "because"},
			want: "explicit authorization",
		},
		{
			name: "no reason",
			args: []string{"--id", names[0], "--authorize"},
			want: "reason is required",
		},
		{
			name: "a name NetForge does not generate",
			args: []string{"--id", "..", "--authorize", "--why", "because"},
			want: "is not a name NetForge generates",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"reconcile", "reclaim"}, test.args...)
			result := execNetforgeEnv(t, "", phase15Env(root), args...)
			if result.exitCode == 0 {
				t.Fatalf("the reclaim was accepted:\n%s", result.stdout)
			}
			if !strings.Contains(result.stderr, test.want) {
				t.Errorf("the refusal does not say %q:\n%s", test.want, result.stderr)
			}
		})
	}

	if listing := phase15List(t, root); len(phase15Names(t, root)) != len(names) {
		t.Errorf("a refused reclaim removed state:\n%s", listing.stdout)
	}
}

func TestAReconcileRefusesStateThatIsStillInUse(t *testing.T) {

	root := t.TempDir()
	document := phase15Document(t)
	command, stdout, _ := phase15LongRun(root, document)
	if err := command.Start(); err != nil {
		t.Fatalf("start the live run: %v", err)
	}

	_ = stdout
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if len(phase15Names(t, root)) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	names := phase15Names(t, root)
	if len(names) == 0 {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Skip("the run did not create its state directory within the deadline")
	}

	t.Logf("reconciling %d state directories of a run that is still going", len(names))
	result := execNetforgeEnv(t, "", phase15Env(root),
		append([]string{"reconcile", "reclaim"},
			append(nameArgs(names),
				"--authorize", "--why", "the operator believes the run is over")...)...)
	if result.exitCode == 0 {
		t.Errorf("reclaiming the state of a live run reported success:\n%s", result.stdout)
	}
	if !strings.Contains(result.stdout, "still running") {
		t.Errorf("the report does not say the state is in use:\n%s", result.stdout)
	}

	for _, name := range names {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("the state of a live run was removed: %s", name)
		}
	}
	_ = command.Process.Kill()
	_ = command.Wait()
}

func phase15KilledRun(root, document string) (*exec.Cmd, *bytes.Buffer, *bytes.Buffer) {
	return phase15Command(root, document,
		"--routing", "software", "--count", "12", "--deadline", "12s")
}

func phase15LongRun(root, document string) (*exec.Cmd, *bytes.Buffer, *bytes.Buffer) {
	return phase15Command(root, document,
		"--count", "40", "--deadline", "30s", "--timeout", "500ms")
}

func phase15Command(root, document string, extra ...string) (*exec.Cmd, *bytes.Buffer, *bytes.Buffer) {
	args := append([]string{
		"experiment", "run",
		"--id", phase15LabID,
		"--namespace", phase15Client,
		"--target", phase15IP,
		"--protocol", "icmp",
		"--after-fault", "300ms",
		"-recover", "remove_fault:" + phase15Server,
		"-authorize",
		"-why", "the run removes the fault its own document declared",
	}, extra...)
	command := exec.Command(binaryPath, append(args, document)...)
	command.Stdin = strings.NewReader("")
	stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
	command.Stdout, command.Stderr = stdout, stderr
	command.Env = append(os.Environ(), namespace.AgentEnv+"=")
	command.Env = append(command.Env, "NETFORGE_STATE_ROOT="+root)
	return command, stdout, stderr
}

func phase15Names(t *testing.T, root string) []string {
	t.Helper()
	var names []string
	for _, entry := range phase15Entries(phase15List(t, root).stdout) {
		if entry.Kind == "unidentified" {
			continue
		}
		names = append(names, entry.Name)
	}
	return names
}

func nameArgs(names []string) []string {
	out := make([]string, 0, len(names)*2)
	for _, name := range names {
		out = append(out, "--id", name)
	}
	return out
}
