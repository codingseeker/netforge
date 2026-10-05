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

type Kind string

const (
	KindLaboratory Kind = "laboratory"

	KindRouter Kind = "router"
)

const MarkerFile = "netforge-state.json"

const MarkerVersion = 1

const nameBytes = 6

func KindOf(name string) (Kind, bool) {
	for _, kind := range Kinds() {
		if generatedName(kind, name) {
			return kind, true
		}
	}
	return "", false
}

func Kinds() []Kind { return []Kind{KindLaboratory, KindRouter} }

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

func generatedName(kind Kind, name string) bool {
	if prefix(kind) == "" || !strings.HasPrefix(name, prefix(kind)) {
		return false
	}
	if err := namespace.ValidateName("reconcile", "name", name); err != nil {
		return false
	}
	return len(name) > len(prefix(kind))
}

type Marker struct {
	Version int `json:"version"`

	Kind Kind `json:"kind"`

	ID string `json:"id"`

	Name string `json:"name"`

	OwnerPID int `json:"owner_pid"`

	CreatedAt time.Time `json:"created_at"`
}

func StateRoot() string {
	if root := strings.TrimSpace(os.Getenv(StateRootEnv)); root != "" {
		return root
	}
	return os.TempDir()
}

const StateRootEnv = "NETFORGE_STATE_ROOT"

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

			_ = os.Remove(path)
			return "", Marker{}, err
		}
		return path, marker, nil
	}
	return "", Marker{}, apperr.IO(op, "a state directory name could not be claimed", lastErr)
}

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

func MarkerPath(dir string) string { return filepath.Join(dir, MarkerFile) }

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

type Entry struct {
	Name string

	Path string

	Kind Kind

	ID string

	Owner int

	Created time.Time

	Age time.Duration

	Identified bool

	InUse bool

	Reason string
}

func (e Entry) Describe() string {
	if !e.Identified {

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

const (
	listingNameWidth = 32
	listingKindWidth = 14
	listingIDWidth   = 22
	listingAgeWidth  = 16
)

func Identified(entries []Entry) []Entry {
	var out []Entry
	for _, entry := range entries {
		if entry.Identified {
			out = append(out, entry)
		}
	}
	return out
}

func Scan(root string, now time.Time) ([]Entry, error) {
	const op = "reconcile.Scan"
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {

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

func inspect(path string, entry fs.DirEntry, now time.Time) Entry {
	found := Entry{Name: entry.Name(), Path: path}

	if info, err := entry.Info(); err == nil {
		found.Created = info.ModTime()
		found.Age = now.Sub(info.ModTime())
	}
	if strings.HasPrefix(entry.Name(), ".") {
		found.Reason = "not a state directory"
		return found
	}
	if !entry.IsDir() {

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

func InUse(entry Entry) bool {
	if !entry.Identified {
		return false
	}
	return entry.InUse
}

func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.Stat(procRoot + "/" + strconv.Itoa(pid))
	if err == nil {
		return true
	}

	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	return true
}

const procRoot = "/proc"

func reasonOf(err error) string {
	var appErr *apperr.Error
	if errors.As(err, &appErr) && appErr.Msg != "" {
		return appErr.Msg
	}
	return err.Error()
}

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
