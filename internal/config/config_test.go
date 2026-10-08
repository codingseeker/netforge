package config

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/codingseeker/netforge/internal/apperr"
)

func TestLoadValidMinimal(t *testing.T) {
	cfg, err := Load(filepath.Join("testdata", "valid-minimal.yaml"))
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}

	if cfg.Name != "minimal-lab" {
		t.Fatalf("Name = %q, want %q", cfg.Name, "minimal-lab")
	}
	if got := cfg.Network(); got != "10.42.0.0/16" {
		t.Fatalf("Network() = %q, want %q", got, "10.42.0.0/16")
	}
	wantNamespaces := []string{"client", "router", "server"}
	if len(cfg.NamespaceNames()) != len(wantNamespaces) {
		t.Fatalf("NamespaceNames() = %v, want %v", cfg.NamespaceNames(), wantNamespaces)
	}
	for i, want := range wantNamespaces {
		if cfg.NamespaceNames()[i] != want {
			t.Fatalf("NamespaceNames() = %v, want %v", cfg.NamespaceNames(), wantNamespaces)
		}
	}
	if cfg.Seed != nil {
		t.Fatalf("Seed = %v, want nil", *cfg.Seed)
	}
	if cfg.Limits != DefaultLimits() {
		t.Fatalf("Limits = %+v, want defaults %+v", cfg.Limits, DefaultLimits())
	}
	if !strings.HasPrefix(cfg.ExperimentID(), "nf-minimal-lab-") {
		t.Fatalf("ExperimentID() = %q, want prefix %q", cfg.ExperimentID(), "nf-minimal-lab-")
	}
}

func TestLoadValidFull(t *testing.T) {
	cfg, err := Load(filepath.Join("testdata", "valid-full.yaml"))
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}

	if cfg.Seed == nil {
		t.Fatal("Seed = nil, want 123456")
	}
	if *cfg.Seed != 123456 {
		t.Fatalf("Seed = %d, want 123456", *cfg.Seed)
	}
	want := Limits{
		MaxNamespaces:       10,
		MaxInterfaces:       30,
		MaxDuration:         Duration(10 * time.Minute),
		MaxBandwidth:        Gbps,
		MaxPacketRate:       100_000,
		MaxConcurrentProbes: 100,
	}
	if cfg.Limits != want {
		t.Fatalf("Limits = %+v, want %+v", cfg.Limits, want)
	}
	if got := cfg.Limits.MaxBandwidth.BitsPerSecond(); got != 1_000_000_000 {
		t.Fatalf("MaxBandwidth = %d bits/s, want 1000000000", got)
	}
}

func TestLoadRejectsMalformedInput(t *testing.T) {
	tests := []struct {
		file     string
		wantKind apperr.Kind
		contains string
	}{
		{file: "malformed.yaml", wantKind: apperr.KindInvalidConfig, contains: "cannot decode configuration"},
		{file: "empty.yaml", wantKind: apperr.KindInvalidConfig, contains: "empty"},
		{file: "unknown-field.yaml", wantKind: apperr.KindInvalidConfig, contains: "allow_host_network"},
		{file: "unknown-nested-field.yaml", wantKind: apperr.KindInvalidConfig, contains: "gateway"},
		{file: "duplicate-key.yaml", wantKind: apperr.KindInvalidConfig},
		{file: "multiple-documents.yaml", wantKind: apperr.KindInvalidConfig, contains: "exactly one YAML document"},
		{file: "wrong-type.yaml", wantKind: apperr.KindInvalidConfig, contains: "cannot decode configuration"},
		{file: "negative-duration.yaml", wantKind: apperr.KindInvalidArgument, contains: "must not be negative"},
		{file: "unitless-duration.yaml", wantKind: apperr.KindInvalidConfig, contains: "must be a quoted string"},
		{file: "invalid-cidr.yaml", wantKind: apperr.KindInvalidArgument, contains: "not a valid CIDR"},
		{file: "host-bits-cidr.yaml", wantKind: apperr.KindInvalidArgument, contains: "host bits set"},
		{file: "default-route-cidr.yaml", wantKind: apperr.KindInvalidArgument, contains: "not a default route"},
		{file: "ipv6-cidr.yaml", wantKind: apperr.KindUnsupported, contains: "IPv4"},
		{file: "invalid-namespace.yaml", wantKind: apperr.KindInvalidArgument, contains: "lowercase letters"},
		{file: "duplicate-namespace.yaml", wantKind: apperr.KindValidation, contains: "duplicates namespace"},
		{file: "invalid-name.yaml", wantKind: apperr.KindInvalidArgument, contains: "lowercase letters"},
		{file: "scope-exceeds-limits.yaml", wantKind: apperr.KindLimitExceeded, contains: "exceeds limits.max_namespaces"},
	}

	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			cfg, err := Load(filepath.Join("testdata", tc.file))
			if err == nil {
				t.Fatalf("Load(%s) = %+v, want error", tc.file, cfg)
			}
			if !apperr.IsKind(err, tc.wantKind) {
				t.Fatalf("Load(%s) = %v, want kind %q", tc.file, err, tc.wantKind)
			}
			if tc.contains != "" && !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("Load(%s) = %v, want message containing %q", tc.file, err, tc.contains)
			}
		})
	}
}

func TestLoadRejectsExcessiveLimits(t *testing.T) {
	_, err := Load(filepath.Join("testdata", "excessive-limits.yaml"))
	if err == nil {
		t.Fatal("Load(excessive-limits.yaml) = nil error, want error")
	}
	if !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Fatalf("Load(excessive-limits.yaml) = %v, want limit_exceeded", err)
	}
	for _, want := range []string{
		"limits.max_namespaces",
		"limits.max_interfaces",
		"limits.max_duration",
		"limits.max_bandwidth",
		"limits.max_packet_rate",
		"limits.max_concurrent_probes",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %v does not report %q", err, want)
		}
	}
}

func TestParseEnforcesSizeLimit(t *testing.T) {
	document := "name: size-limit\n"
	oversized := document + strings.Repeat("# padding padding padding padding padding\n", (MaxConfigBytes/len("# padding padding padding padding padding\n"))+16)

	if len(oversized) <= MaxConfigBytes {
		t.Fatalf("test document is %d bytes, want more than %d", len(oversized), MaxConfigBytes)
	}

	_, err := Parse([]byte(oversized))
	if !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Fatalf("Parse(oversized) = %v, want limit_exceeded", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(MaxConfigBytes)) {
		t.Fatalf("Parse(oversized) = %v, want the limit in the message", err)
	}
}

func TestParseReaderEnforcesSizeLimit(t *testing.T) {
	padding := strings.Repeat("# "+strings.Repeat("x", 200)+"\n", (MaxConfigBytes/203)+16)
	_, err := ParseReader(strings.NewReader("name: reader-limit\n"+padding), "config.ParseReader")
	if !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Fatalf("ParseReader(oversized) = %v, want limit_exceeded", err)
	}
}

func TestParseReaderAcceptsDocumentAtLimit(t *testing.T) {
	header := "name: at-limit\n\nscope:\n  network: 10.42.0.0/16\n  namespaces:\n    - client\n\n#"
	document := header + strings.Repeat("x", MaxConfigBytes-len(header)-1) + "\n"
	if len(document) != MaxConfigBytes {
		t.Fatalf("test document is %d bytes, want %d", len(document), MaxConfigBytes)
	}
	if _, err := Parse([]byte(document)); err != nil {
		t.Fatalf("Parse(at limit) = %v, want nil", err)
	}
}

func TestParseRejectsEmptyInput(t *testing.T) {
	if _, err := Parse(nil); err == nil {
		t.Fatal("Parse(nil) = nil error, want error")
	}
	if _, err := Parse([]byte("")); err == nil {
		t.Fatal("Parse(empty) = nil error, want error")
	}
}

func TestParseRejectsNonMappingRoot(t *testing.T) {
	for _, document := range []string{"- client\n- router\n", "just-a-string\n", "42\n"} {
		if _, err := Parse([]byte(document)); err == nil {
			t.Fatalf("Parse(%q) = nil error, want error", document)
		}
	}
}

func TestParseRejectsControlCharacter(t *testing.T) {
	document := "name: null-byte\n\nscope:\n  network: 10.42.0.0/16\n  namespaces:\n    - \"cl\x00ient\"\n"
	_, err := Parse([]byte(document))
	if !apperr.IsKind(err, apperr.KindInvalidConfig) {
		t.Fatalf("Parse(control character) = %v, want invalid_config", err)
	}
}

func TestValidateRejectsControlCharacter(t *testing.T) {
	cfg := Config{
		Name:   "null-byte",
		Scope:  Scope{Network: "10.42.0.0/16", Namespaces: []string{"cl\x00ient"}},
		Limits: DefaultLimits(),
	}
	if err := cfg.Validate(); !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("Validate() = %v, want invalid_argument", err)
	}
}

func TestLoadValidatesPath(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "empty", path: ""},
		{name: "nul byte", path: "net\x00forge.yaml"},
		{name: "too long", path: strings.Repeat("a", MaxPathLength+1)},
		{name: "directory", path: "testdata"},
		{name: "device", path: os.DevNull},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(tc.path); err == nil {
				t.Fatalf("Load(%q) = nil error, want error", tc.path)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join("testdata", "does-not-exist.yaml"))
	if !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("Load(missing) = %v, want not_found", err)
	}
}

func TestLoadRejectsOversizeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized.yaml")
	if err := os.WriteFile(path, []byte("name: big\n"+strings.Repeat("#\n", MaxConfigBytes)), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}

	_, err := Load(path)
	if !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Fatalf("Load(oversized file) = %v, want limit_exceeded", err)
	}
}

func TestLoadSymlinkToDirectoryIsRejected(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join("testdata"), link); err != nil {
		t.Fatalf("Symlink() = %v, want nil", err)
	}
	if _, err := Load(link); err == nil {
		t.Fatal("Load(symlink to directory) = nil error, want error")
	}
}

func TestLoadClassifiesPermissionFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, permission bits are not enforced")
	}

	path := filepath.Join(t.TempDir(), "unreadable.yaml")
	if err := os.WriteFile(path, []byte("name: locked\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("Chmod() = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatalf("Chmod(restore) = %v, want nil", err)
		}
	})

	_, err := Load(path)
	if !apperr.IsKind(err, apperr.KindPermission) {
		t.Fatalf("Load(unreadable) = %v, want permission_denied", err)
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("Load(unreadable) does not preserve the cause: %v", err)
	}
}

func TestParseReaderClassifiesReadFailure(t *testing.T) {
	_, err := ParseReader(failingReader{err: os.ErrPermission}, "config.Load")
	if !apperr.IsKind(err, apperr.KindPermission) {
		t.Fatalf("ParseReader(permission) = %v, want permission_denied", err)
	}

	_, err = ParseReader(failingReader{err: errors.New("device failure")}, "config.Load")
	if !apperr.IsKind(err, apperr.KindIO) {
		t.Fatalf("ParseReader(other) = %v, want io", err)
	}
}

func TestLoadFromReadsStdin(t *testing.T) {
	document := "name: streamed\nscope:\n  network: 10.42.0.0/16\n  namespaces:\n    - client\n"

	cfg, err := LoadFrom(StdinPath, strings.NewReader(document))
	if err != nil {
		t.Fatalf("LoadFrom(-) = %v, want nil", err)
	}
	if cfg.Name != "streamed" {
		t.Fatalf("Name = %q, want %q", cfg.Name, "streamed")
	}
}

func TestLoadPropagateStdinFailure(t *testing.T) {
	_, err := LoadFrom(StdinPath, failingReader{err: errRead})
	if !apperr.IsKind(err, apperr.KindIO) {
		t.Fatalf("LoadFrom(-, failing) = %v, want io", err)
	}
}

func TestLoadFromRejectsMissingStdin(t *testing.T) {
	_, err := LoadFrom(StdinPath, nil)
	if !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("LoadFrom(-, nil) = %v, want invalid_argument", err)
	}
}

func TestLoadClassifiesUnexpectedOpenFailure(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(file, []byte("name: x\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}

	_, err := Load(filepath.Join(file, "nested.yaml"))
	if !apperr.IsKind(err, apperr.KindIO) {
		t.Fatalf("Load(nested) = %v, want io", err)
	}
}

func TestLoadRejectsUnreadableTrailingDocument(t *testing.T) {
	document := "name: x\nscope:\n  network: 10.42.0.0/16\n  namespaces:\n    - client\n---\nkey: [1, 2\n"

	_, err := Parse([]byte(document))
	if !apperr.IsKind(err, apperr.KindInvalidConfig) {
		t.Fatalf("Parse(broken trailing document) = %v, want invalid_config", err)
	}
	if !strings.Contains(err.Error(), "cannot decode configuration") {
		t.Fatalf("Parse(broken trailing document) = %q, want a decode failure", err)
	}
}

func TestParseRejectsCommentOnlyDocument(t *testing.T) {
	_, err := Parse([]byte("# only a comment\n"))
	if !apperr.IsKind(err, apperr.KindInvalidConfig) {
		t.Fatalf("Parse(comment only) = %v, want invalid_config", err)
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Fatalf("Parse(comment only) = %q, want an empty document error", err)
	}
}

func TestLoadHasNoSideEffects(t *testing.T) {
	before := snapshotDir(t, "testdata")

	if _, err := Load(filepath.Join("testdata", "valid-minimal.yaml")); err != nil {
		t.Fatalf("Load(valid) = %v, want nil", err)
	}
	if _, err := Load(filepath.Join("testdata", "malformed.yaml")); err == nil {
		t.Fatal("Load(malformed) = nil error, want error")
	}

	after := snapshotDir(t, "testdata")
	if len(before) != len(after) {
		t.Fatalf("testdata file count changed from %d to %d", len(before), len(after))
	}
	for name, size := range before {
		if after[name] != size {
			t.Fatalf("testdata file %q changed from %d to %d bytes", name, size, after[name])
		}
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	original, err := Load(filepath.Join("testdata", "valid-full.yaml"))
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}

	data, err := Marshal(*original)
	if err != nil {
		t.Fatalf("Marshal() = %v, want nil", err)
	}

	restored, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse(Marshal()) = %v, want nil\n%s", err, data)
	}
	if restored.Name != original.Name {
		t.Fatalf("Name = %q, want %q", restored.Name, original.Name)
	}
	if restored.Limits != original.Limits {
		t.Fatalf("Limits = %+v, want %+v", restored.Limits, original.Limits)
	}
	if restored.Seed == nil || original.Seed == nil || *restored.Seed != *original.Seed {
		t.Fatalf("Seed = %v, want %v", restored.Seed, original.Seed)
	}
	if !strings.Contains(string(data), "max_duration: 10m0s") {
		t.Fatalf("marshalled document does not use duration text:\n%s", data)
	}
	if !strings.Contains(string(data), "max_bandwidth: 1Gbps") {
		t.Fatalf("marshalled document does not use bandwidth text:\n%s", data)
	}
}

func TestMarshalIsDeterministic(t *testing.T) {
	cfg, err := Load(filepath.Join("testdata", "valid-full.yaml"))
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}

	first, err := Marshal(*cfg)
	if err != nil {
		t.Fatalf("Marshal() = %v, want nil", err)
	}
	for i := 0; i < 20; i++ {
		next, err := Marshal(*cfg)
		if err != nil {
			t.Fatalf("Marshal() = %v, want nil", err)
		}
		if string(next) != string(first) {
			t.Fatalf("Marshal() is not deterministic:\n%s\n---\n%s", first, next)
		}
	}
}

func TestParseIsDeterministic(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "valid-full.yaml"))
	if err != nil {
		t.Fatalf("ReadFile() = %v, want nil", err)
	}

	reference, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse() = %v, want nil", err)
	}
	for i := 0; i < 20; i++ {
		next, err := Parse(data)
		if err != nil {
			t.Fatalf("Parse() = %v, want nil", err)
		}
		if next.Name != reference.Name || next.Limits != reference.Limits {
			t.Fatalf("Parse() is not deterministic: %+v vs %+v", next, reference)
		}
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	cfg := Config{
		Name:  "Bad Name",
		Scope: Scope{Network: "not-a-cidr", Namespaces: []string{"client", "client", "BAD"}},
		Limits: Limits{
			MaxNamespaces:       -1,
			MaxInterfaces:       0,
			MaxDuration:         Duration(-time.Second),
			MaxBandwidth:        0,
			MaxPacketRate:       0,
			MaxConcurrentProbes: 0,
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want error")
	}
	for _, want := range []string{
		"name",
		"not a valid CIDR",
		"duplicates namespace",
		"lowercase letters",
		"max_namespaces",
		"max_interfaces",
		"max_duration",
		"max_bandwidth",
		"max_packet_rate",
		"max_concurrent_probes",
		"10 configuration problems",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Validate() = %v, want message containing %q", err, want)
		}
	}
}

func TestValidateAcceptsMinimalConfig(t *testing.T) {
	cfg := Config{
		Name:   "valid",
		Scope:  Scope{Network: "10.42.0.0/16", Namespaces: []string{"client"}},
		Limits: DefaultLimits(),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestValidateAcceptsMaximalNames(t *testing.T) {
	cfg := Config{
		Name:  strings.Repeat("a", MaxNameLength),
		Scope: Scope{Network: "10.0.0.0/8", Namespaces: []string{"a", "b-1", "c9"}},
		Limits: Limits{
			MaxNamespaces:       CeilingNamespaces,
			MaxInterfaces:       CeilingInterfaces,
			MaxDuration:         Duration(CeilingDuration),
			MaxBandwidth:        CeilingBandwidth,
			MaxPacketRate:       CeilingPacketRate,
			MaxConcurrentProbes: CeilingConcurrentProbes,
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestValidateRejectsJustOverCeilings(t *testing.T) {
	cfg := Config{
		Name:  "over-ceiling",
		Scope: Scope{Network: "10.0.0.0/8", Namespaces: []string{"a"}},
		Limits: Limits{
			MaxNamespaces:       CeilingNamespaces + 1,
			MaxInterfaces:       CeilingInterfaces + 1,
			MaxDuration:         Duration(CeilingDuration + time.Second),
			MaxBandwidth:        CeilingBandwidth + Gbps,
			MaxPacketRate:       CeilingPacketRate + 1,
			MaxConcurrentProbes: CeilingConcurrentProbes + 1,
		},
	}
	err := cfg.Validate()
	if !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Fatalf("Validate() = %v, want limit_exceeded", err)
	}
	if !strings.Contains(err.Error(), "6 configuration problems") {
		t.Fatalf("Validate() = %v, want every ceiling reported", err)
	}
}

func TestValidateRejectsTooManyNamespaces(t *testing.T) {
	namespaces := make([]string, 0, CeilingNamespaces+1)
	for i := 0; i <= CeilingNamespaces; i++ {
		namespaces = append(namespaces, fmt.Sprintf("ns%d", i))
	}
	cfg := Config{
		Name:   "too-many",
		Scope:  Scope{Network: "10.0.0.0/8", Namespaces: namespaces},
		Limits: Limits{MaxNamespaces: CeilingNamespaces, MaxInterfaces: 1, MaxDuration: Duration(time.Minute), MaxBandwidth: Gbps, MaxPacketRate: 1, MaxConcurrentProbes: 1},
	}
	if err := cfg.Validate(); !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Fatalf("Validate() = %v, want limit_exceeded", err)
	}
}

func TestValidateRejectsMissingScope(t *testing.T) {
	cfg := Config{Name: "no-scope", Limits: DefaultLimits()}
	err := cfg.Validate()
	if !apperr.IsKind(err, apperr.KindInvalidArgument) {
		t.Fatalf("Validate() = %v, want invalid_argument", err)
	}
	if !strings.Contains(err.Error(), "scope.network is required") {
		t.Fatalf("Validate() = %v, want missing network report", err)
	}
	if !strings.Contains(err.Error(), "at least one namespace") {
		t.Fatalf("Validate() = %v, want missing namespaces report", err)
	}
}

func TestWithDefaults(t *testing.T) {
	empty := Limits{}
	if got := empty.WithDefaults(); got != DefaultLimits() {
		t.Fatalf("WithDefaults() = %+v, want %+v", got, DefaultLimits())
	}

	explicit := Limits{
		MaxNamespaces:       1,
		MaxInterfaces:       2,
		MaxDuration:         Duration(time.Second),
		MaxBandwidth:        1 * Mbps,
		MaxPacketRate:       3,
		MaxConcurrentProbes: 4,
	}
	if got := explicit.WithDefaults(); got != explicit {
		t.Fatalf("WithDefaults() = %+v, want %+v", got, explicit)
	}
}

func TestExperimentIDIsDeterministic(t *testing.T) {
	scope := Scope{Network: "10.42.0.0/16", Namespaces: []string{"client", "router"}}
	seed := uint64(1234)
	limits := DefaultLimits()

	first := ExperimentID("lab", scope, &seed, limits)
	for i := 0; i < 20; i++ {
		if got := ExperimentID("lab", scope, &seed, limits); got != first {
			t.Fatalf("ExperimentID() = %q, want the stable value %q", got, first)
		}
	}
	if !strings.HasPrefix(first, "nf-lab-") {
		t.Fatalf("ExperimentID() = %q, want prefix %q", first, "nf-lab-")
	}
	if len(first) != len("nf-lab-")+8 {
		t.Fatalf("ExperimentID() = %q, want an 8 character digest suffix", first)
	}
}

func TestExperimentIDChangesConfig(t *testing.T) {
	scope := Scope{Network: "10.42.0.0/16", Namespaces: []string{"client"}}
	seed := uint64(1234)
	limits := DefaultLimits()
	otherSeed := uint64(1235)
	otherLimits := DefaultLimits()
	otherLimits.MaxPacketRate = 1

	tests := []struct {
		name  string
		left  string
		right string
	}{
		{name: "name", left: ExperimentID("lab", scope, &seed, limits), right: ExperimentID("other", scope, &seed, limits)},
		{name: "network", left: ExperimentID("lab", scope, &seed, limits), right: ExperimentID("lab", Scope{Network: "10.43.0.0/16", Namespaces: scope.Namespaces}, &seed, limits)},
		{name: "namespaces", left: ExperimentID("lab", scope, &seed, limits), right: ExperimentID("lab", Scope{Network: scope.Network, Namespaces: []string{"client", "server"}}, &seed, limits)},
		{name: "seed", left: ExperimentID("lab", scope, &seed, limits), right: ExperimentID("lab", scope, &otherSeed, limits)},
		{name: "seed absence", left: ExperimentID("lab", scope, &seed, limits), right: ExperimentID("lab", scope, nil, limits)},
		{name: "limits", left: ExperimentID("lab", scope, &seed, limits), right: ExperimentID("lab", scope, &seed, otherLimits)},
	}

	for _, tc := range tests {
		if tc.left == tc.right {
			t.Fatalf("changing the %s did not change the experiment id", tc.name)
		}
	}
}

func TestExperimentIDForConfig(t *testing.T) {
	cfg, err := Load(filepath.Join("testdata", "valid-full.yaml"))
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}

	again, err := Load(filepath.Join("testdata", "valid-full.yaml"))
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	if cfg.ExperimentID() != again.ExperimentID() {
		t.Fatalf("ExperimentID() = %q, want a stable value across loads", cfg.ExperimentID())
	}
	if !strings.HasPrefix(cfg.ExperimentID(), "nf-cascading-failure-") {
		t.Fatalf("ExperimentID() = %q, want prefix %q", cfg.ExperimentID(), "nf-cascading-failure-")
	}
}

func TestParseNetwork(t *testing.T) {
	valid := map[string]string{
		"10.42.0.0/16":   "10.42.0.0/16",
		"10.0.0.0/8":     "10.0.0.0/8",
		"192.168.1.0/24": "192.168.1.0/24",
		"10.42.0.1/32":   "10.42.0.1/32",
	}
	for in, want := range valid {
		prefix, err := ParseNetwork(in)
		if err != nil {
			t.Fatalf("ParseNetwork(%q) = %v, want nil", in, err)
		}
		if prefix.String() != want {
			t.Fatalf("ParseNetwork(%q) = %q, want %q", in, prefix, want)
		}
		if got := prefix.Addr(); !got.Is4() {
			t.Fatalf("ParseNetwork(%q).Addr() = %v, want IPv4", in, got)
		}
	}

	invalid := []string{"", "10.42.0.0", "10.42.0.0/33", "10.42.0.0/-1", "not-a-cidr", "0.0.0.0/0", "10.42.0.5/16", "10.42.0.0/16/24"}
	for _, in := range invalid {
		if prefix, err := ParseNetwork(in); err == nil {
			t.Fatalf("ParseNetwork(%q) = %v, want error", in, prefix)
		}
	}
}

func TestParseNetworkAcceptsEveryIPv4Mask(t *testing.T) {
	for bits := 1; bits <= 32; bits++ {
		value := netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 42, 0, 0}), bits).Masked().String()
		if _, err := ParseNetwork(value); err != nil {
			t.Fatalf("ParseNetwork(%q) = %v, want nil", value, err)
		}
	}
}

func snapshotDir(t *testing.T, dir string) map[string]int64 {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q) = %v, want nil", dir, err)
	}
	snapshot := make(map[string]int64, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatalf("Info(%q) = %v, want nil", entry.Name(), err)
		}
		snapshot[entry.Name()] = info.Size()
	}
	return snapshot
}

func TestErrorsPreserveYAMLCause(t *testing.T) {
	document := "name: x\nscope:\n  network: 10.42.0.0/16\n  namespaces:\n    - client\n" +
		"faults: not-a-list\n"
	_, err := Parse([]byte(document))
	if !apperr.IsKind(err, apperr.KindInvalidConfig) {
		t.Fatalf("Parse() = %v, want invalid_config", err)
	}
	var typeErr *yaml.TypeError
	if !errors.As(err, &typeErr) {
		t.Fatalf("Parse() = %v, want the YAML cause to be preserved", err)
	}
}
