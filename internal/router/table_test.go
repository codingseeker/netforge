package router

import (
	"net/netip"
	"testing"

	"github.com/codingseeker/netforge/internal/apperr"
)

func mustTable(t *testing.T, scope string, limit int) *Table {
	t.Helper()
	prefix, err := netip.ParsePrefix(scope)
	if err != nil {
		t.Fatalf("scope %q: %v", scope, err)
	}
	table, err := NewTable(prefix, limit)
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	return table
}

func mustPrefix(t *testing.T, text string) netip.Prefix {
	t.Helper()
	prefix, err := netip.ParsePrefix(text)
	if err != nil {
		t.Fatalf("prefix %q: %v", text, err)
	}
	return prefix
}

func mustAddr(t *testing.T, text string) netip.Addr {
	t.Helper()
	address, err := netip.ParseAddr(text)
	if err != nil {
		t.Fatalf("address %q: %v", text, err)
	}
	return address
}

func TestLongestPrefixMatchWins(t *testing.T) {
	table := mustTable(t, "10.0.0.0/8", 0)
	routes := []Route{
		{Prefix: mustPrefix(t, "10.0.0.0/8"), Device: "wide"},
		{Prefix: mustPrefix(t, "10.42.0.0/16"), Device: "narrow"},
		{Prefix: mustPrefix(t, "10.42.7.0/24"), Device: "narrowest"},
	}
	for _, route := range routes {
		if err := table.Add(route); err != nil {
			t.Fatalf("Add %s: %v", route, err)
		}
	}
	cases := []struct {
		destination string
		want        string
	}{
		{"10.99.0.1", "wide"},
		{"10.42.99.1", "narrow"},
		{"10.42.7.1", "narrowest"},
		{"10.42.8.1", "narrow"},
	}
	for _, test := range cases {
		route, ok := table.Lookup(mustAddr(t, test.destination))
		if !ok {
			t.Errorf("no route for %s", test.destination)
			continue
		}
		if route.Device != test.want {
			t.Errorf("route for %s used %s, want %s", test.destination, route.Device, test.want)
		}
	}
}

func TestLookupMissesOutsideEveryPrefix(t *testing.T) {
	table := mustTable(t, "10.0.0.0/8", 0)
	if err := table.Add(Route{Prefix: mustPrefix(t, "10.42.0.0/16"), Device: "eth0"}); err != nil {
		t.Fatal(err)
	}
	for _, destination := range []string{"192.168.1.1", "11.0.0.1", "0.0.0.0"} {
		if route, ok := table.Lookup(mustAddr(t, destination)); ok {
			t.Errorf("%s matched %s, want no route", destination, route)
		}
	}
	if _, ok := table.Lookup(netip.Addr{}); ok {
		t.Error("the zero address matched a route")
	}
	if _, ok := table.Lookup(mustAddr(t, "fe80::1")); ok {
		t.Error("an IPv6 address matched a route")
	}
}

func TestOverlappingRoutesPreferTheMostSpecific(t *testing.T) {
	table := mustTable(t, "10.0.0.0/8", 0)
	pairs := [][2]string{
		{"10.0.0.0/8", "eth0"},
		{"10.0.0.0/16", "eth1"},
		{"10.0.0.0/24", "eth2"},
		{"10.0.0.0/30", "eth3"},
	}
	for _, pair := range pairs {
		if err := table.Add(Route{Prefix: mustPrefix(t, pair[0]), Device: pair[1]}); err != nil {
			t.Fatalf("Add %s: %v", pair[0], err)
		}
	}
	route, ok := table.Lookup(mustAddr(t, "10.0.0.1"))
	if !ok || route.Device != "eth3" {
		t.Errorf("10.0.0.1 resolved to %v, want the /30 on eth3", route)
	}
	if got := table.Len(); got != len(pairs) {
		t.Errorf("table holds %d routes, want %d", got, len(pairs))
	}
}

func TestConflictingRoutesAreRejected(t *testing.T) {
	table := mustTable(t, "10.0.0.0/8", 0)
	first := Route{Prefix: mustPrefix(t, "10.42.0.0/16"), NextHop: mustAddr(t, "10.0.0.1"), Device: "eth0"}
	if err := table.Add(first); err != nil {
		t.Fatal(err)
	}
	clash := Route{Prefix: first.Prefix, NextHop: mustAddr(t, "10.0.0.2"), Device: "eth0"}
	err := table.Add(clash)
	if err == nil {
		t.Fatal("a conflicting route was accepted")
	}
	if !apperr.Is(err, apperr.KindConflict) {
		t.Errorf("conflict reported as %v, want a conflict", err)
	}
	if got := table.Len(); got != 1 {
		t.Errorf("the rejected route was stored, the table holds %d", got)
	}
	route, _ := table.Lookup(mustAddr(t, "10.42.0.9"))
	if route.NextHop != first.NextHop {
		t.Errorf("the surviving route changed to %s", route)
	}
}

func TestIdenticalRouteIsReplaced(t *testing.T) {
	table := mustTable(t, "10.0.0.0/8", 0)
	route := Route{Prefix: mustPrefix(t, "10.42.0.0/16"), Device: "eth0"}
	if err := table.Add(route); err != nil {
		t.Fatal(err)
	}
	if err := table.Add(route); err != nil {
		t.Fatalf("re-adding an identical route: %v", err)
	}
	if got := table.Len(); got != 1 {
		t.Errorf("table holds %d routes, want 1", got)
	}
}

func TestRoutesOutsideTheScopeAreRejected(t *testing.T) {
	table := mustTable(t, "10.42.0.0/24", 0)
	cases := []Route{
		{Prefix: mustPrefix(t, "10.43.0.0/16"), Device: "eth0"},
		{Prefix: mustPrefix(t, "0.0.0.0/0"), Device: "eth0"},
		{Prefix: mustPrefix(t, "192.168.0.0/16"), Device: "eth0"},
		{Prefix: mustPrefix(t, "10.42.0.0/25"), NextHop: mustAddr(t, "192.168.9.9"), Device: "eth0"},
	}
	for _, route := range cases {
		err := table.Add(route)
		if err == nil {
			t.Errorf("%s was accepted, want a scope refusal", route)
			continue
		}
		if !apperr.Is(err, apperr.KindPermission) {
			t.Errorf("%s reported as %v, want a permission error", route, err)
		}
	}
	if got := table.Len(); got != 0 {
		t.Errorf("a refused route was stored, the table holds %d", got)
	}
}

func TestMalformedRoutesAreRejected(t *testing.T) {
	table := mustTable(t, "10.0.0.0/8", 0)
	cases := []Route{
		{Prefix: mustPrefix(t, "10.42.0.1/16"), Device: "eth0"},
		{Prefix: netip.Prefix{}, Device: "eth0"},
		{Prefix: mustPrefix(t, "10.42.0.0/16"), Device: ""},
		{Prefix: mustPrefix(t, "10.42.0.0/16"), Device: "a-very-long-device-name"},
		{Prefix: mustPrefix(t, "10.42.0.0/16"), Device: "eth 0"},
		{Prefix: mustPrefix(t, "10.42.0.0/16"), Device: "eth/0"},
		{Prefix: mustPrefix(t, "10.42.0.0/16"), NextHop: mustAddr(t, "fe80::1"), Device: "eth0"},
	}
	for _, route := range cases {
		if err := table.Add(route); err == nil {
			t.Errorf("%+v was accepted, want a refusal", route)
		}
	}
}

func TestRouteTableIsBounded(t *testing.T) {
	table := mustTable(t, "10.0.0.0/8", 4)
	for i := 0; i < 4; i++ {
		prefix := mustPrefix(t, "10."+string(rune('0'+i))+".0.0/16")
		if err := table.Add(Route{Prefix: prefix, Device: "eth0"}); err != nil {
			t.Fatalf("route %d: %v", i, err)
		}
	}
	err := table.Add(Route{Prefix: mustPrefix(t, "10.9.0.0/16"), Device: "eth0"})
	if err == nil {
		t.Fatal("the table accepted a route past its limit")
	}
	if !apperr.Is(err, apperr.KindLimitExceeded) {
		t.Errorf("overflow reported as %v, want a limit error", err)
	}
	if got := table.Len(); got != 4 {
		t.Errorf("table holds %d routes, want 4", got)
	}
}

func TestTableLimitsAreValidated(t *testing.T) {
	prefix := mustPrefix(t, "10.0.0.0/8")
	if _, err := NewTable(netip.Prefix{}, 0); err == nil {
		t.Error("an invalid scope was accepted")
	}
	if _, err := NewTable(mustPrefix(t, "2001:db8::/32"), 0); err == nil {
		t.Error("an IPv6 scope was accepted")
	}
	if _, err := NewTable(prefix, 1<<20); err == nil {
		t.Error("an excessive limit was accepted")
	}
	table, err := NewTable(prefix, 0)
	if err != nil {
		t.Fatal(err)
	}
	if table.Limit() != defaultMaxRoutes {
		t.Errorf("default limit is %d, want %d", table.Limit(), defaultMaxRoutes)
	}
}

func TestRemoveDropsExactlyOneRoute(t *testing.T) {
	table := mustTable(t, "10.0.0.0/8", 0)
	first := Route{Prefix: mustPrefix(t, "10.42.0.0/16"), Device: "eth0"}
	second := Route{Prefix: mustPrefix(t, "10.43.0.0/16"), Device: "eth1"}
	if err := table.Add(first); err != nil {
		t.Fatal(err)
	}
	if err := table.Add(second); err != nil {
		t.Fatal(err)
	}
	if err := table.Remove(second.Prefix); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if table.Len() != 1 {
		t.Fatalf("table holds %d routes, want 1", table.Len())
	}
	if _, ok := table.Lookup(mustAddr(t, "10.43.0.1")); ok {
		t.Error("the removed route still matches")
	}
	if _, ok := table.Lookup(mustAddr(t, "10.42.0.1")); !ok {
		t.Error("the surviving route stopped matching")
	}
	if err := table.Remove(second.Prefix); err == nil {
		t.Error("removing a missing route was accepted")
	}
}

func TestTableRejectsPrefixWiderThanScope(t *testing.T) {
	table, err := NewTable(netip.MustParsePrefix("10.42.0.0/24"), 0)
	if err != nil {
		t.Fatal(err)
	}
	wider := mustPrefix(t, "10.42.0.0/16")
	if err := table.Add(Route{Prefix: wider, Device: "c0"}); err == nil {
		t.Error("a prefix that reaches outside the scope was accepted")
	}
	same := mustPrefix(t, "10.42.0.0/24")
	if err := table.Add(Route{Prefix: same, Device: "c0"}); err != nil {
		t.Errorf("the scope itself was refused: %v", err)
	}
	narrow := mustPrefix(t, "10.42.0.0/25")
	if err := table.Add(Route{Prefix: narrow, Device: "c1"}); err != nil {
		t.Errorf("a prefix inside the scope was refused: %v", err)
	}
}

func TestTableListsWhatItHolds(t *testing.T) {
	table, err := NewTable(mustPrefix(t, "10.42.0.0/24"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(table.List()) != 0 {
		t.Fatalf("a new table lists %d routes", len(table.List()))
	}
	routes := []Route{
		{Prefix: mustPrefix(t, "10.42.0.4/30"), Device: "nfr-s0"},
		{Prefix: mustPrefix(t, "10.42.0.0/30"), Device: "nfr-c0"},
	}
	for _, route := range routes {
		if err := table.Add(route); err != nil {
			t.Fatalf("Add %s: %v", route.Prefix, err)
		}
	}
	list := table.List()
	if len(list) != 2 {
		t.Fatalf("the table lists %d routes, want 2", len(list))
	}
	list[0].Device = "changed"
	if table.List()[0].Device == "changed" {
		t.Error("the listing is a view of the table, not a copy")
	}
	if err := table.Remove(mustPrefix(t, "10.42.0.0/30")); err != nil {
		t.Fatal(err)
	}
	if len(table.List()) != 1 {
		t.Errorf("the table lists %d routes after a removal", len(table.List()))
	}
}
