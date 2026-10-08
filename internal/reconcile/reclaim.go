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

	"github.com/codingseeker/netforge/internal/apperr"
)

type Limits struct {
	MaxRemovals int

	MaxReasonLength int

	MaxEntries int
}

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

type Request struct {
	Names []string

	Reason string

	Consent bool
}

type Authorization struct {
	root    string
	names   []string
	reason  string
	granted time.Time
}

func (a Authorization) Names() []string {
	return append([]string(nil), a.names...)
}

func (a Authorization) Reason() string { return a.reason }

func (a Authorization) Root() string { return a.root }

func (a Authorization) GrantedAt() time.Time { return a.granted }

func (a Authorization) String() string {
	return "reclaim " + strings.Join(a.names, ", ") + " (" + a.reason + ")"
}

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

type Outcome struct {
	Name string

	Removed bool

	Absent bool

	Reason string
}

type Report struct {
	Root string

	Reason string

	GrantedAt time.Time

	Outcomes []Outcome

	Removed int

	Refused int

	Absent int
}

func (r Report) Names() []string {
	var out []string
	for _, outcome := range r.Outcomes {
		if outcome.Removed {
			out = append(out, outcome.Name)
		}
	}
	return out
}

func (r Report) Refusals() []Outcome {
	var out []Outcome
	for _, outcome := range r.Outcomes {
		if !outcome.Removed {
			out = append(out, outcome)
		}
	}
	return out
}

func Reclaim(ctx context.Context, root string, authorization Authorization, now time.Time) (Report, error) {
	const op = "reconcile.Reclaim"
	if err := apperr.FromContext(ctx, op); err != nil {
		return Report{}, err
	}

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

func reclaimOne(op, root, name string, now time.Time) (Outcome, error) {
	checked, err := checkName(op, root, name)
	if err != nil {
		return Outcome{}, err
	}
	path := filepath.Join(root, checked)
	outcome := Outcome{Name: path}

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
