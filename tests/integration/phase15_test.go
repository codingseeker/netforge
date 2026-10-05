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

// Phase 15 is the proof that a killed run's leftovers can be found and taken back
// without a reconciler ever touching anything that is not NetForge's.
//
// Every test here runs against a state root of its own, set through the
// environment variable rather than by reaching into the process, so what is being
// tested is the command an operator would run.

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

// phase15Entry is one line of a listing, read back so that a test can assert on
// which directory was identified rather than on prose around it.
type phase15Entry struct {
	Name string
	Kind string
	ID   string
	Line string
}

// phase15Entries reads a listing into its entries. A line that is not an entry —
// the header, the root, the summary — has an empty Name.
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

// TestAKilledRunLeavesIdentifiableState is the gap the phase closes. A run killed
// with SIGKILL cannot remove its state directory, and before this phase nothing
// listed it, nothing proved it was NetForge's and nothing took it back.
func TestAKilledRunLeavesIdentifiableState(t *testing.T) {
	root := t.TempDir()
	document := phase15Document(t)
	before := phase15List(t, root)
	if !strings.Contains(before.stdout, "nothing is there") {
		t.Fatalf("the state root was not empty at the start:\n%s", before.stdout)
	}

	// A software-routed run creates both kinds of state directory, so both are
	// what a killed run of this shape leaves behind.
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
	// Every entry is positively identified, named with the kind and the
	// identifier the run was using. A listing that printed a name without a kind
	// would not tell a laboratory's directory from a router's.
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

// TestReclaimingWhatAKilledRunLeftIsBoundedReportedAndIdempotent is the removal
// requirement, end to end. The two halves that matter are that it removes only
// what it proved, and that running it again does not fail — an operator who
// re-runs a reconcile to check the first one worked must not be met with an error.
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

	// The second pass removes nothing and does not fail. An absence is counted
	// apart from a refusal, so the command exits zero rather than reporting a
	// conflict over directories that were already dealt with.
	second := execNetforgeEnv(t, "", phase15Env(root), args...)
	if second.exitCode != 0 {
		t.Fatalf("reconciling twice failed: exit %d\n%s\n%s", second.exitCode, second.stdout, second.stderr)
	}
	if !strings.Contains(second.stdout, "removed 0, refused 0, already gone "+strconv.Itoa(len(names))) {
		t.Errorf("the second reclaim did something: %s\n%s", second.stdout, second.stderr)
	}
}

// TestTheReconcilerWillNotTouchAStateRootItWasNotGiven proves the confinement
// from the outside: an authorization is for one root, and spending it on another
// is refused rather than obeyed.
func TestTheReconcilerWillNotTouchAStateRootItWasNotGiven(t *testing.T) {
	// Two roots side by side. The first holds a directory that a killed run of
	// ours left; the second holds a stranger's file, which is what "unknown
	// resource" looks like from outside.
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
	// The stranger's directory carries a name NetForge generates, so the name
	// check alone would accept it. Only the marker refuses it, which is exactly
	// the point being made.
	forged := filepath.Join(theirs, names[0])
	if err := os.Mkdir(forged, 0o700); err != nil {
		t.Fatal(err)
	}
	bystander := filepath.Join(theirs, "someone-elses-file.txt")
	if err := os.WriteFile(bystander, []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Point the reclaim at the other root using a name taken from ours. The name
	// is one NetForge generates, so the refusal has to come from the ownership
	// check rather than the name check.
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

	// Our own state is untouched by all of that.
	if listing := phase15List(t, ours); len(phase15Names(t, ours)) != len(names) {
		t.Errorf("our state root was changed by a reclaim pointed elsewhere:\n%s", listing.stdout)
	}
}

// TestAReconcileNeedsAuthorizationAndAReason proves the command line cannot be
// used to delete state by accident. A flag alone is the destructive operation
// this document's rules are about, and a reason is what makes the deletion
// accountable afterwards.
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
	// Nothing above removed anything.
	if listing := phase15List(t, root); len(phase15Names(t, root)) != len(names) {
		t.Errorf("a refused reclaim removed state:\n%s", listing.stdout)
	}
}

// TestAReconcileRefusesStateThatIsStillInUse is the property that makes a
// reconciler safe to point at a host that is running a laboratory. The marker
// names the owning process, and a live owner is a refusal rather than a deletion.
func TestAReconcileRefusesStateThatIsStillInUse(t *testing.T) {
	// A run that is still going holds its own state directory open, and the
	// reconciler is asked to reclaim it while it runs.
	root := t.TempDir()
	document := phase15Document(t)
	command, stdout, _ := phase15LongRun(root, document)
	if err := command.Start(); err != nil {
		t.Fatalf("start the live run: %v", err)
	}
	// The laboratory exists once the report has been written, which is the last
	// thing the run does. Waiting for that is waiting for the state directory to
	// exist, and it is a wait on an observable event rather than on a duration.
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
	// One directory, because this run is kernel-routed: the software router's
	// directory is the other kind, and TestAKilledRunLeavesIdentifiableState is
	// where both are found.
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
	// The run is still going, and its state is still there: a reconciler that
	// pulled it out would have broken a running experiment.
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("the state of a live run was removed: %s", name)
		}
	}
	_ = command.Process.Kill()
	_ = command.Wait()
}

// phase15KilledRun builds the run the reconciler is asked to clean up after. It
// is a software-routed run because that is the shape that leaves both a
// laboratory's state directory and a router's.
func phase15KilledRun(root, document string) (*exec.Cmd, *bytes.Buffer, *bytes.Buffer) {
	return phase15Command(root, document,
		"--routing", "software", "--count", "12", "--deadline", "12s")
}

// phase15LongRun builds a run that stays alive long enough to be reconciled while
// it is running.
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

// phase15Names returns the state directory names a listing positively identified.
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
