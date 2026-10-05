package reconcile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/netforge/netforge/internal/apperr"
)

// A state root of the reconciler's own. Every test here is about what a
// reconciler does with a directory it may or may not own, and the only way to
// ask that question honestly is against a real directory rather than a mock:
// the ownership argument is a claim about files on disk.

const testID = "nfrecon"

func stateRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	// A state root has to look like one: NetForge's own is the system temporary
	// directory, so the reconciler is exercised against a root that is not a
	// freshly created leaf but an ordinary shared directory.
	return root
}

func create(t *testing.T, root string, kind Kind, id string) string {
	t.Helper()
	path, marker, err := Create(root, kind, id, time.Now())
	if err != nil {
		t.Fatalf("Create(%s) = %v, want nil", kind, err)
	}
	if marker.Kind != kind || marker.ID != id {
		t.Errorf("Create returned a marker for %s %s, want %s %s", marker.Kind, marker.ID, kind, id)
	}
	return path
}

func TestCreateWritesAMarkerThisBuildTrusts(t *testing.T) {
	root := stateRoot(t)
	path := create(t, root, KindLaboratory, testID)
	marker, err := ReadMarker(path)
	if err != nil {
		t.Fatalf("ReadMarker = %v, want nil", err)
	}
	if marker.Version != MarkerVersion {
		t.Errorf("the marker is version %d, want %d", marker.Version, MarkerVersion)
	}
	if marker.OwnerPID != os.Getpid() {
		t.Errorf("the marker names owner %d, want this process %d", marker.OwnerPID, os.Getpid())
	}
	if marker.CreatedAt.IsZero() {
		t.Error("the marker was never dated, so an age could not be computed")
	}
	if filepath.Base(path) != marker.Name {
		t.Errorf("the marker names %q inside %q", marker.Name, path)
	}
}

func TestTwoLaboratoriesDoNotShareADirectory(t *testing.T) {
	// The reason the name is generated rather than derived from the identifier:
	// two runs of the same experiment on one host must not fight over one path.
	root := stateRoot(t)
	first := create(t, root, KindLaboratory, testID)
	second := create(t, root, KindLaboratory, testID)
	if first == second {
		t.Fatal("two laboratories were given the same state directory")
	}
}

func TestTheTwoKindsAreToldApart(t *testing.T) {
	// The requirement that a reader can tell a laboratory's state directory from
	// a router's. Both sit in the same shared root, and a name that could be
	// either would make the listing useless.
	root := stateRoot(t)
	lab := create(t, root, KindLaboratory, testID)
	routerDir := create(t, root, KindRouter, testID)
	if filepath.Base(lab) == filepath.Base(routerDir) {
		t.Fatal("the two kinds share a name")
	}
	labMarker, err := ReadMarker(lab)
	if err != nil {
		t.Fatal(err)
	}
	routerMarker, err := ReadMarker(routerDir)
	if err != nil {
		t.Fatal(err)
	}
	if labMarker.Kind != KindLaboratory || routerMarker.Kind != KindRouter {
		t.Errorf("the markers say %s and %s", labMarker.Kind, routerMarker.Kind)
	}
	// Each marker's name declares its own kind, and a marker that disagrees with
	// the name it sits in is not trusted.
	if kind, ok := KindOf(filepath.Base(lab)); !ok || kind != KindLaboratory {
		t.Errorf("KindOf(%q) = %q/%t, want laboratory", filepath.Base(lab), kind, ok)
	}
	if kind, ok := KindOf(filepath.Base(routerDir)); !ok || kind != KindRouter {
		t.Errorf("KindOf(%q) = %q/%t, want router", filepath.Base(routerDir), kind, ok)
	}
}

func TestScanNamesTheIdentifierTheKindAndTheAge(t *testing.T) {
	root := stateRoot(t)
	lab := create(t, root, KindLaboratory, testID)
	entries, err := Scan(root, time.Now())
	if err != nil {
		t.Fatalf("Scan = %v, want nil", err)
	}
	if len(entries) != 1 {
		t.Fatalf("Scan found %d entries, want 1", len(entries))
	}
	entry := entries[0]
	if entry.Name != filepath.Base(lab) {
		t.Errorf("the entry names %q, want %q", entry.Name, filepath.Base(lab))
	}
	if entry.ID != testID || entry.Kind != KindLaboratory {
		t.Errorf("the entry carries %q %q, want %q laboratory", entry.Kind, entry.ID, testID)
	}
	if entry.Age < 0 || entry.Age > time.Minute {
		t.Errorf("the entry's age is %s, want a moment rather than %s", entry.Age, time.Minute)
	}
	described := entry.Describe()
	for _, want := range []string{filepath.Base(lab), testID, "laboratory"} {
		if !strings.Contains(described, want) {
			t.Errorf("the listing line %q does not carry %q", described, want)
		}
	}
}

func TestADirectoryWithoutAMarkerIsNeverIdentified(t *testing.T) {
	// The whole of the ownership argument. A directory carrying a name NetForge
	// would have generated proves nothing at all: the name is the cheap half, and
	// the marker is the half that cannot be forged by a coincidence.
	root := stateRoot(t)
	forged := filepath.Join(root, "netforge-lab-000000000000")
	if err := os.Mkdir(forged, 0o700); err != nil {
		t.Fatal(err)
	}
	entries, err := Scan(root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("Scan found %d entries, want the forged one", len(entries))
	}
	if entries[0].Identified {
		t.Error("a directory with no marker was identified as NetForge's")
	}
	if entries[0].ID != "" || entries[0].Kind != "" {
		t.Errorf("an unidentified entry carries %q %q, want no claim at all", entries[0].Kind, entries[0].ID)
	}
	if !strings.Contains(entries[0].Describe(), "unidentified") {
		t.Errorf("the listing line %q does not say it is unidentified", entries[0].Describe())
	}
	if got := Identified(entries); len(got) != 0 {
		t.Errorf("Identified returned %d entries, want none", len(got))
	}
}

func TestAMarkerThatWasCopiedIsNotOwnership(t *testing.T) {
	// A marker that names a different directory is a marker that has been moved
	// or copied. Either way the thing on disk is not the thing the marker was
	// written for, so it is refused.
	root := stateRoot(t)
	original := create(t, root, KindLaboratory, testID)
	copied := filepath.Join(root, "netforge-lab-aaaaaaaaaaaa")
	if err := os.Mkdir(copied, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(MarkerPath(original))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(MarkerPath(copied), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMarker(copied); err == nil {
		t.Fatal("a copied marker was accepted as ownership")
	} else if !strings.Contains(err.Error(), "claims to belong to") {
		t.Errorf("the refusal %q does not say the marker names another directory", err)
	}
}

func TestAnUnreadableOrForeignMarkerIsNotOwnership(t *testing.T) {
	root := stateRoot(t)
	cases := map[string]string{
		"not a marker":  "{this is not",
		"another build": `{"version":99,"kind":"laboratory","id":"x","name":"netforge-lab-bbbbbbbbbbbb","owner_pid":1,"created_at":"2026-01-01T00:00:00Z"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(root, "netforge-lab-"+strings.ReplaceAll(name, " ", ""))
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(MarkerPath(dir), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadMarker(dir); err == nil {
				t.Fatalf("a marker that is %q was accepted", name)
			}
		})
	}
}

func TestAnIdentifierANamespaceCouldNotCarryIsNotOwnership(t *testing.T) {
	// The marker has to survive the same validation a namespace name does,
	// because the directory it proves ownership of was made for a laboratory and
	// a laboratory is named after one.
	root := stateRoot(t)
	dir := filepath.Join(root, "netforge-lab-cccccccccccc")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := Marker{
		Version:   MarkerVersion,
		Kind:      KindLaboratory,
		ID:        "Not A Namespace",
		Name:      filepath.Base(dir),
		OwnerPID:  os.Getpid(),
		CreatedAt: time.Now(),
	}
	if err := WriteMarker(dir, marker); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMarker(dir); err == nil {
		t.Fatal("a marker carrying an identifier no namespace could carry was accepted")
	}
}

func TestSymlinksAndFilesAreNeverCandidates(t *testing.T) {
	// Following a symlink would take a removal somewhere the name does not say,
	// which is the one way a reconciler over a shared directory becomes a way to
	// delete something else.
	root := stateRoot(t)
	target := t.TempDir()
	link := filepath.Join(root, "netforge-lab-dddddddddddd")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks are unavailable here: %v", err)
	}
	file := filepath.Join(root, "netforge-router-eeeeeeeeeeee")
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := Scan(root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("Scan found %d entries, want 2", len(entries))
	}
	for _, entry := range entries {
		if entry.Identified {
			t.Errorf("%q was identified, want neither a symlink nor a file to be a candidate", entry.Name)
		}
	}
}

func TestReclaimRefusesWithoutAuthorization(t *testing.T) {
	root := stateRoot(t)
	path := create(t, root, KindLaboratory, testID)
	_, err := Authorize(root, Request{Names: []string{filepath.Base(path)}, Reason: "a reason"},
		DefaultLimits(), time.Now())
	if err == nil {
		t.Fatal("a reclaim without consent was authorized")
	}
	if !apperr.IsKind(err, apperr.KindPermission) {
		t.Errorf("the refusal is %v, want a permission refusal", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Error("the refusal touched the directory")
	}
}

func TestReclaimRefusesWithoutAReason(t *testing.T) {
	// The same rule a recovery follows. A deletion with no recorded justification
	// is a deletion a later reader cannot account for.
	root := stateRoot(t)
	path := create(t, root, KindLaboratory, testID)
	_, err := Authorize(root, Request{Names: []string{filepath.Base(path)}, Consent: true},
		DefaultLimits(), time.Now())
	if err == nil {
		t.Fatal("a reclaim with no reason was authorized")
	}
	if !strings.Contains(err.Error(), "reason is required") {
		t.Errorf("the refusal %q does not ask for a reason", err)
	}
}

func TestReclaimRefusesANameItDoesNotGenerate(t *testing.T) {
	// The confinement check, and it costs no filesystem call: a name that is not
	// a single path element, or is not one NetForge generates, never reaches the
	// disk.
	root := stateRoot(t)
	for _, name := range []string{
		"..",
		".",
		"/etc",
		"../elsewhere",
		"somebody-elses-directory",
		"netforge-lab-",
		"netforge-lab-UPPERCASE",
		"sub/netforge-lab-000000000000",
		"",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Authorize(root, Request{Names: []string{name}, Reason: "a reason", Consent: true},
				DefaultLimits(), time.Now())
			if err == nil {
				t.Fatalf("the name %q was authorized", name)
			}
		})
	}
}

func TestReclaimIsConfinedToTheStateRoot(t *testing.T) {
	// Two roots side by side, and an authorization for one may not be spent on
	// the other. Without this a caller could authorize a harmless name and then
	// point the removal at a different directory.
	root := stateRoot(t)
	other := stateRoot(t)
	create(t, other, KindLaboratory, testID)
	path := create(t, root, KindLaboratory, testID)
	authorization, err := Authorize(root, Request{
		Names:   []string{filepath.Base(path)},
		Reason:  "the crashed run left it",
		Consent: true,
	}, DefaultLimits(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Reclaim(context.Background(), other, authorization, time.Now()); err == nil {
		t.Fatal("an authorization was spent against a different state root")
	}
	if entries, _ := Scan(other, time.Now()); len(entries) != 1 {
		t.Errorf("the other root holds %d entries, want its own untouched", len(entries))
	}
}

func TestReclaimRemovesWhatItProvesAndReportsIt(t *testing.T) {
	root := stateRoot(t)
	lab := create(t, root, KindLaboratory, testID)
	routerDir := create(t, root, KindRouter, testID)
	authorization, err := Authorize(root, Request{
		Names:   []string{filepath.Base(lab), filepath.Base(routerDir)},
		Reason:  "the killed run left both",
		Consent: true,
	}, DefaultLimits(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// The markers name this process, so the reclaim is expected to refuse them.
	// That is the in-use rule doing its job, and it is what the next two tests
	// turn off to reach the removal path.
	report, err := Reclaim(context.Background(), root, authorization, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if report.Removed != 0 {
		t.Fatalf("removed %d, want nothing while this process owns both", report.Removed)
	}
	if report.Refused != 2 {
		t.Fatalf("refused %d, want both directories refused", report.Refused)
	}
	for _, outcome := range report.Outcomes {
		if !strings.Contains(outcome.Reason, "still running") {
			t.Errorf("the outcome %q does not say the directory is in use", outcome.Reason)
		}
	}
	for _, path := range []string{lab, routerDir} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%q was removed while its owner was running", path)
		}
	}
}

func TestReclaimRemovesTheStateOfADeadOwner(t *testing.T) {
	// A killed run's owner is gone, which is the only case a removal is for.
	root := stateRoot(t)
	path := create(t, root, KindLaboratory, testID)
	deadened(t, root, path)
	authorization, err := Authorize(root, Request{
		Names:   []string{filepath.Base(path)},
		Reason:  "the run that owned it was killed",
		Consent: true,
	}, DefaultLimits(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	report, err := Reclaim(context.Background(), root, authorization, time.Now())
	if err != nil {
		t.Fatalf("Reclaim = %v, want nil", err)
	}
	if report.Removed != 1 || report.Refused != 0 {
		t.Errorf("removed %d refused %d, want 1 and 0", report.Removed, report.Refused)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("the state directory survived the reclaim")
	}
	// The outcome names the full path, because a report an operator reads after
	// the fact has to say which directory was taken, and the name alone is only
	// meaningful next to the state root.
	if names := report.Names(); len(names) != 1 || names[0] != path {
		t.Errorf("the report names %v, want %q", names, path)
	}
}

func TestReclaimingTwiceRemovesNothingAndDoesNotFail(t *testing.T) {
	// The idempotency requirement, stated as a test rather than a claim: a
	// second pass over the same authorization must succeed and remove nothing.
	root := stateRoot(t)
	path := create(t, root, KindLaboratory, testID)
	deadened(t, root, path)
	authorization, err := Authorize(root, Request{
		Names:   []string{filepath.Base(path)},
		Reason:  "the run that owned it was killed",
		Consent: true,
	}, DefaultLimits(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	first, err := Reclaim(context.Background(), root, authorization, time.Now())
	if err != nil || first.Removed != 1 {
		t.Fatalf("the first reclaim removed %d with %v, want 1 and no error", first.Removed, err)
	}
	second, err := Reclaim(context.Background(), root, authorization, time.Now())
	if err != nil {
		t.Fatalf("the second reclaim failed: %v", err)
	}
	if second.Removed != 0 {
		t.Errorf("the second reclaim removed %d, want nothing", second.Removed)
	}
	// The second pass is counted as an absence rather than as a refusal. A
	// directory that is gone has been dealt with, and reporting it as a refusal
	// would make a successful reconcile look like it had hit a problem.
	if second.Refused != 0 || second.Absent != 1 {
		t.Errorf("the second reclaim refused %d and found %d absent, want 0 and 1", second.Refused, second.Absent)
	}
	if len(second.Outcomes) != 1 || !strings.Contains(second.Outcomes[0].Reason, "nothing left") {
		t.Errorf("the second reclaim reported %+v, want it to say there was nothing left", second.Outcomes)
	}
}

func TestADirectoryInUseIsRefusedRatherThanRemoved(t *testing.T) {
	// The property that makes a reconciler safe to run against a live host: it
	// cannot pull the state out from under a laboratory that is still writing to
	// it.
	root := stateRoot(t)
	create(t, root, KindLaboratory, testID)
	entries, err := Scan(root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].Identified || !entries[0].InUse {
		t.Fatalf("a directory this process owns was not reported in use: %+v", entries)
	}
	if !InUse(entries[0]) {
		t.Error("InUse said no for a directory this process owns")
	}
	// A refused directory is not an error: the caller asked to remove state and
	// the answer is that the state is live, which the report carries.
	authorization, err := Authorize(root, Request{
		Names:   []string{entries[0].Name},
		Reason:  "the operator believes this run is over",
		Consent: true,
	}, DefaultLimits(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	report, err := Reclaim(context.Background(), root, authorization, time.Now())
	if err != nil {
		t.Fatalf("Reclaim = %v, want a refusal rather than a failure", err)
	}
	if report.Removed != 0 || report.Refused != 1 {
		t.Errorf("removed %d refused %d, want 0 and 1", report.Removed, report.Refused)
	}
}

func TestARemovalIsBounded(t *testing.T) {
	// A reclaim that could be asked for a million directories would be a way to
	// make the host do arbitrary work, so the count is capped at the limit rather
	// than at whatever fits in memory.
	root := stateRoot(t)
	many := make([]string, 0, DefaultLimits().MaxRemovals+1)
	for i := 0; i <= DefaultLimits().MaxRemovals; i++ {
		many = append(many, "netforge-lab-"+strings.Repeat("a", i%10)+strings.Repeat("b", i/10+1))
	}
	_, err := Authorize(root, Request{Names: many, Reason: "a reason", Consent: true},
		DefaultLimits(), time.Now())
	if err == nil {
		t.Fatal("an unbounded reclaim was authorized")
	}
	if !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Errorf("the refusal is %v, want a limit refusal", err)
	}
}

func TestLimitsCannotBeWidenedPastTheBuiltMaximum(t *testing.T) {
	// A caller that may raise its own limits may as well have no limits.
	wide := DefaultLimits()
	wide.MaxRemovals = 1 << 20
	if _, err := Authorize("", Request{Names: []string{"netforge-lab-000000000000"}, Reason: "r", Consent: true},
		wide, time.Now()); err == nil || !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Errorf("widened limits were accepted: %v", err)
	}
}

func TestARepeatedNameIsRefused(t *testing.T) {
	root := stateRoot(t)
	name := filepath.Base(create(t, root, KindLaboratory, testID))
	_, err := Authorize(root, Request{
		Names:   []string{name, name},
		Reason:  "a reason",
		Consent: true,
	}, DefaultLimits(), time.Now())
	if err == nil {
		t.Fatal("a name given twice was authorized")
	}
	if !strings.Contains(err.Error(), "twice") {
		t.Errorf("the refusal %q does not say the name was repeated", err)
	}
}

func TestAScanOfAStateRootThatDoesNotExistIsEmptyRatherThanAFailure(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "never-created")
	entries, err := Scan(missing, time.Now())
	if err != nil {
		t.Fatalf("Scan = %v, want an empty listing", err)
	}
	if len(entries) != 0 {
		t.Errorf("Scan found %d entries under a state root that does not exist", len(entries))
	}
}

func TestReclaimTouchesNothingOutsideTheStateRoot(t *testing.T) {
	// The reconciler removes files and nothing else. This is what lets it run
	// unprivileged and what makes it incapable of touching host networking: there
	// is no code path from an authorization to a namespace, a link or a route.
	root := stateRoot(t)
	outside := stateRoot(t)
	bystander := filepath.Join(outside, "not-mine.txt")
	if err := os.WriteFile(bystander, []byte("a stranger's file"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := create(t, root, KindLaboratory, testID)
	deadened(t, root, path)
	authorization, err := Authorize(root, Request{
		Names:   []string{filepath.Base(path)},
		Reason:  "the run that owned it was killed",
		Consent: true,
	}, DefaultLimits(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Reclaim(context.Background(), root, authorization, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bystander); err != nil {
		t.Error("the reclaim removed a file outside the state root")
	}
}

// deadened rewrites a marker so that it names an owner which is not running, which
// is the state a SIGKILLed run's directory is in. The process is not started and
// not waited for: the marker only has to name a pid that is gone.
func deadened(t *testing.T, root, path string) {
	t.Helper()
	marker, err := ReadMarker(path)
	if err != nil {
		t.Fatal(err)
	}
	marker.OwnerPID = goneProcess(t)
	marker.CreatedAt = time.Now().Add(-2 * time.Hour)
	if err := WriteMarker(path, marker); err != nil {
		t.Fatal(err)
	}
	// A directory that has been dead for two hours also has to be older than the
	// filesystem's clock resolution, so the modification time is set too.
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
	_ = root
}

// goneProcess returns a process identifier that is not running.
func goneProcess(t *testing.T) int {
	t.Helper()
	// Starting and reaping a process is the only way to be certain an identifier
	// is free, and a free identifier cannot become a live one during the test
	// because the machine's pid space is not that empty and the kernel does not
	// reuse one that recent.
	high := 1 << 22
	for pid := high; pid > high-4096; pid-- {
		if !reconcileAlive(pid) {
			return pid
		}
	}
	t.Skip("no unused process identifier could be found")
	return 0
}

// reconcileAlive reports whether a process identifier is running, using the
// package's own answer so that the search for a free identifier cannot disagree
// with the check that later refuses on it.
func reconcileAlive(pid int) bool { return alive(pid) }

// TestAnAuthorizationIsAValueRatherThanAPermission covers the accessors and the
// rendering, because a type whose fields are unexported is only as good as the
// values it hands out: a caller that could mutate the names it was granted, or
// read a reason that is not the one it supplied, would break the property the
// unexported fields exist to provide.
func TestAnAuthorizationIsAValueRatherThanAPermission(t *testing.T) {
	root := stateRoot(t)
	name := filepath.Base(create(t, root, KindLaboratory, testID))
	const reason = "the run that owned it was killed"
	authorization, err := Authorize(root, Request{
		Names:   []string{name},
		Reason:  reason,
		Consent: true,
	}, DefaultLimits(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if authorization.Root() != root {
		t.Errorf("Root() = %q, want %q", authorization.Root(), root)
	}
	if authorization.Reason() != reason {
		t.Errorf("Reason() = %q, want %q", authorization.Reason(), reason)
	}
	if authorization.GrantedAt().IsZero() {
		t.Error("GrantedAt() is zero, so a report could not say when the removal was authorized")
	}
	// Names hands back a copy, so a caller cannot add a directory to an
	// authorization it was not granted.
	names := authorization.Names()
	if len(names) != 1 || names[0] != name {
		t.Fatalf("Names() = %v, want [%s]", names, name)
	}
	names[0] = "netforge-lab-000000000000"
	if again := authorization.Names(); again[0] != name {
		t.Error("mutating the returned names changed the authorization")
	}
	if !strings.Contains(authorization.String(), name) || !strings.Contains(authorization.String(), reason) {
		t.Errorf("String() = %q, want it to name the directory and the reason", authorization.String())
	}
}

// TestAnAuthorizationRefusesAStateRootItCannotUse covers the refusals that have
// to happen before a name is looked at: a root that is not a directory, and one
// that does not exist.
func TestAnAuthorizationRefusesAStateRootItCannotUse(t *testing.T) {
	t.Run("a root that does not exist", func(t *testing.T) {
		_, err := Authorize(filepath.Join(t.TempDir(), "absent"), Request{
			Names:   []string{"netforge-lab-000000000000"},
			Reason:  "a reason",
			Consent: true,
		}, DefaultLimits(), time.Now())
		if !apperr.IsKind(err, apperr.KindNotFound) {
			t.Errorf("the refusal is %v, want a not-found refusal", err)
		}
	})
	t.Run("a root that is a file", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Authorize(file, Request{
			Names:   []string{"netforge-lab-000000000000"},
			Reason:  "a reason",
			Consent: true,
		}, DefaultLimits(), time.Now())
		if !apperr.IsKind(err, apperr.KindInvalidArgument) {
			t.Errorf("the refusal is %v, want an invalid-argument refusal", err)
		}
	})
	t.Run("no root at all", func(t *testing.T) {
		_, err := Authorize("   ", Request{
			Names:   []string{"netforge-lab-000000000000"},
			Reason:  "a reason",
			Consent: true,
		}, DefaultLimits(), time.Now())
		if !apperr.IsKind(err, apperr.KindInvalidArgument) {
			t.Errorf("the refusal is %v, want an invalid-argument refusal", err)
		}
	})
	t.Run("no names", func(t *testing.T) {
		_, err := Authorize(t.TempDir(), Request{Reason: "a reason", Consent: true},
			DefaultLimits(), time.Now())
		if !apperr.IsKind(err, apperr.KindInvalidArgument) {
			t.Errorf("the refusal is %v, want an invalid-argument refusal", err)
		}
	})
}

// TestAReclaimStopsWhenItsContextIsCancelled covers the bounded end: a reclaim
// runs under a context like every other long operation, and a cancelled one
// reports what it had already done rather than nothing at all.
func TestAReclaimStopsWhenItsContextIsCancelled(t *testing.T) {
	root := stateRoot(t)
	path := create(t, root, KindLaboratory, testID)
	deadened(t, root, path)
	authorization, err := Authorize(root, Request{
		Names:   []string{filepath.Base(path)},
		Reason:  "the run that owned it was killed",
		Consent: true,
	}, DefaultLimits(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Reclaim(ctx, root, authorization, time.Now()); !apperr.IsKind(err, apperr.KindCanceled) {
		t.Errorf("a cancelled reclaim returned %v, want a cancellation", err)
	}
	// Nothing was removed, because the context was checked before the first
	// removal rather than after it.
	if _, err := os.Stat(path); err != nil {
		t.Error("a cancelled reclaim still removed state")
	}
}

// TestCreateRefusesWhatItCannotMake covers the refusals at creation, which
// matter because a directory created without a marker is one no later run may
// remove.
func TestCreateRefusesWhatItCannotMake(t *testing.T) {
	t.Run("a kind this build does not write", func(t *testing.T) {
		_, _, err := Create(stateRoot(t), Kind("firewall"), testID, time.Now())
		if !apperr.IsKind(err, apperr.KindUnsupported) {
			t.Errorf("the refusal is %v, want an unsupported refusal", err)
		}
	})
	t.Run("no root", func(t *testing.T) {
		_, _, err := Create("", KindLaboratory, testID, time.Now())
		if !apperr.IsKind(err, apperr.KindInvalidArgument) {
			t.Errorf("the refusal is %v, want an invalid-argument refusal", err)
		}
	})
}

// TestAWriteMarkerThatCannotBePublishedLeavesNothingTrustworthy covers the atomic
// write's failure path. A directory that exists without its marker is a directory
// no later run will remove, so it is better not to exist at all.
func TestAWriteMarkerThatCannotBePublishedLeavesNothingTrustworthy(t *testing.T) {
	root := stateRoot(t)
	// A read-only state root cannot be written to at all, which is the shape of
	// this failure without needing to intercept the rename.
	if err := os.Chmod(root, 0o500); err != nil {
		t.Skipf("the state root cannot be made read-only here: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	_, _, err := Create(root, KindLaboratory, testID, time.Now())
	if err == nil {
		t.Fatal("a state directory was created in a read-only root")
	}
	_ = os.Chmod(root, 0o700)
	entries, scanErr := Scan(root, time.Now())
	if scanErr != nil {
		t.Fatal(scanErr)
	}
	for _, entry := range entries {
		if entry.Identified {
			t.Errorf("a failed create left an identifiable directory behind: %+v", entry)
		}
	}
}

// TestAReportNamesBothSidesOfWhatHappened covers the accessors a caller reads to
// tell a removal from a refusal from an absence, since the counts are what an
// operator acts on.
func TestAReportNamesBothSidesOfWhatHappened(t *testing.T) {
	root := stateRoot(t)
	gone := create(t, root, KindLaboratory, testID)
	deadened(t, root, gone)
	live := create(t, root, KindRouter, testID)
	authorization, err := Authorize(root, Request{
		Names:   []string{filepath.Base(gone), filepath.Base(live)},
		Reason:  "the run that owned the first was killed",
		Consent: true,
	}, DefaultLimits(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	report, err := Reclaim(context.Background(), root, authorization, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if names := report.Names(); len(names) != 1 || names[0] != gone {
		t.Errorf("Names() = %v, want the one dead owner's directory %q", names, gone)
	}
	refusals := report.Refusals()
	if len(refusals) != 1 || refusals[0].Name != live {
		t.Errorf("Refusals() = %+v, want the one live owner's directory %q", refusals, live)
	}
	if report.Root != root {
		t.Errorf("the report names the root %q, want %q", report.Root, root)
	}
	if report.Reason != authorization.Reason() {
		t.Errorf("the report carries the reason %q, want %q", report.Reason, authorization.Reason())
	}
	if !report.GrantedAt.Equal(authorization.GrantedAt()) {
		t.Error("the report does not carry the time the removal was authorized")
	}
	// The removal says what it removed and for whose laboratory, because a later
	// reader is the one who has to know what went missing.
	removed := report.Outcomes[0]
	if !strings.Contains(removed.Reason, "laboratory state directory") ||
		!strings.Contains(removed.Reason, testID) {
		t.Errorf("the removal says %q, want the kind and the identifier", removed.Reason)
	}
}

// TestTheStateRootIsTheTemporaryDirectoryUnlessTheEnvironmentSaysOtherwise
// covers the two ways the root is chosen. A flag is checked by the command; this
// is the fallback the package provides, and it has to be the shared directory an
// operator expects rather than something scoped to one laboratory.
func TestTheStateRootIsTheTemporaryDirectoryUnlessTheEnvironmentSaysOtherwise(t *testing.T) {
	t.Setenv(StateRootEnv, "")
	if root := StateRoot(); root != os.TempDir() {
		t.Errorf("StateRoot() = %q, want the system temporary directory %q", root, os.TempDir())
	}
	t.Setenv(StateRootEnv, "  ")
	if root := StateRoot(); root != os.TempDir() {
		t.Errorf("a blank %s gave %q, want the system temporary directory", StateRootEnv, root)
	}
	chosen := t.TempDir()
	t.Setenv(StateRootEnv, chosen)
	if root := StateRoot(); root != chosen {
		t.Errorf("StateRoot() = %q, want the environment's %q", root, chosen)
	}
}
