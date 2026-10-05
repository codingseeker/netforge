package reconcile

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/netforge/netforge/internal/apperr"
)

// Limits bound what a reclaim may do.
type Limits struct {
	// MaxRemovals is the most state directories one authorization may name.
	MaxRemovals int
	// MaxReasonLength is the longest justification an authorization may carry.
	MaxReasonLength int
	// MaxEntries is the most entries a listing may report.
	MaxEntries int
}

// DefaultLimits returns limits suited to reclaiming what a crashed run of one
// laboratory left behind.
func DefaultLimits() Limits {
	return Limits{
		MaxRemovals:     64,
		MaxReasonLength: 512,
		MaxEntries:      1024,
	}
}

func (l Limits) withDefaults() Limits {
	if l.MaxRemovals <= 0 {
		l.MaxRemovals = DefaultLimits().MaxRemovals
	}
	if l.MaxReasonLength <= 0 {
		l.MaxReasonLength = DefaultLimits().MaxReasonLength
	}
	if l.MaxEntries <= 0 {
		l.MaxEntries = DefaultLimits().MaxEntries
	}
	return l
}

func (l Limits) check() (Limits, error) {
	const op = "reconcile.Limits"
	checked := l.withDefaults()
	if checked.MaxRemovals > DefaultLimits().MaxRemovals {
		return checked, apperr.LimitExceeded(op, "limits.max_removals is "+
			strconv.Itoa(checked.MaxRemovals)+", the maximum is "+
			strconv.Itoa(DefaultLimits().MaxRemovals), nil)
	}
	if checked.MaxReasonLength > DefaultLimits().MaxReasonLength {
		return checked, apperr.LimitExceeded(op, "limits.max_reason_length is "+
			strconv.Itoa(checked.MaxReasonLength)+", the maximum is "+
			strconv.Itoa(DefaultLimits().MaxReasonLength), nil)
	}
	if checked.MaxEntries > DefaultLimits().MaxEntries {
		return checked, apperr.LimitExceeded(op, "limits.max_entries is "+
			strconv.Itoa(checked.MaxEntries)+", the maximum is "+
			strconv.Itoa(DefaultLimits().MaxEntries), nil)
	}
	return checked, nil
}

// Request is what an operator asks to have reclaimed.
type Request struct {
	// Names are the state directory names to reclaim. They are names, not paths:
	// a request cannot carry a path, so it cannot carry a way out of the state
	// root.
	Names []string
	// Reason is the operator's justification. It is required, for the same
	// reason a recovery requires one: a later reader has to be able to see why
	// something was deleted.
	Reason string
	// Consent is the explicit authorization. Without it the request is refused,
	// whatever the listing said.
	Consent bool
}

// Authorization is a request that passed every check: consent was given, a reason
// was written, and every name is one NetForge generates inside the state root.
//
// The fields are unexported for the reason recovery.Authorize's are: only
// Authorize can produce one, so holding an Authorization means the checks ran
// rather than that a caller believed they had.
type Authorization struct {
	root    string
	names   []string
	reason  string
	granted time.Time
}

// Names returns the state directory names the authorization covers.
func (a Authorization) Names() []string {
	return append([]string(nil), a.names...)
}

// Reason returns the operator's justification.
func (a Authorization) Reason() string { return a.reason }

// Root returns the state root the authorization is confined to.
func (a Authorization) Root() string { return a.root }

// GrantedAt is when the authorization was issued.
func (a Authorization) GrantedAt() time.Time { return a.granted }

// String renders the authorization for a log line.
func (a Authorization) String() string {
	return "reclaim " + strings.Join(a.names, ", ") + " (" + a.reason + ")"
}

// Authorize validates one request into an authorization.
//
// Every check here happens before the filesystem is touched at all, which is the
// point of the order: a name that is not one NetForge generates, or that names
// something outside the state root, is refused without ever being stat'd. The
// state root is shared, so the cheap checks come first and the expensive ones —
// the marker — only run for names that already look like NetForge's own.
func Authorize(root string, request Request, limits Limits, now time.Time) (Authorization, error) {
	const op = "reconcile.Authorize"
	checked, err := limits.check()
	if err != nil {
		return Authorization{}, err
	}
	if !request.Consent {
		return Authorization{}, apperr.Permission(op,
			"reclaiming state needs explicit authorization, and none was given", nil)
	}
	if len(request.Names) == 0 {
		return Authorization{}, apperr.InvalidArgument(op,
			"a reclaim needs the name of at least one state directory", nil)
	}
	if len(request.Names) > checked.MaxRemovals {
		return Authorization{}, apperr.LimitExceeded(op, "a reclaim names "+
			strconv.Itoa(len(request.Names))+" state directories, the maximum is "+
			strconv.Itoa(checked.MaxRemovals), nil)
	}
	reason, err := checkReason(op, request.Reason, checked)
	if err != nil {
		return Authorization{}, err
	}
	clean, err := checkRoot(op, root)
	if err != nil {
		return Authorization{}, err
	}
	names := make([]string, 0, len(request.Names))
	seen := make(map[string]bool, len(request.Names))
	for _, name := range request.Names {
		checked, err := checkName(op, clean, name)
		if err != nil {
			return Authorization{}, err
		}
		if seen[checked] {
			return Authorization{}, apperr.InvalidArgument(op,
				"the state directory "+strconv.Quote(checked)+" is named twice", nil)
		}
		seen[checked] = true
		names = append(names, checked)
	}
	return Authorization{root: clean, names: names, reason: reason, granted: now.UTC()}, nil
}

// checkRoot refuses a state root that is not a directory this process can act
// on, and refuses one that is a file, so a removal cannot be pointed at a file
// whose name happens to end in a separator.
func checkRoot(op, root string) (string, error) {
	clean := strings.TrimSpace(root)
	if clean == "" {
		return "", apperr.InvalidArgument(op, "a reclaim needs a state root", nil)
	}
	clean = filepath.Clean(clean)
	info, err := os.Stat(clean)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", apperr.NotFound(op, "the state root "+clean+" does not exist", err)
		}
		return "", apperr.IO(op, "the state root "+clean+" could not be read", err)
	}
	if !info.IsDir() {
		return "", apperr.InvalidArgument(op, "the state root "+clean+" is not a directory", nil)
	}
	return clean, nil
}

// checkName validates one requested name and returns the form the reconciler
// will use. It is the confinement check: the name has to be a single path
// element, it has to be one NetForge generates, and the path it names has to be
// a direct child of the state root. None of that reads the filesystem.
func checkName(op, root, name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", apperr.InvalidArgument(op, "a state directory name cannot be empty", nil)
	}
	if trimmed != filepath.Base(trimmed) || strings.ContainsRune(trimmed, os.PathSeparator) ||
		strings.ContainsRune(trimmed, '/') {
		return "", apperr.InvalidArgument(op,
			"a reclaim names a directory, not a path: "+strconv.Quote(trimmed), nil)
	}
	if _, generated := KindOf(trimmed); !generated {
		return "", apperr.InvalidArgument(op,
			strconv.Quote(trimmed)+" is not a name NetForge generates", nil)
	}
	path := filepath.Join(root, trimmed)
	if filepath.Dir(path) != root {
		return "", apperr.InvalidArgument(op,
			strconv.Quote(trimmed)+" does not name a directory inside the state root", nil)
	}
	return trimmed, nil
}

// checkReason refuses a reclaim without a justification.
func checkReason(op, reason string, limits Limits) (string, error) {
	if strings.TrimSpace(reason) == "" {
		return "", apperr.Validation(op,
			"reason is required, so a later reader can see why state was removed", nil)
	}
	if len(reason) > limits.MaxReasonLength {
		return "", apperr.LimitExceeded(op, "reason is "+strconv.Itoa(len(reason))+
			" characters, the maximum is "+strconv.Itoa(limits.MaxReasonLength), nil)
	}
	return reason, nil
}

// Outcome is what a reclaim did with one state directory.
type Outcome struct {
	// Name is the directory the outcome is about.
	Name string
	// Removed reports whether it was removed.
	Removed bool
	// Absent reports that there was nothing there to remove, which is what a
	// second pass over the same authorization finds. It is counted apart from a
	// refusal because it is not one: a directory that is gone has been dealt
	// with, and reporting it as a refusal would make a successful reconcile look
	// like it had hit a problem.
	Absent bool
	// Reason is why it was removed, refused, or found to be already gone.
	Reason string
}

// Report is what a reclaim did, and is returned whether it removed anything or
// not: a reconciler that reports nothing when it refuses everything is a
// reconciler an operator cannot tell from one that never ran.
type Report struct {
	// Root is the state root the reclaim was confined to.
	Root string
	// Reason is the authorization's justification.
	Reason string
	// GrantedAt is when the authorization was issued.
	GrantedAt time.Time
	// Outcomes are the removals, refusals and absences, in the order they were
	// named.
	Outcomes []Outcome
	// Removed is the number of directories taken back.
	Removed int
	// Refused is the number left alone because they could not be proven NetForge's
	// or were still in use.
	Refused int
	// Absent is the number that were already gone.
	Absent int
}

// Names returns the directories the reclaim removed.
func (r Report) Names() []string {
	var out []string
	for _, outcome := range r.Outcomes {
		if outcome.Removed {
			out = append(out, outcome.Name)
		}
	}
	return out
}

// Refusals returns the directories the reclaim left alone, with the reason.
func (r Report) Refusals() []Outcome {
	var out []Outcome
	for _, outcome := range r.Outcomes {
		if !outcome.Removed {
			out = append(out, outcome)
		}
	}
	return out
}

// Reclaim removes the state directories an authorization names, and reports what
// it removed and what it refused.
//
// It removes nothing it cannot positively identify, and nothing whose owning
// process is still running. It is idempotent: a directory that is already gone
// was removed, so reconciling twice reports the second time that there was
// nothing to do rather than failing. It touches files under the state root and
// nothing else — no namespace, no link, no route and no qdisc — which is why it
// needs no privileges and cannot damage host networking.
func Reclaim(ctx context.Context, root string, authorization Authorization, now time.Time) (Report, error) {
	const op = "reconcile.Reclaim"
	if err := apperr.FromContext(ctx, op); err != nil {
		return Report{}, err
	}
	// The authorization carries its own root. A caller cannot point an
	// authorization at one root and reclaim from another, and it cannot hand a
	// reclaim an authorization for a different root than the one it asks about.
	clean, err := checkRoot(op, root)
	if err != nil {
		return Report{}, err
	}
	if authorization.root != "" && clean != authorization.root {
		return Report{}, apperr.Permission(op, "the authorization is for state root "+
			strconv.Quote(authorization.root)+", not for "+strconv.Quote(clean), nil)
	}
	report := Report{Root: clean, Reason: authorization.reason, GrantedAt: authorization.granted}
	for _, name := range authorization.names {
		if err := apperr.FromContext(ctx, op); err != nil {
			return report, err
		}
		outcome, err := reclaimOne(op, clean, name, now)
		if err != nil {
			return report, err
		}
		switch {
		case outcome.Removed:
			report.Removed++
		case outcome.Absent:
			report.Absent++
		default:
			report.Refused++
		}
		report.Outcomes = append(report.Outcomes, outcome)
	}
	return report, nil
}

// reclaimOne removes one state directory, or refuses it with the reason.
func reclaimOne(op, root, name string, now time.Time) (Outcome, error) {
	checked, err := checkName(op, root, name)
	if err != nil {
		return Outcome{}, err
	}
	path := filepath.Join(root, checked)
	outcome := Outcome{Name: path}
	// The ownership check happens here rather than at authorization time on
	// purpose: authorization says what an operator may ask for, and this says
	// whether the thing on disk is still the thing they asked about. A directory
	// that was removed and recreated by something else between the two is
	// refused, because its marker is now a different marker's.
	// A directory that is not there at all has been dealt with already, which is
	// what makes reconciling twice the same operation as reconciling once. It is
	// checked apart from the marker, because "there is nothing there" and "there
	// is something there that is not ours" are different answers and conflating
	// them would let a refusal read as a success.
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			outcome.Absent = true
			outcome.Reason = "there was nothing left to remove"
			return outcome, nil
		}
		return outcome, apperr.IO(op, "the state directory "+path+" could not be read", err)
	}
	marker, err := ReadMarker(path)
	if err != nil {
		outcome.Reason = "refused: " + reasonOf(err)
		return outcome, nil
	}
	if alive(marker.OwnerPID) {
		outcome.Reason = "refused: the process that created it, pid " +
			strconv.Itoa(marker.OwnerPID) + ", is still running"
		return outcome, nil
	}
	if err := os.RemoveAll(path); err != nil {
		return outcome, apperr.IO(op, "the state directory "+path+" could not be removed", err)
	}
	if _, err := os.Stat(path); err == nil {
		return outcome, apperr.IO(op, "the state directory "+path+" survived removal", nil)
	}
	outcome.Removed = true
	outcome.Reason = "removed: " + marker.Kind.String() + " " + marker.ID +
		", created " + now.Sub(marker.CreatedAt).Round(time.Second).String() + " ago"
	return outcome, nil
}

// String names a kind in a sentence.
func (k Kind) String() string {
	switch k {
	case KindLaboratory:
		return "laboratory state directory"
	case KindRouter:
		return "router state directory"
	default:
		return string(k)
	}
}
