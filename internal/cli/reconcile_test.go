package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/netforge/netforge/internal/reconcile"
)

func phase15Root(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func phase15Create(t *testing.T, root string, kind reconcile.Kind, id string) string {
	t.Helper()
	path, _, err := reconcile.Create(root, kind, id, time.Now())
	if err != nil {
		t.Fatalf("Create(%s) = %v, want nil", kind, err)
	}
	return path
}

func phase15List(t *testing.T, root string) runResult {
	return run(t, context.Background(), "", "reconcile", "list", "--state-root", root)
}

func TestReconcileListNamesTheIdentifierKindAndAge(t *testing.T) {
	root := phase15Root(t)
	path := phase15Create(t, root, reconcile.KindLaboratory, "nfp15")
	result := phase15List(t, root)
	if result.err != nil {
		t.Fatalf("Run(reconcile list) = %v, want nil\n%s", result.err, result.stderr)
	}
	for _, want := range []string{
		filepath.Base(path),
		"nfp15",
		"laboratory",
		"state root: " + root,
	} {
		if !strings.Contains(result.stdout, want) {
			t.Errorf("the listing %q is missing %q", result.stdout, want)
		}
	}
	if !strings.Contains(result.stdout, "1 identified as NetForge's, 0 not identified") {
		t.Errorf("the listing does not say what it identified:\n%s", result.stdout)
	}
}

func TestReconcileListTellsTheTwoKindsApart(t *testing.T) {

	root := phase15Root(t)
	phase15Create(t, root, reconcile.KindLaboratory, "nfp15")
	routerDir := phase15Create(t, root, reconcile.KindRouter, "nfp15")
	result := phase15List(t, root)
	if result.err != nil {
		t.Fatalf("Run(reconcile list) = %v, want nil", result.err)
	}
	labLine := listingLine(result.stdout, "netforge-lab-")
	routerLine := listingLine(result.stdout, "netforge-router-")
	if !strings.Contains(labLine, "laboratory") {
		t.Errorf("the laboratory's line %q does not say what it is for", labLine)
	}
	if !strings.Contains(routerLine, "router") {
		t.Errorf("the router's line %q does not say what it is for", routerLine)
	}
	if !strings.Contains(result.stdout, filepath.Base(routerDir)) {
		t.Errorf("the listing %q does not name the router's directory", result.stdout)
	}
}

func TestReconcileListClaimsNothingItCannotProve(t *testing.T) {

	root := phase15Root(t)
	forged := filepath.Join(root, "netforge-lab-000000000000")
	if err := os.Mkdir(forged, 0o700); err != nil {
		t.Fatal(err)
	}
	result := phase15List(t, root)
	if result.err != nil {
		t.Fatalf("Run(reconcile list) = %v, want nil", result.err)
	}
	if !strings.Contains(result.stdout, "unidentified") {
		t.Errorf("the listing %q does not say the directory is unidentified", result.stdout)
	}
	if !strings.Contains(result.stdout, "0 identified as NetForge's, 1 not identified") {
		t.Errorf("the listing claims ownership it does not have:\n%s", result.stdout)
	}

	if strings.Contains(result.stdout, "netforge reconcile reclaim") {
		t.Errorf("the listing suggests reclaiming something it did not identify:\n%s", result.stdout)
	}
}

func TestReconcileListOfACleanHostSaysSo(t *testing.T) {

	result := phase15List(t, phase15Root(t))
	if result.err != nil {
		t.Fatalf("Run(reconcile list) = %v, want nil", result.err)
	}
	if !strings.Contains(result.stdout, "nothing is there") {
		t.Errorf("an empty state root was not reported as empty:\n%s", result.stdout)
	}
}

func TestReclaimIsRefusedWithoutAuthorization(t *testing.T) {
	root := phase15Root(t)
	path := phase15Create(t, root, reconcile.KindLaboratory, "nfp15")
	result := run(t, context.Background(), "",
		"reconcile", "reclaim",
		"--state-root", root,
		"--id", filepath.Base(path),
		"--why", "the run that owned it was killed")
	if result.err == nil {
		t.Fatal("a reclaim without -authorize was accepted")
	}
	if !strings.Contains(result.err.Error(), "explicit authorization") {
		t.Errorf("the refusal %q does not say authorization was required", result.err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("the refused reclaim touched the directory")
	}
}

func TestReclaimIsRefusedWithoutAReason(t *testing.T) {

	root := phase15Root(t)
	path := phase15Create(t, root, reconcile.KindLaboratory, "nfp15")
	result := run(t, context.Background(), "",
		"reconcile", "reclaim",
		"--state-root", root,
		"--id", filepath.Base(path),
		"--authorize")
	if result.err == nil {
		t.Fatal("a reclaim with no -why was accepted")
	}
	if !strings.Contains(result.err.Error(), "reason is required") {
		t.Errorf("the refusal %q does not ask for a reason", result.err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("the refused reclaim touched the directory")
	}
}

func TestReclaimRefusesANameItDoesNotGenerate(t *testing.T) {

	root := phase15Root(t)
	for _, name := range []string{"..", "/etc", "../escape", "someone-elses-directory"} {
		t.Run(name, func(t *testing.T) {
			result := run(t, context.Background(), "",
				"reconcile", "reclaim",
				"--state-root", root,
				"--id", name,
				"--authorize",
				"--why", "an operator guessed at a name")
			if result.err == nil {
				t.Fatalf("the name %q was accepted", name)
			}
		})
	}
}

func TestReclaimRefusesADirectoryThatIsStillInUse(t *testing.T) {

	root := phase15Root(t)
	path := phase15Create(t, root, reconcile.KindLaboratory, "nfp15")
	result := run(t, context.Background(), "",
		"reconcile", "reclaim",
		"--state-root", root,
		"--id", filepath.Base(path),
		"--authorize",
		"--why", "the operator believes the run is over")
	if result.err == nil {
		t.Fatal("a reclaim of a live directory reported success")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("the directory was removed while its owner was running")
	}
	if !strings.Contains(result.stdout, "still running") {
		t.Errorf("the report does not say the directory is in use:\n%s", result.stdout)
	}
	if !strings.Contains(result.stdout, "refused") {
		t.Errorf("the report does not say it refused:\n%s", result.stdout)
	}
}

func TestReclaimReportsWhatItRemovedAndIsIdempotent(t *testing.T) {

	root := phase15Root(t)
	path := phase15Create(t, root, reconcile.KindLaboratory, "nfp15")
	deadOwner(t, path)
	args := []string{
		"reconcile", "reclaim",
		"--state-root", root,
		"--id", filepath.Base(path),
		"--authorize",
		"--why", "the run that owned it was killed",
	}
	first := run(t, context.Background(), "", args...)
	if first.err != nil {
		t.Fatalf("the first reclaim failed: %v\n%s", first.err, first.stdout)
	}
	if !strings.Contains(first.stdout, "removed 1, refused 0, already gone 0") {
		t.Errorf("the first reclaim did not report one removal:\n%s", first.stdout)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("the state directory survived the reclaim")
	}
	second := run(t, context.Background(), "", args...)
	if second.err != nil {
		t.Fatalf("the second reclaim failed: %v\n%s", second.err, second.stdout)
	}
	if !strings.Contains(second.stdout, "removed 0, refused 0, already gone 1") {
		t.Errorf("the second reclaim removed something:\n%s", second.stdout)
	}
}

func TestReclaimCarriesTheReasonIntoItsReport(t *testing.T) {
	root := phase15Root(t)
	path := phase15Create(t, root, reconcile.KindLaboratory, "nfp15")
	deadOwner(t, path)
	const reason = "the run that owned it was killed and the host is being rebuilt"
	result := run(t, context.Background(), "",
		"reconcile", "reclaim",
		"--state-root", root,
		"--id", filepath.Base(path),
		"--authorize",
		"--why", reason)
	if result.err != nil {
		t.Fatalf("Run(reconcile reclaim) = %v, want nil", result.err)
	}
	if !strings.Contains(result.stdout, reason) {
		t.Errorf("the report does not carry the reason:\n%s", result.stdout)
	}
}

func TestReclaimLeavesAnUnidentifiedDirectoryAlone(t *testing.T) {

	root := phase15Root(t)
	forged := filepath.Join(root, "netforge-lab-000000000000")
	if err := os.Mkdir(forged, 0o700); err != nil {
		t.Fatal(err)
	}
	result := run(t, context.Background(), "",
		"reconcile", "reclaim",
		"--state-root", root,
		"--id", filepath.Base(forged),
		"--authorize",
		"--why", "the name looked like one of ours")
	if result.err == nil {
		t.Fatal("a reclaim of an unidentified directory reported success")
	}
	if _, err := os.Stat(forged); err != nil {
		t.Fatal("an unidentified directory was removed")
	}
	if !strings.Contains(result.stdout, "no ownership marker") {
		t.Errorf("the report does not say why it refused:\n%s", result.stdout)
	}
}

func TestAnOlderThanGateRefusesAYoungDirectory(t *testing.T) {

	root := phase15Root(t)
	path := phase15Create(t, root, reconcile.KindLaboratory, "nfp15")
	deadOwner(t, path)
	result := run(t, context.Background(), "",
		"reconcile", "reclaim",
		"--state-root", root,
		"--id", filepath.Base(path),
		"--older-than", "24h",
		"--authorize",
		"--why", "only reclaim state that is definitely stale")
	if result.err == nil {
		t.Fatal("a directory younger than the threshold was reclaimed")
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("a directory younger than the threshold was removed")
	}
	if !strings.Contains(result.err.Error(), "-older-than") {
		t.Errorf("the refusal %q does not name the flag", result.err)
	}
}

func TestReconcileRefusesExtraArgumentsAndUnknownSubcommands(t *testing.T) {
	if result := run(t, context.Background(), "", "reconcile", "list", "extra"); result.err == nil ||
		!strings.Contains(result.err.Error(), "unexpected argument") {
		t.Errorf("an extra argument gave %v, want a refusal naming it", result.err)
	}
	if result := run(t, context.Background(), "", "reconcile", "frobnicate"); result.err == nil ||
		!strings.Contains(result.err.Error(), "unknown subcommand") {
		t.Errorf("an unknown subcommand gave %v, want a refusal naming it", result.err)
	}
	if result := run(t, context.Background(), "", "reconcile"); result.err == nil ||
		!strings.Contains(result.err.Error(), "missing subcommand") {
		t.Errorf("a bare reconcile gave %v, want a refusal asking for a subcommand", result.err)
	}
}

func TestReconcileAppearsInHelp(t *testing.T) {

	root := run(t, context.Background(), "", "help")
	if !strings.Contains(root.stdout, "reconcile") {
		t.Errorf("the root help does not mention the reconcile command:\n%s", root.stdout)
	}
	group := run(t, context.Background(), "", "help", "reconcile")
	if group.err != nil {
		t.Fatalf("Run(help reconcile) = %v, want nil", group.err)
	}
	for _, sub := range []string{"list", "reclaim"} {
		if !strings.Contains(group.stdout, sub) {
			t.Errorf("the reconcile help does not list %q:\n%s", sub, group.stdout)
		}
	}
	reclaim := run(t, context.Background(), "", "help", "reconcile", "reclaim")
	if reclaim.err != nil {
		t.Fatalf("Run(help reconcile reclaim) = %v, want nil", reclaim.err)
	}
	for _, want := range []string{"--id", "--authorize", "--why", "--state-root"} {
		if !strings.Contains(reclaim.stdout, want) {
			t.Errorf("the help for reclaim does not mention %q:\n%s", want, reclaim.stdout)
		}
	}
}

func TestTheStateRootDefaultsToTheSystemTemporaryDirectory(t *testing.T) {

	t.Setenv(reconcile.StateRootEnv, "/nonexistent/netforge-state")
	result := run(t, context.Background(), "", "reconcile", "list")
	if result.err != nil {
		t.Fatalf("Run(reconcile list) = %v, want nil", result.err)
	}
	if !strings.Contains(result.stdout, "/nonexistent/netforge-state") {
		t.Errorf("the listing did not use the environment's state root:\n%s", result.stdout)
	}
	override := phase15Root(t)
	phase15Create(t, override, reconcile.KindLaboratory, "nfp15")
	flagged := run(t, context.Background(), "", "reconcile", "list", "--state-root", override)
	if !strings.Contains(flagged.stdout, "nfp15") {
		t.Errorf("the flag did not win over the environment:\n%s", flagged.stdout)
	}
}

func listingLine(listing, fragment string) string {
	for _, line := range strings.Split(listing, "\n") {
		if strings.Contains(line, fragment) {
			return line
		}
	}
	return ""
}

func deadOwner(t *testing.T, path string) {
	t.Helper()
	marker, err := reconcile.ReadMarker(path)
	if err != nil {
		t.Fatalf("ReadMarker(%s) = %v, want nil", path, err)
	}
	marker.OwnerPID = unusedPID(t)
	past := time.Now().Add(-2 * time.Hour)
	marker.CreatedAt = past
	if err := reconcile.WriteMarker(path, marker); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
}

func unusedPID(t *testing.T) int {
	t.Helper()
	high := 1 << 22
	for pid := high; pid > high-4096; pid-- {
		if err := findProcess(pid); err != nil {
			return pid
		}
	}
	t.Skip("no unused process identifier could be found")
	return 0
}

func findProcess(pid int) error {
	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	return err
}

func TestReconcileResolvesItsStateRoot(t *testing.T) {

	flagged := phase15Root(t)
	phase15Create(t, flagged, reconcile.KindLaboratory, "nfp15")
	t.Setenv(reconcile.StateRootEnv, phase15Root(t))
	result := run(t, context.Background(), "", "reconcile", "list", "--state-root", flagged)
	if !strings.Contains(result.stdout, "nfp15") {
		t.Errorf("the flag did not win over the environment:\n%s", result.stdout)
	}

	fromEnv := phase15Root(t)
	phase15Create(t, fromEnv, reconcile.KindRouter, "nfp15")
	t.Setenv(reconcile.StateRootEnv, fromEnv)
	if got := run(t, context.Background(), "", "reconcile", "list"); !strings.Contains(got.stdout, "router") {
		t.Errorf("the environment's root was not used:\n%s", got.stdout)
	}

	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	refused := run(t, context.Background(), "", "reconcile", "list", "--state-root", file)
	if refused.err == nil {
		t.Fatalf("a file was used as a state root:\n%s", refused.stdout)
	}
	if !strings.Contains(refused.err.Error(), "not a directory") {
		t.Errorf("the refusal %q does not say the root is not a directory", refused.err)
	}
}

func TestASingularRefusalIsReportedAsOne(t *testing.T) {
	root := phase15Root(t)
	path := phase15Create(t, root, reconcile.KindLaboratory, "nfp15")
	result := run(t, context.Background(), "",
		"reconcile", "reclaim",
		"--state-root", root,
		"--id", filepath.Base(path),
		"--authorize",
		"--why", "the operator believes the run is over")
	if result.err == nil {
		t.Fatal("a reclaim of a live directory reported success")
	}
	if !strings.Contains(result.err.Error(), "1 name that was refused") {
		t.Errorf("the refusal %q does not read as a single name", result.err)
	}
}
