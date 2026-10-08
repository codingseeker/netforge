package reconcile

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codingseeker/netforge/internal/apperr"
)

const testID = "nfrecon"

func stateRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

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

	root := stateRoot(t)
	first := create(t, root, KindLaboratory, testID)
	second := create(t, root, KindLaboratory, testID)
	if first == second {
		t.Fatal("two laboratories were given the same state directory")
	}
}

func TestTheTwoKindsAreToldApart(t *testing.T) {

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

	root := stateRoot(t)
	path := create(t, root, KindLaboratory, testID)
	deadened(t, path)
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

	if names := report.Names(); len(names) != 1 || names[0] != path {
		t.Errorf("the report names %v, want %q", names, path)
	}
}

func TestReclaimingTwiceRemovesNothingAndDoesNotFail(t *testing.T) {

	root := stateRoot(t)
	path := create(t, root, KindLaboratory, testID)
	deadened(t, path)
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

	if second.Refused != 0 || second.Absent != 1 {
		t.Errorf("the second reclaim refused %d and found %d absent, want 0 and 1", second.Refused, second.Absent)
	}
	if len(second.Outcomes) != 1 || !strings.Contains(second.Outcomes[0].Reason, "nothing left") {
		t.Errorf("the second reclaim reported %+v, want it to say there was nothing left", second.Outcomes)
	}
}

func TestADirectoryInUseIsRefusedRatherThanRemoved(t *testing.T) {

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

	root := stateRoot(t)
	outside := stateRoot(t)
	bystander := filepath.Join(outside, "not-mine.txt")
	if err := os.WriteFile(bystander, []byte("a stranger's file"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := create(t, root, KindLaboratory, testID)
	deadened(t, path)
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

func deadened(t *testing.T, path string) {
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

	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
}

func goneProcess(t *testing.T) int {
	t.Helper()

	high := 1 << 22
	for pid := high; pid > high-4096; pid-- {
		if !reconcileAlive(pid) {
			return pid
		}
	}
	t.Skip("no unused process identifier could be found")
	return 0
}

func reconcileAlive(pid int) bool { return alive(pid) }

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

func TestAReclaimStopsWhenItsContextIsCancelled(t *testing.T) {
	root := stateRoot(t)
	path := create(t, root, KindLaboratory, testID)
	deadened(t, path)
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

	if _, err := os.Stat(path); err != nil {
		t.Error("a cancelled reclaim still removed state")
	}
}

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

func TestAWriteMarkerThatCannotBePublishedLeavesNothingTrustworthy(t *testing.T) {
	root := stateRoot(t)

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

func TestAReportNamesBothSidesOfWhatHappened(t *testing.T) {
	root := stateRoot(t)
	gone := create(t, root, KindLaboratory, testID)
	deadened(t, gone)
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

	removed := report.Outcomes[0]
	if !strings.Contains(removed.Reason, "laboratory state directory") ||
		!strings.Contains(removed.Reason, testID) {
		t.Errorf("the removal says %q, want the kind and the identifier", removed.Reason)
	}
}

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

func TestOneListingIsBounded(t *testing.T) {
	limit := DefaultLimits().MaxEntries

	t.Run("a root exactly at the bound is enumerated", func(t *testing.T) {
		root := stateRoot(t)
		fill(t, root, limit)
		entries, err := Scan(root, time.Now())
		if err != nil {
			t.Fatalf("Scan = %v, want nil at exactly %d entries", err, limit)
		}
		if len(entries) != limit {
			t.Errorf("Scan returned %d entries, want %d", len(entries), limit)
		}
	})

	t.Run("a root past the bound is refused, not enumerated", func(t *testing.T) {
		root := stateRoot(t)
		fill(t, root, limit+1)
		entries, err := Scan(root, time.Now())
		if !apperr.IsKind(err, apperr.KindLimitExceeded) {
			t.Fatalf("Scan = %v, want a limit-exceeded refusal", err)
		}
		if entries != nil {
			t.Errorf("a refused listing returned %d entries, want none", len(entries))
		}
		if !strings.Contains(err.Error(), strconv.Itoa(limit)) {
			t.Errorf("the refusal %q does not name the bound %d", err, limit)
		}
	})
}

func fill(t *testing.T, root string, count int) {
	t.Helper()
	for i := range count {
		name := filepath.Join(root, "entry-"+strconv.Itoa(i))
		if err := os.WriteFile(name, nil, 0o600); err != nil {
			t.Fatalf("WriteFile(%s) = %v, want nil", name, err)
		}
	}
}
