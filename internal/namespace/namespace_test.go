package namespace

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestNamespaceNameValidation(t *testing.T) {
	for _, name := range []string{"a", "nf-client0", "0", "a-b-9", strings.Repeat("a", MaxNameLength)} {
		if err := ValidateName("test", "name", name); err != nil {
			t.Errorf("ValidateName rejected %q: %v", name, err)
		}
	}
	for _, name := range []string{
		"", "-lead", "trail-", "Upper", "under_score", "dot.name", "sp ace",
		"sla/sh", "a..b", strings.Repeat("a", MaxNameLength+1),
	} {
		if err := ValidateName("test", "name", name); err == nil {
			t.Errorf("ValidateName accepted %q", name)
		}
	}
}

func TestNamespaceNameErrorMentionsTheField(t *testing.T) {
	err := ValidateName("network.Create", "name", "Bad")
	if err == nil {
		t.Fatal("ValidateName accepted Bad")
	}
	if !strings.Contains(err.Error(), "network.Create") || !strings.Contains(err.Error(), "name") {
		t.Errorf("the error %q does not carry the operation and the field", err)
	}
}

func TestProcessGoneForThisProcess(t *testing.T) {
	gone, err := processGone(os.Getpid())
	if err != nil {
		t.Fatalf("processGone reported an error for a live process: %v", err)
	}
	if gone {
		t.Error("the running process was reported as gone")
	}
}

func TestProcessGoneForAMissingProcess(t *testing.T) {
	gone, err := processGone(1 << 30)
	if err != nil {
		t.Fatalf("processGone reported an error for a missing process: %v", err)
	}
	if !gone {
		t.Error("a missing process was not reported as gone")
	}
}

func TestSpaceOperationsOnAClosedSpace(t *testing.T) {
	var space *Space
	if space.PID() != 0 || space.Alive() {
		t.Error("a nil space looks alive")
	}
	if err := space.Close(); err != nil {
		t.Errorf("closing a nil space reported %v", err)
	}
	if _, err := space.ID(); !errors.Is(err, ErrNotFound) {
		t.Errorf("the id of a nil space is %v, want ErrNotFound", err)
	}
	if _, _, err := space.Exec("true"); !errors.Is(err, ErrNotFound) {
		t.Errorf("running in a nil space is %v, want ErrNotFound", err)
	}
	if _, err := space.Write("/proc/sys/net/ipv4/ip_forward", "1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("writing through a nil space is %v, want ErrNotFound", err)
	}
}

func TestWritesAreLimitedToTheNetworkProc(t *testing.T) {
	lab := &Lab{}
	space := &Space{Name: "fake", pid: 1, lab: lab}
	for _, path := range []string{"/etc/passwd", "/proc/sys/kernel/core_pattern", "../escape", ""} {
		if _, err := space.Write(path, "1"); err == nil {
			t.Errorf("Write accepted %q", path)
		}
		if _, err := space.Read(path); err == nil {
			t.Errorf("Read accepted %q", path)
		}
	}
}

func TestSetnsIsKnownOnThisArchitecture(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("setns is a Linux facility, this is %s", runtime.GOOS)
	}
	if _, ok := setnsNumbers[runtime.GOARCH]; !ok {
		t.Fatalf("setns is not known on %s", runtime.GOARCH)
	}
	if targetNSNet != 0 {
		t.Errorf("the network namespace handle type is %#x, want 0", targetNSNet)
	}
}

func TestSetnsRejectsAClosedDescriptor(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = read.Close()
	_ = write.Close()
	if err := setns(int(read.Fd()), targetNSNet); err == nil {
		t.Error("setns accepted a closed descriptor")
	}
}

func TestProcSysNetPathStaysInsideTheNetworkSettings(t *testing.T) {
	allowed := []string{
		"/proc/sys/net/ipv4/ip_forward",
		"/proc/sys/net/ipv4/conf/all/route_localnet",
		"/proc/sys/net/ipv6/conf/all/disable_ipv6",
	}
	for _, name := range allowed {
		if _, err := procSysNetPath(name); err != nil {
			t.Errorf("%s was refused: %v", name, err)
		}
	}
	refused := []string{
		"/proc/sys/net/../../../../etc/passwd",
		"/proc/sys/net/ipv4/../../kernel/core_pattern",
		"/proc/sys/net/",
		"/proc/sys/net",
		"/etc/passwd",
		"/proc/self/environ",
		"/proc/sys/net/ipv4/ip_forward/../../../../etc/hostname",
		"relative/path",
		"",
		"/proc/sys/net/ipv4/ip_forward\x00/etc/shadow",
	}
	for _, name := range refused {
		if clean, err := procSysNetPath(name); err == nil {
			t.Errorf("%q was accepted as %q", name, clean)
		}
	}
}

func TestProcSysNetPathKeepsTheWholePrefix(t *testing.T) {
	clean, err := procSysNetPath("/proc/sys/net/ipv4/conf/nfr-c0/arp_filter")
	if err != nil {
		t.Fatal(err)
	}
	if clean != "/proc/sys/net/ipv4/conf/nfr-c0/arp_filter" {
		t.Errorf("the path was rewritten to %q", clean)
	}
}

// The agent re-checks the path it is handed rather than trusting that its parent
// cleaned it. A prefix test alone would accept /proc/sys/net/../../etc/passwd,
// which resolves outside the network settings the agent is allowed to touch.
func TestTheAgentRefusesAWriteThatLeavesTheNetworkSettings(t *testing.T) {
	escapes := []string{
		"/proc/sys/net/../../../../etc/passwd",
		"/proc/sys/net/ipv4/../../kernel/core_pattern",
		"/etc/passwd",
		"/proc/self/environ",
	}
	for _, name := range escapes {
		reply := write(request{ID: 1, Op: "write", PID: 1,
			Argv: []string{name, "0"}})
		if reply.Err == "" {
			t.Errorf("the agent accepted a write to %q", name)
		}
		if reply.Code == 0 {
			t.Errorf("the agent reported success writing to %q", name)
		}
	}
}

func TestHolderPipesAreHeldNotGarbage(t *testing.T) {
	read, feed, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = read.Close() }()
	holdPipe(feed)

	for i := 0; i < 4; i++ {
		_ = make([]byte, 1024)
	}
	runtime.GC()
	runtime.GC()

	if _, err := feed.Write([]byte("still held\n")); err != nil {
		t.Fatalf("the pipe was closed underneath the agent: %v", err)
	}
	releasePipes()
	if _, err := feed.Write([]byte("after release\n")); err == nil {
		t.Error("the pipe is still writable after release, so a holder would not see the end of it")
	}
}

func TestReleasePipesIsIdempotent(t *testing.T) {
	releasePipes()
	releasePipes()
}
