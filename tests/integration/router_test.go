package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/netforge/netforge/internal/network"
	"github.com/netforge/netforge/internal/probe"
)

func softLab(t *testing.T, id string) *network.Lab {
	t.Helper()
	lab, err := network.Create(context.Background(), network.Spec{
		ID:      id,
		Network: "10.42.0.0/24",
		Clients: 1,
		Servers: 1,
		MTU:     1500,
		Names: []string{
			network.NamespaceName("client0", id),
			network.NamespaceName("server0", id),
			network.NamespaceName("router", id),
		},
		Routing: network.RoutingSoftware,
	})
	if err != nil {
		t.Fatalf("create the laboratory: %v", err)
	}
	t.Cleanup(func() { _ = lab.Destroy(context.Background()) })
	return lab
}

func TestSoftwareRouterForwardsRealTraffic(t *testing.T) {
	lab := softLab(t, "soft")
	client, from, err := lab.Client(0)
	if err != nil {
		t.Fatal(err)
	}
	_, to, err := lab.Server(0)
	if err != nil {
		t.Fatal(err)
	}

	forwarding, err := lab.Forwarding()
	if err != nil {
		t.Fatal(err)
	}
	if forwarding {
		t.Error("kernel forwarding is on, so the kernel would forward instead of the router")
	}

	client.Exec("ping", "-c", "1", "-W", "1", "-w", "2", to.Address.Addr().String())

	out, _, err := client.Exec("ping", "-c", "3", "-W", "2", "-w", "6", to.Address.Addr().String())
	t.Logf("%s -> %s\n%s", from.Address.Addr(), to.Address.Addr(), out)
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	if !strings.Contains(out, "0% packet loss") {
		t.Fatalf("traffic did not cross the router:\n%s", out)
	}
	if !strings.Contains(out, "ttl=63") {
		t.Errorf("the reply did not cross exactly one router:\n%s", out)
	}

	big, _, err := client.Exec("ping", "-c", "2", "-W", "2", "-w", "6", "-s", "1200", to.Address.Addr().String())
	if err != nil {
		t.Fatalf("ping with a full payload: %v", err)
	}
	if !strings.Contains(big, "0% packet loss") {
		t.Errorf("a full sized packet did not survive the router:\n%s", big)
	}

	stats, err := lab.RouterStats()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("router stats: %+v", stats)
	if stats.Forwarded < 3 {
		t.Errorf("the router forwarded %d packets, want at least 3", stats.Forwarded)
	}
	if stats.Malformed != 0 {
		t.Errorf("the router saw %d malformed frames during clean traffic", stats.Malformed)
	}
	if stats.SendErrors != 0 {
		t.Errorf("the router failed to send %d times: %s", stats.SendErrors, stats.LastError)
	}
	if stats.Unresolved == 0 {
		t.Error("the router never reported an unresolved destination, so the first packet was not dropped")
	}
	if stats.LastError != "" {
		t.Errorf("the router still reports the failure %q", stats.LastError)
	}
}

func TestSoftwareRouterExpiresSmallTTL(t *testing.T) {
	lab := softLab(t, "ttl")
	client, _, err := lab.Client(0)
	if err != nil {
		t.Fatal(err)
	}
	_, to, err := lab.Server(0)
	if err != nil {
		t.Fatal(err)
	}
	target := to.Address.Addr().String()

	client.Exec("ping", "-c", "1", "-W", "1", "-w", "2", target)

	out, _, err := client.Exec("ping", "-c", "2", "-t", "1", "-W", "2", "-w", "6", target)
	t.Logf("ttl 1 -> %s\n%s", target, out)
	if err != nil {
		t.Fatalf("ping with a tiny ttl: %v", err)
	}
	if !strings.Contains(out, "Time to live exceeded") {
		t.Errorf("the router did not report the expiry of the packet:\n%s", out)
	}
	if !strings.Contains(out, "100% packet loss") {
		t.Errorf("an expired packet was still delivered:\n%s", out)
	}

	stats, err := lab.RouterStats()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("router stats: %+v", stats)
	if stats.TTLExceeded == 0 {
		t.Error("the router did not count the expired packet")
	}
}

func TestSoftwareRouterReportsNoRoute(t *testing.T) {
	lab := softLab(t, "route")
	client, from, err := lab.Client(0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.Exec("ip", "route", "add", "10.99.0.0/16",
		"via", from.Gateway.String(), "dev", from.Interface); err != nil {
		t.Fatalf("add a route towards a network the router does not know: %v", err)
	}

	out, _, err := client.Exec("ping", "-c", "2", "-W", "2", "-w", "6", "10.99.0.1")
	t.Logf("no route ->\n%s", out)
	if err != nil {
		t.Fatalf("ping an unreachable network: %v", err)
	}
	if !strings.Contains(out, "Destination Net Unreachable") {
		t.Errorf("the router did not report the missing route:\n%s", out)
	}

	stats, err := lab.RouterStats()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("router stats: %+v", stats)
	if stats.NoRoute == 0 {
		t.Error("the router did not count the packet without a route")
	}
	if stats.Unreachable == 0 {
		t.Error("the router did not answer the packet without a route")
	}
}

// The router forwards frames it reads from a packet socket, which show the
// bytes as they are on the wire rather than as the sending stack meant them.
// A sender that offloads its transport sum leaves only the seed of the checksum
// in those bytes, so a router that forwards them unchanged hands the far end a
// packet it rejects as corrupt: a connection that is refused and never answered,
// which on the wire is indistinguishable from a path that is down.
//
// Every protocol the prober speaks is checked here against a real service
// behind the software router, because the seed is only left unfinished by the
// stacks that offload: ICMP computes its own sum, so an echo request crosses
// fine while a handshake does not. A test that only pinged would have passed
// while TCP was broken.
func TestSoftwareRouterForwardsEveryProtocolTheProberSpeaks(t *testing.T) {
	plan, err := network.BuildPlan("softproto", "10.44.0.0/24", 1, 1, 1500)
	if err != nil {
		t.Fatalf("BuildPlan() = %v, want nil", err)
	}
	lab, err := network.Create(context.Background(), network.Spec{
		ID:      "softproto",
		Network: "10.44.0.0/24",
		Clients: 1,
		Servers: 1,
		MTU:     1500,
		Names:   []string{plan.RouterNS, plan.Nodes[0].Namespace, plan.Nodes[1].Namespace},
		Routing: network.RoutingSoftware,
		Services: []network.ServiceSpec{{
			Namespace:  plan.Nodes[1].Namespace,
			TCPPort:    9400,
			UDPPort:    9401,
			DNSPort:    9402,
			TLSPort:    9403,
			HTTPPort:   9404,
			DNSNames:   map[string]string{"router.lab": plan.Nodes[1].Address.Addr().String()},
			HTTPPrefix: "/health",
			HTTPBody:   "ok",
		}},
	})
	if err != nil {
		t.Fatalf("network.Create: %v", err)
	}
	t.Cleanup(func() {
		if err := lab.Destroy(context.Background()); err != nil {
			t.Errorf("destroy: %v", err)
		}
	})

	client := plan.Nodes[0].Namespace
	server := plan.Nodes[1].Address.Addr().String()
	clientSpace, err := lab.Space(client)
	if err != nil {
		t.Fatalf("Space(%s): %v", client, err)
	}
	// The router resolves a destination by asking for it, which drops the first
	// packet of every new flow. Echoing once warms the resolution so the
	// protocols below are measured against a router that already knows where
	// the server is, rather than against the cost of learning.
	if _, _, err := clientSpace.Exec("ping", "-n", "-c", "1", "-W", "2", "-w", "4", server); err != nil {
		t.Fatalf("warm the router: %v", err)
	}

	for _, test := range []struct {
		name     string
		protocol probe.Protocol
		port     uint16
		name_    string
		path     string
	}{
		{name: "TCP", protocol: probe.ProtocolTCP, port: 9400},
		{name: "UDP", protocol: probe.ProtocolUDP, port: 9401},
		{name: "DNS", protocol: probe.ProtocolDNS, port: 9402, name_: "router.lab"},
		{name: "TLS", protocol: probe.ProtocolTLS, port: 9403, name_: "router.lab"},
		{name: "HTTP", protocol: probe.ProtocolHTTP, port: 9404, path: "/health"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := lab.Probe(context.Background(), probe.Request{
				Protocol:   test.protocol,
				Namespace:  client,
				Target:     server,
				Port:       test.port,
				Name:       test.name_,
				Path:       test.path,
				Experiment: "softproto",
				Timeout:    10 * time.Second,
			})
			if err != nil {
				t.Fatalf("Probe(%s) = %v, want nil", test.name, err)
			}
			if !result.OK {
				t.Errorf("Probe(%s) failed as %q: %s", test.name, result.Failure, result.Error)
			}
		})
	}

	// The router's own record has to agree: a probe that succeeded without the
	// router carrying it would mean the probe never crossed the router at all.
	stats, err := lab.RouterStats()
	if err != nil {
		t.Fatalf("RouterStats: %v", err)
	}
	t.Logf("router stats: %+v", stats)
	if stats.Malformed != 0 {
		t.Errorf("the router counted %d malformed frames during clean traffic", stats.Malformed)
	}
	if stats.Forwarded == 0 {
		t.Error("the router forwarded nothing, so the probes did not cross it")
	}

	// A packet larger than the link it would leave by cannot be forwarded, and
	// the router has to say so rather than drop it quietly. Two boundaries meet
	// here: the sender may hand over a packet its own stack never segmented,
	// and the link still has a size. The answer has to name the size, so this
	// checks that the router answered rather than merely failed, and that it
	// was still forwarding afterwards.
	if stats.Oversize > 0 {
		if stats.Unreachable == 0 {
			t.Errorf("the router refused %d oversized packets without telling the sender", stats.Oversize)
		}
		if stats.SendErrors != 0 {
			t.Errorf("the router counted %d oversized packets and also failed to send %d times: %s",
				stats.Oversize, stats.SendErrors, stats.LastError)
		}
	} else if stats.SendErrors != 0 {
		t.Errorf("the router failed to send %d times for a reason other than an oversized packet: %s",
			stats.SendErrors, stats.LastError)
	}
	// Whatever the router refused, it kept working: one more probe after the
	// refusals has to answer, so a refusal cannot have become an outage.
	again, err := lab.Probe(context.Background(), probe.Request{
		Protocol:   probe.ProtocolHTTP,
		Namespace:  client,
		Target:     server,
		Port:       9404,
		Path:       "/health",
		Experiment: "softproto",
		Timeout:    10 * time.Second,
	})
	if err != nil || !again.OK {
		t.Errorf("the router stopped answering after refusing an oversized packet: ok=%v failure=%q err=%v",
			again.OK, again.Failure, err)
	}
}

func TestSoftwareRouterIgnoresForeignTraffic(t *testing.T) {
	lab := softLab(t, "foreign")
	client, from, err := lab.Client(0)
	if err != nil {
		t.Fatal(err)
	}
	_, to, err := lab.Server(0)
	if err != nil {
		t.Fatal(err)
	}
	target := to.Address.Addr().String()
	client.Exec("ping", "-c", "1", "-W", "1", "-w", "2", target)

	if _, _, err := client.Exec("ping", "-b", "-c", "20", "-i", "0.05", "-W", "1",
		"10.42.0.255"); err != nil {
		t.Logf("the broadcast flood reported %v, which is acceptable", err)
	}
	if _, _, err := client.Exec("arping", "-c", "2", "-I", from.Interface,
		from.Gateway.String()); err != nil {
		t.Logf("arping is not available or failed, which is acceptable: %v", err)
	}
	if !lab.RouterAlive() {
		t.Fatal("the router stopped on broadcast and neighbour traffic")
	}
	stats, err := lab.RouterStats()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("router stats after foreign traffic: %+v", stats)
	if stats.Malformed != 0 {
		t.Errorf("ordinary traffic was counted as malformed: %d", stats.Malformed)
	}
	if stats.Local != 0 {
		t.Errorf("the router answered %d packets meant for somebody else", stats.Local)
	}
	if stats.Ignored == 0 {
		t.Error("the router ignored nothing, so it cannot have seen the foreign traffic")
	}
	out, _, err := client.Exec("ping", "-c", "2", "-W", "2", "-w", "6", target)
	if err != nil {
		t.Fatalf("the router stopped forwarding after foreign traffic: %v", err)
	}
	if !strings.Contains(out, "0% packet loss") {
		t.Errorf("forwarding broke after foreign traffic:\n%s", out)
	}
}

func TestSoftwareRouterLeavesTheHostAlone(t *testing.T) {
	before, err := hostState()
	if err != nil {
		t.Skipf("the host network state is not readable: %v", err)
	}
	lab := softLab(t, "host")
	client, _, err := lab.Client(0)
	if err != nil {
		t.Fatal(err)
	}
	_, to, err := lab.Server(0)
	if err != nil {
		t.Fatal(err)
	}
	client.Exec("ping", "-c", "1", "-W", "1", "-w", "2", to.Address.Addr().String())
	if _, _, err := client.Exec("ping", "-c", "2", "-W", "2", "-w", "6",
		to.Address.Addr().String()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := lab.Destroy(context.Background()); err != nil {
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
	if lab.RouterAlive() {
		t.Error("the router outlived the laboratory")
	}
}

func TestSoftwareRouterStopsWithTheLaboratory(t *testing.T) {
	ctx := context.Background()
	lab, err := network.Create(ctx, network.Spec{
		ID:      "stop",
		Network: "10.42.0.0/24",
		Clients: 1, Servers: 1, MTU: 1500,
		Names: []string{
			network.NamespaceName("client0", "stop"),
			network.NamespaceName("server0", "stop"),
			network.NamespaceName("router", "stop"),
		},
		Routing: network.RoutingSoftware,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !lab.RouterAlive() {
		t.Fatal("the router is not running")
	}
	if err := lab.Destroy(ctx); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if lab.RouterAlive() {
		t.Error("the router survived the laboratory")
	}
}
