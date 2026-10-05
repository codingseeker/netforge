// Package reconcile finds and reclaims the state a killed NetForge run leaves
// behind.
//
// A run that finishes, or is interrupted, takes its state directory with it. A
// run killed with SIGKILL cannot: it releases its namespaces, because each
// namespace holder is tied to a pipe the agent holds, but the directory holding
// its service status files and its router's statistics survives. This package is
// how an operator finds that directory and takes it back.
//
// The whole of its safety argument is in the ownership check. The state root is
// shared — it is the system temporary directory — so nothing here removes
// anything it cannot prove is NetForge's own, and the proof is a marker NetForge
// wrote inside the directory when it created it. A directory without a valid
// marker is listed and left alone; it is never a candidate.
package reconcile

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/namespace"
)

// Kind is what a state directory is for. It is the first thing a reader needs,
// because the state root is shared and a directory named netforge-router-… is
// not a laboratory and one named netforge-lab-… is not a router's counters.
type Kind string

const (
	// KindLaboratory is a laboratory's state directory: the service status files
	// its services report into.
	KindLaboratory Kind = "laboratory"
	// KindRouter is the software router's state directory: the statistics file
	// it refreshes on SIGUSR1.
	KindRouter Kind = "router"
)

// MarkerFile is the name of the ownership marker inside a state directory.
const MarkerFile = "netforge-state.json"

// MarkerVersion is the marker format this build reads and writes. A marker that
// names another version is not this build's marker, and a directory whose marker
// names another version is not positively identified.
const MarkerVersion = 1

// nameBytes is how much randomness a generated directory name carries. Twelve
// hexadecimal characters is enough that two laboratories on one host do not
// collide, and short enough to read in a listing.
const nameBytes = 6

// KindOf reports the kind a generated directory name declares, and whether the
// name is one NetForge could have generated at all.
func KindOf(name string) (Kind, bool) {
	for _, kind := range Kinds() {
		if generatedName(kind, name) {
			return kind, true
		}
	}
	return "", false
}

// Kinds lists the kinds this build knows. A marker naming a kind that is not in
// this list is not positively identified: a future NetForge could write kinds
// this one would remove without understanding.
func Kinds() []Kind { return []Kind{KindLaboratory, KindRouter} }

// prefix returns the name prefix a kind's directories are generated with.
func prefix(kind Kind) string {
	switch kind {
	case KindLaboratory:
		return "netforge-lab-"
	case KindRouter:
		return "netforge-router-"
	default:
		return ""
	}
}

// generatedName reports whether name is one this build generates for a kind:
// the kind's prefix followed by a suffix that passes the same name validation a
// namespace does. It is the first half of the ownership argument, and it costs
// no filesystem call, which is why a name that fails it is refused before
// anything is read or removed.
func generatedName(kind Kind, name string) bool {
	if prefix(kind) == "" || !strings.HasPrefix(name, prefix(kind)) {
		return false
	}
	if err := namespace.ValidateName("reconcile", "name", name); err != nil {
		return false
	}
	return len(name) > len(prefix(kind))
}

// Marker is the proof that a directory is NetForge's.
//
// It is written once, when the directory is created, and it is written whole or
// not at all: a half-written marker is a marker that cannot be trusted, and an
// untrusted marker is not ownership.
type Marker struct {
	// Version is the marker format this file was written in.
	Version int `json:"version"`
	// Kind is what the directory is for.
	Kind Kind `json:"kind"`
	// ID is the identifier of the laboratory that owns the directory, or the
	// laboratory it was serving.
	ID string `json:"id"`
	// Name is the directory's own name. It is carried in the marker as well as
	// derived from the path, because a marker that names one directory while
	// sitting in another has been copied, and a copy is not ownership.
	Name string `json:"name"`
	// OwnerPID is the process that created the directory. A directory whose owner
	// is still running is in use, and is never removed.
	OwnerPID int `json:"owner_pid"`
	// CreatedAt is when the directory was created, which is what an age is
	// measured against.
	CreatedAt time.Time `json:"created_at"`
}

// StateRoot returns the directory NetForge keeps its state in. It is the
// system's temporary directory unless a caller chose another one, and nothing
// outside it is ever touched.
func StateRoot() string {
	if root := strings.TrimSpace(os.Getenv(StateRootEnv)); root != "" {
		return root
	}
	return os.TempDir()
}

// StateRootEnv names the environment variable that overrides the state root. It
// exists so a laboratory can be given a directory of its own on a host whose
// temporary directory is shared with software NetForge does not control.
const StateRootEnv = "NETFORGE_STATE_ROOT"

// Create makes a state directory of a kind and writes its marker, owned by this
// process. It hands back the path and the marker, so the caller can report what
// it made without reading it back.
func Create(root string, kind Kind, id string, now time.Time) (string, Marker, error) {
	const op = "reconcile.Create"
	if root == "" {
		return "", Marker{}, apperr.InvalidArgument(op, "a state directory needs a root", nil)
	}
	known := false
	for _, candidate := range Kinds() {
		if candidate == kind {
			known = true
		}
	}
	if !known {
		return "", Marker{}, apperr.Unsupported(op,
			"kind "+strconv.Quote(string(kind))+" is not a kind this build writes", nil)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", Marker{}, apperr.IO(op, "the state root "+root+" could not be prepared", err)
	}
	// The name is generated here rather than taken from the caller, because the
	// name is what a later run has to recognise as NetForge's own. A caller that
	// chose it could choose one that collides with something else.
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		name, err := generated(kind)
		if err != nil {
			return "", Marker{}, apperr.Wrap(err, apperr.KindInternal, op, "a state directory name could not be generated")
		}
		path := filepath.Join(root, name)
		if err := os.Mkdir(path, 0o700); err != nil {
			if errors.Is(err, fs.ErrExist) {
				lastErr = err
				continue
			}
			return "", Marker{}, apperr.IO(op, "the state directory could not be created", err)
		}
		marker := Marker{
			Version:   MarkerVersion,
			Kind:      kind,
			ID:        id,
			Name:      name,
			OwnerPID:  os.Getpid(),
			CreatedAt: now.UTC(),
		}
		if err := WriteMarker(path, marker); err != nil {
			// A directory with no marker is not NetForge's as far as any later
			// run is concerned, so a marker that cannot be written must not be
			// left behind as something to reason about.
			_ = os.Remove(path)
			return "", Marker{}, err
		}
		return path, marker, nil
	}
	return "", Marker{}, apperr.IO(op, "a state directory name could not be claimed", lastErr)
}

// generated returns a fresh directory name for a kind.
func generated(kind Kind) (string, error) {
	const op = "reconcile.generated"
	raw := make([]byte, nameBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	name := prefix(kind) + hex.EncodeToString(raw)
	if err := namespace.ValidateName(op, "name", name); err != nil {
		return "", err
	}
	return name, nil
}

// MarkerPath is where the marker of a state directory lives.
func MarkerPath(dir string) string { return filepath.Join(dir, MarkerFile) }

// WriteMarker writes a marker into a state directory atomically, so a reader
// never sees a marker that is half a marker.
func WriteMarker(dir string, marker Marker) error {
	const op = "reconcile.WriteMarker"
	data, err := json.Marshal(marker)
	if err != nil {
		return apperr.IO(op, "the ownership marker could not be encoded", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return apperr.IO(op, "the state directory "+dir+" could not be prepared", err)
	}
	temporary, err := os.CreateTemp(dir, MarkerFile+".*.tmp")
	if err != nil {
		return apperr.IO(op, "the ownership marker could not be written", err)
	}
	// The rename is the commit. Everything before it can fail without leaving a
	// file a later run would try to trust.
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		_ = os.Remove(temporary.Name())
		return apperr.IO(op, "the ownership marker could not be written", err)
	}
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		_ = os.Remove(temporary.Name())
		return apperr.IO(op, "the ownership marker could not be restricted", err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporary.Name())
		return apperr.IO(op, "the ownership marker could not be closed", err)
	}
	if err := os.Rename(temporary.Name(), MarkerPath(dir)); err != nil {
		_ = os.Remove(temporary.Name())
		return apperr.IO(op, "the ownership marker could not be published", err)
	}
	return nil
}

// ReadMarker returns the marker of a state directory.
//
// It refuses anything it cannot positively identify: a marker that does not
// parse, one written in another format, one naming a kind this build does not
// know, one whose recorded name is not the directory it sits in, and one whose
// identifier is not a name a namespace could carry. Every one of those is a
// directory this build did not create and will not remove.
func ReadMarker(dir string) (Marker, error) {
	const op = "reconcile.ReadMarker"
	name := filepath.Base(dir)
	kind, generated := KindOf(name)
	if !generated {
		return Marker{}, apperr.Permission(op,
			"the directory name "+strconv.Quote(name)+" is not one NetForge generates", nil)
	}
	data, err := os.ReadFile(MarkerPath(dir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Marker{}, apperr.NotFound(op, "there is no ownership marker in "+name, err)
		}
		return Marker{}, apperr.IO(op, "the ownership marker in "+name+" could not be read", err)
	}
	var marker Marker
	if err := json.Unmarshal(data, &marker); err != nil {
		return Marker{}, apperr.Permission(op, "the ownership marker in "+name+" is not a marker", err)
	}
	switch {
	case marker.Version != MarkerVersion:
		return Marker{}, apperr.Permission(op, "the ownership marker in "+name+" is format version "+
			strconv.Itoa(marker.Version)+", this build reads "+strconv.Itoa(MarkerVersion), nil)
	case marker.Name != name:
		return Marker{}, apperr.Permission(op, "the ownership marker in "+name+
			" claims to belong to "+strconv.Quote(marker.Name), nil)
	case marker.Kind != kind:
		return Marker{}, apperr.Permission(op, "the ownership marker in "+name+" claims to be "+
			strconv.Quote(string(marker.Kind))+" rather than the "+string(kind)+" its name declares", nil)
	case marker.OwnerPID <= 0:
		return Marker{}, apperr.Permission(op, "the ownership marker in "+name+" names no owner process", nil)
	case marker.CreatedAt.IsZero():
		return Marker{}, apperr.Permission(op, "the ownership marker in "+name+" was never dated", nil)
	}
	if err := namespace.ValidateName(op, "marker.id", marker.ID); err != nil {
		return Marker{}, apperr.Permission(op, "the ownership marker in "+name+
			" carries an identifier a namespace could not carry", err)
	}
	return marker, nil
}

// Entry is one thing under the state root.
type Entry struct {
	// Name is the directory's own name.
	Name string
	// Path is where it is.
	Path string
	// Kind is what it is for, and is empty when it could not be identified.
	Kind Kind
	// ID is the identifier it carries, and is empty when it could not be
	// identified.
	ID string
	// Owner is the process that created it, and is zero when unknown.
	Owner int
	// Created is when it was created, from its marker or from the filesystem.
	Created time.Time
	// Age is how long ago that was.
	Age time.Duration
	// Identified reports whether this build positively identified the entry as
	// NetForge's own. An entry that is not identified is never removed.
	Identified bool
	// InUse reports whether the process that owns the entry is still running.
	InUse bool
	// Reason is why the entry is not identified, or why it would not be removed.
	Reason string
}

// Describe renders an entry as one line, naming what it is and how old it is.
// An entry that could not be identified is named as unidentified and nothing
// more: this listing is a claim about what NetForge owns, and it does not make
// claims about anything else.
func (e Entry) Describe() string {
	if !e.Identified {
		// An unidentified entry carries no identifier at all, not a blank one, so
		// the reason sits where the identifier would be rather than after an
		// empty column that could be misread as an empty value.
		return pad(e.Name, listingNameWidth) + pad("unidentified", listingKindWidth) +
			age(e.Created, e.Age) + e.Reason
	}
	state := "idle"
	if e.InUse {
		state = "in use by pid " + strconv.Itoa(e.Owner)
	}
	return pad(e.Name, listingNameWidth) + pad(string(e.Kind), listingKindWidth) +
		pad(e.ID, listingIDWidth) + age(e.Created, e.Age) + state
}

// The listing's column widths. A router's generated name is longer than a
// laboratory's, and a column too narrow for it would run the kind into the name —
// which is not a cosmetic problem here, because the kind is the field that tells
// the two apart.
const (
	listingNameWidth = 32
	listingKindWidth = 14
	listingIDWidth   = 22
	listingAgeWidth  = 16
)

// Identified returns the entries a listing positively identified, which is the
// only set a reclaim may act on.
func Identified(entries []Entry) []Entry {
	var out []Entry
	for _, entry := range entries {
		if entry.Identified {
			out = append(out, entry)
		}
	}
	return out
}

// Scan lists the state root and classifies everything in it.
//
// Nothing is removed and nothing is created. An entry this build cannot
// positively identify is listed with its reason and no identifier, so an
// operator can see that something is there without NetForge having claimed it.
func Scan(root string, now time.Time) ([]Entry, error) {
	const op = "reconcile.Scan"
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// A state root that does not exist is a host no run has left
			// anything in, which is an empty listing rather than a failure.
			return nil, nil
		}
		return nil, apperr.IO(op, "the state root "+root+" could not be listed", err)
	}
	out := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, inspect(filepath.Join(root, entry.Name()), entry, now))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// inspect classifies one entry under the state root.
func inspect(path string, entry fs.DirEntry, now time.Time) Entry {
	found := Entry{Name: entry.Name(), Path: path}
	// The age is taken from the filesystem first so that an entry which cannot be
	// identified still carries one: a reader deciding whether to look at an
	// unfamiliar directory needs to know how long it has been sitting there.
	if info, err := entry.Info(); err == nil {
		found.Created = info.ModTime()
		found.Age = now.Sub(info.ModTime())
	}
	if strings.HasPrefix(entry.Name(), ".") {
		found.Reason = "not a state directory"
		return found
	}
	if !entry.IsDir() {
		// A symlink is not a directory either, whatever it points at, and this
		// is the case where the distinction matters: following one would take a
		// removal somewhere the name does not say.
		if entry.Type()&fs.ModeSymlink != 0 {
			found.Reason = "a symlink is not a state directory"
			return found
		}
		found.Reason = "not a directory"
		return found
	}
	if _, generated := KindOf(entry.Name()); !generated {
		found.Reason = "the name is not one NetForge generates"
		return found
	}
	marker, err := ReadMarker(path)
	if err != nil {
		found.Reason = reasonOf(err)
		return found
	}
	found.Kind = marker.Kind
	found.ID = marker.ID
	found.Owner = marker.OwnerPID
	found.Created = marker.CreatedAt
	found.Age = now.Sub(marker.CreatedAt)
	found.Identified = true
	found.InUse = alive(marker.OwnerPID)
	if found.InUse {
		found.Reason = "the process that created it, pid " + strconv.Itoa(marker.OwnerPID) + ", is still running"
	}
	return found
}

// InUse reports whether the process that owns an entry is still running.
//
// A directory whose owner is running is a live laboratory's, and removing it
// would pull its state out from under a run that is still writing to it. The
// check is deliberately one-sided: a process that died and whose identifier has
// been reused makes a directory look in use, which costs an operator one manual
// removal, while the opposite mistake would delete a running laboratory's state.
func InUse(entry Entry) bool {
	if !entry.Identified {
		return false
	}
	return entry.InUse
}

// alive reports whether a process is still running.
//
// It asks /proc rather than signalling the process. A signal would be a way to
// ask a question about a process from code that has no business signalling one,
// and the repository's security suite holds that boundary for exactly this
// reason: a reconciler that can signal a process can be argued into doing so.
// Reading the process's own directory is the same answer with no such power
// behind it.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.Stat(procRoot + "/" + strconv.Itoa(pid))
	if err == nil {
		return true
	}
	// A host with no /proc cannot answer the question. That is reported as
	// running, because the two errors are not symmetric: refusing to remove a
	// directory that was already gone costs an operator one manual removal, and
	// removing a live laboratory's state costs the laboratory.
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	return true
}

// procRoot is where the kernel describes running processes. It is a constant
// rather than a flag because it is the kernel's own path, not a choice: a
// reconciler asked to look somewhere else for liveness would be a reconciler
// whose answer to "is this in use" came from the caller.
const procRoot = "/proc"

// reasonOf turns a refusal into the sentence a listing prints.
func reasonOf(err error) string {
	var appErr *apperr.Error
	if errors.As(err, &appErr) && appErr.Msg != "" {
		return appErr.Msg
	}
	return err.Error()
}

// pad renders a value at a fixed width, padding on the right. A value longer than
// the width is followed by a single space rather than nothing, so a name another
// program left in a shared state root does not run its reason into the next
// column and make the row unreadable.
func pad(value string, width int) string {
	for len(value) < width {
		value += " "
	}
	if len(value) == width {
		return value + " "
	}
	return value
}

func age(created time.Time, value time.Duration) string {
	if created.IsZero() {
		return pad("unknown age", listingAgeWidth)
	}
	return pad(value.Round(time.Second).String(), listingAgeWidth)
}
