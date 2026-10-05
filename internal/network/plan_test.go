package network

import (
	"net/netip"
	"strings"
	"testing"
)

func TestPlanIsDeterministic(t *testing.T) {
	first, err := BuildPlan("x", "10.42.0.0/24", 2, 2, 1500)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildPlan("x", "10.42.0.0/24", 2, 2, 1500)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Nodes) != 4 {
		t.Fatalf("the plan holds %d nodes, want 4", len(first.Nodes))
	}
	for i := range first.Nodes {
		if first.Nodes[i] != second.Nodes[i] {
			t.Errorf("node %d differs:\n%+v\n%+v", i, first.Nodes[i], second.Nodes[i])
		}
	}
	want := []string{
		"client0:10.42.0.2/30", "client1:10.42.0.6/30",
		"server0:10.42.0.10/30", "server1:10.42.0.14/30",
	}
	for i, node := range first.Nodes {
		if got := node.Name + ":" + node.Address.String(); got != want[i] {
			t.Errorf("node %d is %s, want %s", i, got, want[i])
		}
	}
}

func TestPlanAddressesAreUniqueAndInScope(t *testing.T) {
	for _, network := range []string{"10.42.0.0/24", "192.168.0.0/16", "172.16.5.0/24"} {
		plan, err := BuildPlan("s", network, 3, 3, 1500)
		if err != nil {
			t.Fatalf("%s: %v", network, err)
		}
		seen := map[netip.Addr]string{}
		for _, node := range plan.Nodes {
			for _, address := range []netip.Addr{node.Address.Addr(), node.Router.Addr()} {
				if other, clash := seen[address]; clash {
					t.Errorf("%s reuses %s for %s and %s", network, address, other, node.Name)
				}
				seen[address] = node.Name
				if !plan.Contains(address) {
					t.Errorf("%s puts %s outside %s", node.Name, address, plan.Network)
				}
			}
			if node.Gateway != node.Router.Addr() {
				t.Errorf("%s has the gateway %s, want %s", node.Name, node.Gateway, node.Router)
			}
			if node.Address.Bits() != plan.BlockLen {
				t.Errorf("%s has the prefix length %d, want %d", node.Name, node.Address.Bits(), plan.BlockLen)
			}
		}
	}
}

func TestPlanRefusesImpossibleInput(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		network               string
		clients, servers, mtu int
	}{
		{name: "not a prefix", network: "10.42.0.0", clients: 1, servers: 1},
		{name: "host bits", network: "10.42.0.1/24", clients: 1, servers: 1},
		{name: "default route", network: "0.0.0.0/0", clients: 1, servers: 1},
		{name: "too small", network: "10.42.0.0/28", clients: 1, servers: 1},
		{name: "no room for four addresses", network: "10.42.0.0/25", clients: 1, servers: 1},
		{name: "no clients", network: "10.42.0.0/24", clients: 0, servers: 1},
		{name: "no servers", network: "10.42.0.0/24", clients: 1, servers: 0},
		{name: "too many clients", network: "10.42.0.0/24", clients: 9, servers: 1},
		{name: "too many servers", network: "10.42.0.0/24", clients: 1, servers: 9},
		{name: "small mtu", network: "10.42.0.0/24", clients: 1, servers: 1, mtu: 68 - 1},
		{name: "large mtu", network: "10.42.0.0/24", clients: 1, servers: 1, mtu: 65536},
		{name: "ipv6", network: "fd00::/120", clients: 1, servers: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BuildPlan("s", tc.network, tc.clients, tc.servers, tc.mtu); err == nil {
				t.Errorf("BuildPlan accepted %s clients=%d servers=%d mtu=%d",
					tc.network, tc.clients, tc.servers, tc.mtu)
			}
		})
	}
}

func TestPlanNamesIncludeTheIdentifier(t *testing.T) {
	plan, err := BuildPlan("run7", "10.42.0.0/24", 1, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if plan.MTU != 1500 {
		t.Errorf("the default mtu is %d, want 1500", plan.MTU)
	}
	if plan.RouterNS != "nf-router-run7" {
		t.Errorf("the router namespace is %q", plan.RouterNS)
	}
	for _, node := range plan.Nodes {
		if !strings.HasSuffix(node.Namespace, "-run7") {
			t.Errorf("namespace %q does not carry the identifier", node.Namespace)
		}
	}
	plain, err := BuildPlan("", "10.42.0.0/24", 1, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if plain.RouterNS != "nf-router" {
		t.Errorf("without an identifier the router is %q", plain.RouterNS)
	}
}

func TestEnforceScopeNeedsEveryNamespace(t *testing.T) {
	plan, err := BuildPlan("sc", "10.42.0.0/24", 2, 2, 1500)
	if err != nil {
		t.Fatal(err)
	}
	var all []string
	all = append(all, plan.RouterNS)
	for _, node := range plan.Nodes {
		all = append(all, node.Namespace)
	}

	spec := Spec{ID: "sc", Network: "10.42.0.0/24", Clients: 2, Servers: 2, Names: all}
	if err := enforceScope(spec, plan); err != nil {
		t.Fatalf("a complete scope was refused: %v", err)
	}

	for i := range all {
		narrow := Spec{ID: "sc", Network: "10.42.0.0/24", Clients: 2, Servers: 2, Names: all}
		narrow.Names = append(narrow.Names[:i:i], narrow.Names[i+1:]...)
		if err := enforceScope(narrow, plan); err == nil {
			t.Errorf("a scope without %s was accepted", all[i])
		}
	}

	empty := Spec{ID: "sc", Network: "10.42.0.0/24", Clients: 2, Servers: 2}
	if err := enforceScope(empty, plan); err == nil {
		t.Error("an empty scope was accepted")
	}
}

func TestCheckSpecRespectsLimits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spec  Spec
		valid bool
	}{
		{name: "plain", spec: Spec{Clients: 1, Servers: 1}, valid: true},
		{name: "no clients", spec: Spec{Servers: 1}},
		{name: "no servers", spec: Spec{Clients: 1}},
		{name: "namespaces fit", spec: Spec{Clients: 1, Servers: 1, MaxNames: 3}, valid: true},
		{name: "namespaces do not fit", spec: Spec{Clients: 1, Servers: 1, MaxNames: 2}},
		{name: "interfaces fit", spec: Spec{Clients: 1, Servers: 1, MaxIfs: 4}, valid: true},
		{name: "interfaces do not fit", spec: Spec{Clients: 1, Servers: 1, MaxIfs: 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkSpec(tc.spec)
			if tc.valid && err != nil {
				t.Errorf("checkSpec refused %+v: %v", tc.spec, err)
			}
			if !tc.valid && err == nil {
				t.Errorf("checkSpec accepted %+v", tc.spec)
			}
		})
	}
}

func TestServerBlocksAreListed(t *testing.T) {
	plan, err := BuildPlan("sb", "10.42.0.0/24", 1, 3, 1500)
	if err != nil {
		t.Fatal(err)
	}
	blocks := plan.ServerBlocks()
	if len(blocks) != 3 {
		t.Fatalf("the plan lists %d server blocks, want 3", len(blocks))
	}
	if len(plan.Clients()) != 1 || len(plan.Servers()) != 3 {
		t.Errorf("the plan holds %d clients and %d servers", len(plan.Clients()), len(plan.Servers()))
	}
	for _, block := range blocks {
		if !plan.Contains(block.Addr()) {
			t.Errorf("block %s lies outside %s", block, plan.Network)
		}
	}
}
