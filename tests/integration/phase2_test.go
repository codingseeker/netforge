package integration

import (
	"context"
	"errors"
	"net/netip"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/netforge/netforge/internal/iface"
	"github.com/netforge/netforge/internal/namespace"
	"github.com/netforge/netforge/internal/network"
)

func labSpec(id string) network.Spec {
	return network.Spec{ID: id, Network: "10.42.0.0/24", Clients: 1, Servers: 1, MTU: 1500}
}

func granted(spec network.Spec) network.Spec {
	plan, err := network.BuildPlan(spec.ID, spec.Network, spec.Clients, spec.Servers, spec.MTU)
	if err != nil {
		panic(err)
	}
	names := []string{plan.RouterNS}
	for _, node := range plan.Nodes {
		names = append(names, node.Namespace)
	}
	spec.Names = names
	return spec
}

func grantedSpec(id string) network.Spec {
	return granted(labSpec(id))
}

func newLab(t *testing.T, spec network.Spec) *network.Lab {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	lab, err := network.Create(ctx, granted(spec))
	if err != nil {
		t.Fatalf("network.Create: %v", err)
	}
	t.Cleanup(func() {
		if err := lab.Destroy(context.Background()); err != nil {
			t.Errorf("destroy: %v", err)
		}
	})
	return lab
}

func TestNamespaceLifecycle(t *testing.T) {
	lab := newLab(t, labSpec("lc"))

	names := lab.Names()
	if len(names) != 3 {
		t.Fatalf("the laboratory holds %d namespaces, want 3: %v", len(names), names)
	}
	plan := lab.Plan()
	for _, want := range append([]string{plan.RouterNS}, plan.Nodes[0].Namespace, plan.Nodes[1].Namespace) {
		if !contains(names, want) {
			t.Errorf("namespace %s is missing from %v", want, names)
		}
		space, err := lab.Lookup(want)
		if err != nil {
			t.Fatalf("lookup %s: %v", want, err)
		}
		if !space.Alive() {
			t.Errorf("namespace %s is not alive", want)
		}
		id, err := space.ID()
		if err != nil {
			t.Errorf("id of %s: %v", want, err)
		} else if !strings.HasPrefix(id, "net:[") {
			t.Errorf("id of %s is %q, want a net:[inode] handle", want, id)
		}
	}

	distinct := map[string]struct{}{}
	for _, name := range names {
		space, err := lab.Lookup(name)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := space.ID()
		if _, clash := distinct[id]; clash {
			t.Errorf("namespace %s shares the handle %s with another namespace", name, id)
		}
		distinct[id] = struct{}{}
	}

	if _, err := lab.Lookup("nf-missing"); !errors.Is(err, namespace.ErrNotFound) {
		t.Errorf("lookup of an unknown namespace returned %v, want ErrNotFound", err)
	}
}

func TestNamespaceDestructionIsRepeatable(t *testing.T) {
	ctx := context.Background()
	lab, err := network.Create(ctx, grantedSpec("dr"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := lab.Destroy(ctx); err != nil {
		t.Fatalf("first destroy: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := lab.Destroy(ctx); err != nil {
			t.Fatalf("destroy %d: %v", i+2, err)
		}
	}
	if names := lab.Names(); len(names) != 0 {
		t.Errorf("the laboratory still holds %v", names)
	}
}

func TestNamespaceNamesAreValidated(t *testing.T) {
	for _, name := range []string{
		"", "-lead", "trail-", "Upper", "under_score", "dot.name", "sp ace", "sla/sh",
		strings.Repeat("a", 64),
	} {
		if err := namespace.ValidateName("test", "name", name); err == nil {
			t.Errorf("namespace.ValidateName accepted %q", name)
		}
	}
	for _, name := range []string{"a", "nf-client0", "0", "a-b-9", strings.Repeat("a", 63)} {
		if err := namespace.ValidateName("test", "name", name); err != nil {
			t.Errorf("namespace.ValidateName rejected %q: %v", name, err)
		}
	}
}

func TestInterfaceNamesAreValidated(t *testing.T) {
	for _, name := range []string{"", "nfr-c0-x-too-long", strings.Repeat("i", 16), "bad name", "sla/sh", "..", "tab\tname"} {
		if err := iface.ValidateName("test", "interface", name); err == nil {
			t.Errorf("iface.ValidateName accepted %q", name)
		}
	}
	for _, name := range []string{"nfc0", "nfr-s1", "eth0.100", "a_b", strings.Repeat("i", 15)} {
		if err := iface.ValidateName("test", "interface", name); err != nil {
			t.Errorf("iface.ValidateName rejected %q: %v", name, err)
		}
	}
}

func TestCIDRsAreValidated(t *testing.T) {
	for _, value := range []string{"10.42.0.1/24", "10.42.0.0/33", "not-a-cidr", "", "::/0", "10.42.0.0/0"} {
		if _, err := netip.ParsePrefix(value); err == nil {
			if err := iface.ValidatePrefix("test", "network", mustPrefix(value)); err == nil {
				t.Errorf("iface.ValidatePrefix accepted %q", value)
			}
		}
	}
	for _, value := range []string{"10.42.0.0/25", "192.168.0.0/16", "172.16.0.0/30"} {
		if err := iface.ValidatePrefix("test", "network", mustPrefix(value)); err != nil {
			t.Errorf("iface.ValidatePrefix rejected %q: %v", value, err)
		}
	}
}

func TestRoutesAreValidated(t *testing.T) {
	lab := newLab(t, labSpec("rv"))
	_, client, err := lab.Client(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []struct {
		target netip.Prefix
		via    netip.Addr
	}{
		{target: mustPrefix("10.42.0.9/24"), via: client.Gateway},
		{target: mustPrefix("10.42.0.0/30"), via: netip.AddrFrom4([4]byte{10, 42, 0, 250})},
		{target: mustPrefix("10.42.0.0/30"), via: netip.MustParseAddr("fd00::1")},
	} {
		if err := lab.AddRoute(client.Namespace, bad.target, bad.via); err == nil {
			t.Errorf("AddRoute accepted %v via %v", bad.target, bad.via)
		}
	}

	if err := lab.AddRoute(client.Namespace, mustPrefix("10.99.0.0/30"), client.Gateway); err != nil {
		t.Fatalf("AddRoute rejected a valid route: %v", err)
	}
	if err := lab.RemoveRoute(client.Namespace, mustPrefix("10.99.0.0/30")); err != nil {
		t.Fatalf("RemoveRoute failed: %v", err)
	}
	routes, _, err := lab.Send(context.Background(), client.Namespace, "ip", "route")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(routes, "10.99.0.0/30") {
		t.Errorf("the route survived removal:\n%s", routes)
	}
}

func TestInterfacesAreInspected(t *testing.T) {
	lab := newLab(t, labSpec("in"))
	plan := lab.Plan()
	_, client, err := lab.Client(0)
	if err != nil {
		t.Fatal(err)
	}
	state, err := lab.Inspect(client.Namespace)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	link, err := state.Link(client.Interface)
	if err != nil {
		t.Fatalf("the planned interface %s is missing: %v", client.Interface, err)
	}
	if !link.Up || link.MTU != plan.MTU {
		t.Errorf("interface %s is up=%v mtu=%d, want up=true mtu=%d", link.Name, link.Up, link.MTU, plan.MTU)
	}
	if !hasPrefix(link.Addresses, client.Address) {
		t.Errorf("interface %s carries %v, want %s", link.Name, link.Addresses, client.Address)
	}

	router, err := lab.Inspect(plan.RouterNS)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range plan.Nodes {
		side, err := router.Link(node.RouterIf)
		if err != nil {
			t.Errorf("the router side %s of %s is missing: %v", node.RouterIf, node.Name, err)
			continue
		}
		if !hasPrefix(side.Addresses, node.Router) {
			t.Errorf("router interface %s carries %v, want %s", side.Name, side.Addresses, node.Router)
		}
	}
	if len(router.Links) != len(plan.Nodes)+1 {
		t.Errorf("the router has %d links, want %d", len(router.Links), len(plan.Nodes)+1)
	}
	t.Logf("router links: %v", router.Links)
}

func TestScopeIsEnforced(t *testing.T) {
	ctx := context.Background()
	plan, err := network.BuildPlan("sc", "10.42.0.0/24", 1, 1, 1500)
	if err != nil {
		t.Fatal(err)
	}
	wanted := []string{plan.RouterNS, plan.Nodes[0].Namespace, plan.Nodes[1].Namespace}

	first, err := network.Create(ctx, network.Spec{
		ID: "sc", Network: "10.42.0.0/24", Clients: 1, Servers: 1, Names: wanted,
	})
	if err != nil {
		t.Fatalf("a scope that grants exactly what is needed was refused: %v", err)
	}
	if err := first.Destroy(ctx); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	for _, narrow := range [][]string{
		{},
		{plan.Nodes[0].Namespace, plan.Nodes[1].Namespace},
		{plan.RouterNS, plan.Nodes[1].Namespace},
		{plan.RouterNS, plan.Nodes[0].Namespace},
		{"nf-something-else"},
	} {
		created, err := network.Create(ctx, network.Spec{
			ID: "sc", Network: "10.42.0.0/24", Clients: 1, Servers: 1, Names: narrow,
		})
		if err == nil {
			_ = created.Destroy(ctx)
			t.Errorf("scope %v was accepted, want refusal", narrow)
		}
	}

	wide := grantedSpec("sc")
	wide.Names = append(wide.Names, "nf-unused-grant")
	permissive, err := network.Create(ctx, wide)
	if err != nil {
		t.Fatalf("a scope that grants more than the plan needs was refused: %v", err)
	}
	if err := permissive.Destroy(ctx); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	for _, spec := range []network.Spec{
		{ID: "sc", Network: "10.42.0.0/24", Clients: 1, Servers: 1, MaxNames: 2},
		{ID: "sc", Network: "10.42.0.0/24", Clients: 1, Servers: 1, MaxIfs: 2},
	} {
		if _, err := network.Create(ctx, spec); err == nil {
			t.Errorf("limits %+v were accepted, want refusal", spec)
		}
	}
}

func TestInvalidPlansAreRefused(t *testing.T) {
	for _, spec := range []network.Spec{
		{ID: "b", Network: "not-a-cidr", Clients: 1, Servers: 1},
		{ID: "b", Network: "10.42.0.1/24", Clients: 1, Servers: 1},
		{ID: "b", Network: "10.42.0.0/30", Clients: 1, Servers: 1},
		{ID: "b", Network: "10.42.0.0/24", Clients: 0, Servers: 1},
		{ID: "b", Network: "10.42.0.0/24", Clients: 1, Servers: 0},
		{ID: "b", Network: "10.42.0.0/24", Clients: 9, Servers: 1},
		{ID: "b", Network: "10.42.0.0/24", Clients: 1, Servers: 1, MTU: 10},
	} {
		if _, err := network.BuildPlan(spec.ID, spec.Network, spec.Clients, spec.Servers, spec.MTU); err == nil {
			t.Errorf("BuildPlan accepted %+v", spec)
		}
	}
}

func TestDetermAddresses(t *testing.T) {
	first, err := network.BuildPlan("det", "10.42.0.0/24", 2, 2, 1500)
	if err != nil {
		t.Fatal(err)
	}
	second, err := network.BuildPlan("det", "10.42.0.0/24", 2, 2, 1500)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Nodes) != len(second.Nodes) {
		t.Fatalf("plans hold %d and %d nodes", len(first.Nodes), len(second.Nodes))
	}
	seen := map[string]netip.Addr{}
	for i := range first.Nodes {
		a, b := first.Nodes[i], second.Nodes[i]
		if a.Address != b.Address || a.Namespace != b.Namespace || a.Interface != b.Interface || a.Router != b.Router {
			t.Errorf("node %d differs between plans: %+v and %+v", i, a, b)
		}
		if !first.Network.Contains(a.Address.Addr()) {
			t.Errorf("node %s has %s outside %s", a.Name, a.Address, first.Network)
		}
		if clash, ok := seen[a.Address.Addr().String()]; ok {
			t.Errorf("node %s reuses the address of %s", a.Name, clash)
		}
		seen[a.Address.Addr().String()] = a.Address.Addr()
		if clash, ok := seen[a.Router.Addr().String()]; ok {
			t.Errorf("router side of %s reuses the address of %s", a.Name, clash)
		}
		seen[a.Router.Addr().String()] = a.Router.Addr()
	}
	if got := first.Nodes[0].Address.String(); got != "10.42.0.2/30" {
		t.Errorf("the first client has %s, want 10.42.0.2/30", got)
	}
	if got := first.Nodes[2].Address.String(); got != "10.42.0.10/30" {
		t.Errorf("the first server has %s, want 10.42.0.10/30", got)
	}
}

func TestHostIsNeverModified(t *testing.T) {
	before, err := hostState()
	if err != nil {
		t.Skipf("the host network state is not readable: %v", err)
	}

	ctx := context.Background()
	lab, err := network.Create(ctx, grantedSpec("host"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, client, err := lab.Client(0)
	if err != nil {
		t.Fatal(err)
	}
	_, server, err := lab.Server(0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := lab.Send(ctx, client.Namespace, "ping", "-c", "2", "-W", "2", server.Address.Addr().String()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := lab.Destroy(ctx); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	after, err := hostState()
	if err != nil {
		t.Fatal(err)
	}
	if before.links != after.links {
		t.Errorf("host interfaces changed:\nbefore %s\nafter  %s", before.links, after.links)
	}
	if before.routes != after.routes {
		t.Errorf("host routes changed:\nbefore %s\nafter  %s", before.routes, after.routes)
	}
	if before.forwarding != after.forwarding {
		t.Errorf("host forwarding changed from %s to %s", before.forwarding, after.forwarding)
	}
	t.Logf("host links %d bytes, routes %d bytes, forwarding %s",
		len(before.links), len(before.routes), before.forwarding)
}

func TestPartialCreationIsCleanedUp(t *testing.T) {
	ctx := context.Background()
	before, err := hostState()
	if err != nil {
		t.Skipf("the host network state is not readable: %v", err)
	}
	for i := 0; i < 5; i++ {
		_, err := network.Create(ctx, network.Spec{
			ID: "part", Network: "10.43.0.0/24", Clients: 1, Servers: 1, MTU: 1,
		})
		if err == nil {
			t.Fatal("an invalid MTU was accepted")
		}
	}
	after, err := hostState()
	if err != nil {
		t.Fatal(err)
	}
	if before.links != after.links || before.routes != after.routes {
		t.Error("a refused creation left state behind on the host")
	}
}

func TestInterruptedSetupLeavesNothingBehind(t *testing.T) {
	before, err := hostState()
	if err != nil {
		t.Skipf("the host network state is not readable: %v", err)
	}

	holder, err := namespace.NewLab()
	if err != nil {
		t.Fatalf("the laboratory agent did not start: %v", err)
	}
	space, err := holder.Spawn("nf-interrupted")
	if err != nil {
		_ = holder.Close()
		t.Fatalf("spawn: %v", err)
	}
	if !space.Alive() {
		t.Error("the spawned namespace is not alive")
	}
	if err := holder.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if space.Alive() {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		break
	}
	if space.Alive() {
		t.Error("the namespace survived the laboratory")
	}
	after, err := hostState()
	if err != nil {
		t.Fatal(err)
	}
	if before.links != after.links || before.routes != after.routes {
		t.Error("an interrupted setup left state behind on the host")
	}
}

func TestCreationFailures(t *testing.T) {
	holder, err := namespace.NewLab()
	if err != nil {
		t.Skipf("unprivileged user namespaces are unavailable: %v", err)
	}
	defer holder.Close()

	if _, err := holder.Spawn("UPPER"); err == nil {
		t.Error("an invalid namespace name was accepted")
	}
	first, err := holder.Spawn("nf-first")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if _, err := holder.Spawn("nf-first"); err == nil {
		t.Error("a duplicate namespace name was accepted")
	}
	if !first.Alive() {
		t.Error("the first namespace is not alive")
	}
	if _, err := holder.Lookup("nf-absent"); !errors.Is(err, namespace.ErrNotFound) {
		t.Errorf("lookup returned %v, want ErrNotFound", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Errorf("closing twice reported %v, want nil", err)
	}

	controller := iface.New(holder)
	if err := controller.CreatePair("test", "nfr-c0", "nfr-c0"); err == nil {
		t.Error("a duplicate veth name was accepted")
	}
	if err := controller.CreatePair("test", "nfc0", "a-very-long-interface-name"); err == nil {
		t.Error("an over long veth name was accepted")
	}
	if err := controller.CreatePair("test", "nfc1", "nfs1"); err != nil {
		t.Fatalf("CreatePair: %v", err)
	}
	if err := controller.AddAddress("test", first, "missing0", mustPrefix("10.42.0.2/30")); err == nil {
		t.Error("configuring a missing interface was accepted")
	}
	if err := controller.SetUp("test", first, "missing0"); err == nil {
		t.Error("raising a missing interface was accepted")
	}
	if err := controller.Move("test", "missing0", first); err == nil {
		t.Error("moving a missing interface was accepted")
	}
}

func TestLabFailsWhenTheAgentCannotStart(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := network.Create(context.Background(), grantedSpec("noagent"))
	if err == nil {
		t.Fatal("a laboratory started without ip")
	}
	if !strings.Contains(err.Error(), "Cannot find device") && !strings.Contains(err.Error(), "no agent") {
		t.Logf("creation refused with: %v", err)
	}
}

type hostSnapshot struct {
	links      string
	routes     string
	forwarding string
}

func hostState() (hostSnapshot, error) {
	var snapshot hostSnapshot
	links, err := exec.Command("ip", "-o", "link", "show").Output()
	if err != nil {
		return snapshot, err
	}
	routes, err := exec.Command("ip", "-o", "route", "show").Output()
	if err != nil {
		return snapshot, err
	}
	forwarding, err := exec.Command("cat", "/proc/sys/net/ipv4/ip_forward").Output()
	if err != nil {
		return snapshot, err
	}
	return hostSnapshot{
		links:      string(links),
		routes:     string(routes),
		forwarding: strings.TrimSpace(string(forwarding)),
	}, nil
}

func mustPrefix(value string) netip.Prefix {
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		panic(err)
	}
	return prefix
}

func hasPrefix(addresses []netip.Prefix, want netip.Prefix) bool {
	for _, address := range addresses {
		if address == want {
			return true
		}
	}
	return false
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
