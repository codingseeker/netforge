package security

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codingseeker/netforge/internal/apperr"
	"github.com/codingseeker/netforge/internal/reconcile"
)

func auditRoot(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func auditCreate(t *testing.T, root string, kind reconcile.Kind) string {
	t.Helper()
	path, _, err := reconcile.Create(root, kind, "nfaudit", time.Now())
	if err != nil {
		t.Fatalf("Create(%s) = %v, want nil", kind, err)
	}
	return path
}

func deadOwner(t *testing.T, path string) {
	t.Helper()
	marker, err := reconcile.ReadMarker(path)
	if err != nil {
		t.Fatalf("ReadMarker(%s) = %v, want nil", path, err)
	}
	marker.OwnerPID = freePID(t)
	if err := reconcile.WriteMarker(path, marker); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
}

func freePID(t *testing.T) int {
	t.Helper()
	const ceiling = 1 << 22
	for pid := ceiling; pid > ceiling-4096; pid-- {
		if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); err != nil {
			return pid
		}
	}
	t.Skip("no unused process identifier could be found")
	return 0
}

func auditAuthorize(t *testing.T, root string, names ...string) reconcile.Authorization {
	t.Helper()
	authorization, err := reconcile.Authorize(root, reconcile.Request{
		Names:   names,
		Reason:  "the audit is reclaiming state it created",
		Consent: true,
	}, reconcile.DefaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("Authorize(%v) = %v, want nil", names, err)
	}
	return authorization
}

func TestTheReconcilerRemovesNothingItCannotProveOwns(t *testing.T) {
	root := auditRoot(t)
	outside := auditRoot(t)

	bystander := filepath.Join(outside, "a-file-of-someone-elses.txt")
	if err := os.WriteFile(bystander, []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}

	impostor := filepath.Join(root, "netforge-lab-000000000000")
	if err := os.Mkdir(impostor, 0o700); err != nil {
		t.Fatal(err)
	}
	impostorFile := filepath.Join(impostor, "important.txt")
	if err := os.WriteFile(impostorFile, []byte("also not ours"), 0o600); err != nil {
		t.Fatal(err)
	}

	target := auditRoot(t)
	targetFile := filepath.Join(target, "symlinked.txt")
	if err := os.WriteFile(targetFile, []byte("behind a symlink"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "netforge-router-111111111111")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks are unavailable here: %v", err)
	}

	file := filepath.Join(root, "netforge-lab-222222222222")
	if err := os.WriteFile(file, []byte("a file"), 0o600); err != nil {
		t.Fatal(err)
	}

	garbage := filepath.Join(root, "netforge-lab-333333333333")
	if err := os.Mkdir(garbage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reconcile.MarkerPath(garbage), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	genuine := auditCreate(t, root, reconcile.KindLaboratory)
	copied := filepath.Join(root, "netforge-lab-444444444444")
	if err := os.Mkdir(copied, 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(reconcile.MarkerPath(genuine))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reconcile.MarkerPath(copied), body, 0o600); err != nil {
		t.Fatal(err)
	}
	deadOwner(t, genuine)

	names := []string{
		filepath.Base(impostor),
		filepath.Base(link),
		filepath.Base(file),
		filepath.Base(garbage),
		filepath.Base(copied),
		filepath.Base(genuine),
	}
	report, err := reconcile.Reclaim(context.Background(), root, auditAuthorize(t, root, names...), time.Now())
	if err != nil {
		t.Fatalf("Reclaim = %v, want a report rather than a failure", err)
	}
	if report.Removed != 1 {
		t.Errorf("removed %d, want only the one directory that was provably ours", report.Removed)
	}
	if report.Refused != 5 {
		t.Errorf("refused %d, want 5: the impostor, the symlink, the file, the garbage marker and the copied marker",
			report.Refused)
	}

	for _, outcome := range report.Refusals() {
		if !strings.HasPrefix(outcome.Reason, "refused:") || len(outcome.Reason) < 20 {
			t.Errorf("the refusal %q does not say why", outcome.Reason)
		}
	}

	for _, path := range []string{impostorFile, targetFile, file, bystander} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("the reconciler removed %q, which it never proved was NetForge's", path)
		}
	}
	for _, dir := range []string{impostor, garbage, copied} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("the reconciler removed %q, which carried no valid marker", dir)
		}
	}
	if _, err := os.Lstat(link); err != nil {
		t.Error("the reconciler removed the symlink itself, which should have been left for its owner")
	}
	if _, err := os.Stat(genuine); err == nil {
		t.Error("the reconciler left the one directory that was provably ours")
	}
}

func TestOwnershipCannotBeForgedByWritingTheMarker(t *testing.T) {
	root := auditRoot(t)
	forged := filepath.Join(root, "netforge-lab-555555555555")
	if err := os.Mkdir(forged, 0o700); err != nil {
		t.Fatal(err)
	}

	marker := reconcile.Marker{
		Version:   reconcile.MarkerVersion,
		Kind:      reconcile.KindLaboratory,
		ID:        "nfaudit",
		Name:      filepath.Base(forged),
		OwnerPID:  os.Getpid(),
		CreatedAt: time.Now().Add(-48 * time.Hour),
	}
	if err := reconcile.WriteMarker(forged, marker); err != nil {
		t.Fatal(err)
	}
	report, err := reconcile.Reclaim(context.Background(), root,
		auditAuthorize(t, root, filepath.Base(forged)), time.Now())
	if err != nil {
		t.Fatalf("Reclaim = %v, want a refusal", err)
	}
	if report.Removed != 0 {
		t.Error("a directory whose marker named a running process was removed")
	}
	if _, err := os.Stat(forged); err != nil {
		t.Error("the forged directory was removed even though its owner is running")
	}
}

func TestTheReconcilerIsConfinedToItsStateRoot(t *testing.T) {
	ours := auditRoot(t)
	theirs := auditRoot(t)
	victim := filepath.Join(theirs, "netforge-lab-666666666666")
	if err := os.Mkdir(victim, 0o700); err != nil {
		t.Fatal(err)
	}

	authorization := auditAuthorize(t, ours, filepath.Base(victim))
	if _, err := reconcile.Reclaim(context.Background(), theirs, authorization, time.Now()); err == nil {
		t.Fatal("an authorization was spent against a different state root")
	} else if !apperr.IsKind(err, apperr.KindPermission) {
		t.Errorf("the refusal is %v, want a permission refusal", err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Error("the reconciler reached outside its state root")
	}
}

func TestAMarkerIsWrittenWholeOrNotAtAll(t *testing.T) {
	root := auditRoot(t)

	partial := filepath.Join(root, "netforge-lab-777777777777")
	if err := os.Mkdir(partial, 0o700); err != nil {
		t.Fatal(err)
	}
	whole := `{"version":1,"kind":"laboratory","id":"nfaudit","name":"netforge-lab-777777777777"`
	if err := os.WriteFile(reconcile.MarkerPath(partial), []byte(whole), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcile.ReadMarker(partial); err == nil {
		t.Fatal("a truncated marker was accepted as ownership")
	}
	entries, err := reconcile.Scan(root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Identified {
		t.Errorf("a truncated marker was identified: %+v", entries)
	}

	scratch := filepath.Join(root, reconcile.MarkerFile+".123456.tmp")
	if err := os.WriteFile(scratch, []byte(whole), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err = reconcile.Scan(root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := reconcile.Identified(entries); len(got) != 0 {
		t.Errorf("Identified returned %d entries, want none", len(got))
	}
}

func TestTheReconcilerCannotReachTheHostNetwork(t *testing.T) {

	root := auditRoot(t)
	path := auditCreate(t, root, reconcile.KindRouter)
	deadOwner(t, path)

	inside := filepath.Join(path, "router.json")
	if err := os.WriteFile(inside, []byte(`{"state":"running"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcile.Reclaim(context.Background(), root,
		auditAuthorize(t, root, filepath.Base(path)), time.Now()); err != nil {
		t.Fatalf("Reclaim = %v, want nil", err)
	}
	if _, err := os.Stat(inside); err == nil {
		t.Error("the reclaim did not remove the directory it was authorized for")
	}

	entries, err := reconcile.Scan(root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the reconciler left %d entries behind: %+v", len(entries), entries)
	}
}
