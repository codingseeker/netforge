package integration

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/codingseeker/netforge/internal/chaos"
	"github.com/codingseeker/netforge/internal/network"
	"github.com/codingseeker/netforge/internal/probe"
)

const (
	audit12ID   = "nf-ph12-net"
	audit12CIDR = "10.58.0.0/24"
)

func audit12Lab(t *testing.T) (*network.Lab, network.Plan) {
	t.Helper()
	plan, err := network.BuildPlan(audit12ID, audit12CIDR, 1, 1, 1500)
	if err != nil {
		t.Fatalf("BuildPlan() = %v, want nil", err)
	}
	spec := network.Spec{
		ID:      audit12ID,
		Network: audit12CIDR,
		Clients: 1,
		Servers: 1,
		MTU:     1500,
		Names:   []string{plan.RouterNS, plan.Nodes[0].Namespace, plan.Nodes[1].Namespace},
		Routing: network.RoutingSoftware,
		Chaos: chaos.Limits{
			MaxFaults: 8, MaxDuration: 2 * time.Minute, MaxLatency: 5 * time.Second,
			MaxBandwidth: 10_000_000, MaxQueue: 1024, MaxGap: 100,
		},
		Probes: probe.Limits{
			MaxProbes: 100, MaxRate: 200, MaxSize: 1400,
			MaxDuration: 90 * time.Second, MaxConcurrent: 2,
		},
		Services: []network.ServiceSpec{{
			Namespace:  plan.Nodes[1].Namespace,
			TCPPort:    9200,
			UDPPort:    9201,
			DNSPort:    9202,
			TLSPort:    9203,
			HTTPPort:   9204,
			DNSNames:   map[string]string{"check.lab": plan.Nodes[1].Address.Addr().String()},
			HTTPPrefix: "/health",
			HTTPBody:   "ok",
		}},
	}
	lab, err := network.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("network.Create: %v", err)
	}
	t.Cleanup(func() {
		if err := lab.Destroy(context.Background()); err != nil {
			t.Errorf("destroy: %v", err)
		}
	})
	if lab.Measurer() == nil || lab.Chaos() == nil {
		t.Fatal("the laboratory cannot measure or inject faults, so no item below can be checked")
	}
	if lab.Prober() == nil {
		t.Fatal("the laboratory has no probe engine, so the protocol items cannot be measured")
	}
	return lab, plan
}

func TestTheNetworkingChecklistIsMeasuredOnOneLaboratory(t *testing.T) {
	lab, plan := audit12Lab(t)
	ctx := context.Background()

	client := plan.Nodes[0].Namespace
	server := plan.Nodes[1].Address.Addr().String()
	clientAddress := plan.Nodes[0].Address.Addr().String()
	gateway := plan.Nodes[0].Gateway.String()

	warm, err := lab.Space(client)
	if err != nil {
		t.Fatalf("Space(%s): %v", client, err)
	}
	if _, _, err := warm.Exec("ping", "-n", "-c", "1", "-W", "2", "-w", "4", server); err != nil {
		t.Fatalf("warm the path: %v", err)
	}

	healthy := measureAt(t, lab, client, server, 20, 50*time.Millisecond)
	report(t, "checklist baseline", healthy)
	if healthy.Received != healthy.Sent || healthy.Loss != 0 {
		t.Fatalf("the path is not healthy to begin with, so nothing below can be compared: %+v", healthy)
	}

	t.Run("packet forwarding", func(t *testing.T) {

		forwarding, err := lab.Forwarding()
		if err != nil {
			t.Fatalf("Forwarding: %v", err)
		}
		if forwarding {
			t.Error("kernel forwarding is on, so the kernel would have forwarded instead of the router")
		}

		space, err := lab.Space(client)
		if err != nil {
			t.Fatalf("Space(%s): %v", client, err)
		}
		out, _, err := space.Exec("ping", "-n", "-c", "3", "-W", "2", "-w", "8", server)
		if err != nil {
			t.Fatalf("ping: %v", err)
		}
		if !strings.Contains(out, "0% packet loss") {
			t.Fatalf("traffic did not cross the router:\n%s", out)
		}
		if !strings.Contains(out, "ttl=63") {
			t.Errorf("the reply did not cross exactly one router:\n%s", out)
		}
		stats, err := lab.RouterStats()
		if err != nil {
			t.Fatalf("RouterStats: %v", err)
		}
		if stats.Forwarded == 0 {
			t.Error("the router reports no forwarded packets, so nothing was forwarded by it")
		}
		if stats.Malformed != 0 || stats.SendErrors != 0 {
			t.Errorf("clean traffic produced %d malformed frames and %d send errors: %s",
				stats.Malformed, stats.SendErrors, stats.LastError)
		}
	})

	t.Run("routing", func(t *testing.T) {

		space, err := lab.Space(client)
		if err != nil {
			t.Fatalf("Space(%s): %v", client, err)
		}
		out, _, err := space.Exec("ip", "route", "get", server)
		if err != nil {
			t.Fatalf("ip route get %s: %v", server, err)
		}
		if !strings.Contains(out, "dev "+plan.Nodes[0].Interface) {
			t.Errorf("the route to %s does not leave by %s:\n%s", server, plan.Nodes[0].Interface, out)
		}
		if !strings.Contains(out, "via "+gateway) && !strings.Contains(out, "src "+clientAddress) {
			t.Errorf("the route to %s names neither the gateway nor this client:\n%s", server, out)
		}

		if _, _, err := lab.Send(ctx, plan.Nodes[0].Namespace, "ip", "route", "replace",
			"10.58.0.128/25", "via", gateway, "dev", plan.Nodes[0].Interface); err != nil {
			t.Fatalf("add the more specific route: %v", err)
		}
		t.Cleanup(func() {
			if _, _, err := lab.Send(context.Background(), plan.Nodes[0].Namespace,
				"ip", "route", "del", "10.58.0.128/25"); err != nil {
				t.Errorf("remove the more specific route: %v", err)
			}
		})
		state, err := lab.Inspect(plan.Nodes[0].Namespace)
		if err != nil {
			t.Fatalf("Inspect: %v", err)
		}
		found := false
		for _, route := range state.Routes {
			if strings.HasPrefix(route, "10.58.0.128/25") {
				found = true
			}
		}
		if !found {
			t.Errorf("the router's table does not hold the more specific route: %v", state.Routes)
		}
	})

	t.Run("ARP", func(t *testing.T) {

		stats, err := lab.RouterStats()
		if err != nil {
			t.Fatalf("RouterStats: %v", err)
		}
		if stats.ARPRequests == 0 || stats.ARPReplies == 0 {
			t.Errorf("the router answered %d of %d address requests, want both to have happened",
				stats.ARPReplies, stats.ARPRequests)
		}
		if stats.ARPProbes == 0 {
			t.Error("the router never asked the kernel's ARP for an address, so it never resolved one")
		}
		if stats.Neighbors == 0 {
			t.Error("the router holds no neighbours, so it forwarded nothing it had resolved")
		}

		serverSide, err := lab.Space(plan.Nodes[1].Namespace)
		if err != nil {
			t.Fatalf("Space(%s): %v", plan.Nodes[1].Namespace, err)
		}
		neighbours, _, err := serverSide.Exec("ip", "neigh", "show")
		if err != nil {
			t.Fatalf("read the server's neighbour table: %v", err)
		}
		if !strings.Contains(neighbours, plan.Nodes[0].Gateway.String()) &&
			!strings.Contains(neighbours, plan.Nodes[1].Gateway.String()) {
			t.Errorf("the server resolved no gateway, so the exchange the router took part in never happened:\n%s", neighbours)
		}
	})

	t.Run("ICMP", func(t *testing.T) {
		measurement := measureAt(t, lab, client, server, 10, 50*time.Millisecond)
		report(t, "checklist icmp", measurement)
		if measurement.Received != measurement.Sent || measurement.Loss != 0 {
			t.Errorf("%d of %d echo requests came back, want all of them", measurement.Received, measurement.Sent)
		}
		if measurement.AverageRTT <= 0 || measurement.MinRTT <= 0 {
			t.Errorf("the round trip is min %s average %s, want a measured time", measurement.MinRTT, measurement.AverageRTT)
		}
		if measurement.AverageRTT < measurement.MinRTT || measurement.MaxRTT < measurement.AverageRTT {
			t.Errorf("the round trips are min %s average %s max %s, which cannot happen",
				measurement.MinRTT, measurement.AverageRTT, measurement.MaxRTT)
		}
	})

	for _, item := range []struct {
		name     string
		protocol probe.Protocol
		port     uint16
		name_    string
		path     string
		mention  string
	}{
		{name: "TCP", protocol: probe.ProtocolTCP, port: 9200, mention: "connected"},
		{name: "UDP", protocol: probe.ProtocolUDP, port: 9201, mention: "bytes"},
		{name: "DNS", protocol: probe.ProtocolDNS, port: 9202, name_: "check.lab", mention: "check.lab"},
		{name: "TLS", protocol: probe.ProtocolTLS, port: 9203, name_: "check.lab", mention: "tls"},
		{name: "HTTP", protocol: probe.ProtocolHTTP, port: 9204, path: "/health", mention: "HTTP 200"},
	} {
		t.Run(item.name, func(t *testing.T) {
			result, err := lab.Probe(ctx, probe.Request{
				Protocol:   item.protocol,
				Namespace:  client,
				Target:     server,
				Port:       item.port,
				Name:       item.name_,
				Path:       item.path,
				Experiment: audit12ID,
				Timeout:    10 * time.Second,
			})
			if err != nil {
				t.Fatalf("Probe(%s) = %v, want nil", item.name, err)
			}
			if !result.OK {
				t.Fatalf("Probe(%s) failed as %q: %s", item.name, result.Failure, result.Error)
			}
			if !strings.Contains(strings.ToLower(result.Detail), strings.ToLower(item.mention)) {
				t.Errorf("Probe(%s).Detail = %q, want it to mention %q", item.name, result.Detail, item.mention)
			}
			if result.RTT <= 0 {
				t.Errorf("Probe(%s).RTT = %s, want a measured round trip", item.name, result.RTT)
			}
			if result.Protocol != item.protocol {
				t.Errorf("Probe(%s).Protocol = %q, want %q", item.name, result.Protocol, item.protocol)
			}
		})
	}

	for _, item := range []struct {
		name  string
		spec  chaos.Spec
		count int
		slow  time.Duration
		worse func(before, after probe.Measurement) error
	}{
		{
			name:  "packet loss",
			count: 60, slow: 50 * time.Millisecond,
			spec: chaos.Spec{Kind: chaos.KindPacketLoss, Parameters: chaos.Parameters{Loss: 25}},
			worse: func(before, after probe.Measurement) error {
				if after.Received >= after.Sent {
					return fmt.Errorf("all %d probes came back, a loss fault dropped nothing", after.Sent)
				}
				if after.Loss <= 0 {
					return fmt.Errorf("the measurement reports no loss over %d probes", after.Sent)
				}
				return nil
			},
		},
		{
			name:  "latency",
			count: 20, slow: 50 * time.Millisecond,
			spec: chaos.Spec{Kind: chaos.KindLatency, Parameters: chaos.Parameters{Latency: chaos.Duration(60 * time.Millisecond)}},
			worse: func(before, after probe.Measurement) error {

				if after.MinRTT < before.MinRTT+50*time.Millisecond {
					return fmt.Errorf("the fastest reply is %s, the healthy path answered in %s",
						after.MinRTT, before.MinRTT)
				}
				return nil
			},
		},
		{
			name:  "jitter",
			count: 20, slow: 50 * time.Millisecond,
			spec: chaos.Spec{Kind: chaos.KindJitter, Parameters: chaos.Parameters{
				Latency: chaos.Duration(60 * time.Millisecond), Jitter: chaos.Duration(30 * time.Millisecond),
			}},
			worse: func(before, after probe.Measurement) error {

				if spread := after.MaxRTT - after.MinRTT; spread < 10*time.Millisecond {
					return fmt.Errorf("the replies span only %s, which is not a spread", spread)
				}
				if after.MaxRTT <= before.MaxRTT+10*time.Millisecond {
					return fmt.Errorf("the slowest reply is %s, the healthy path's slowest was %s",
						after.MaxRTT, before.MaxRTT)
				}
				return nil
			},
		},
		{
			name:  "bandwidth",
			count: 100, slow: 10 * time.Millisecond,
			spec: chaos.Spec{Kind: chaos.KindBandwidth, Parameters: chaos.Parameters{Rate: 200_000}},
			worse: func(before, after probe.Measurement) error {

				if after.Goodput == 0 {
					return fmt.Errorf("the measurement reports no goodput at all")
				}
				if after.Goodput >= before.Goodput {
					return fmt.Errorf("the goodput is %d bit/s, the healthy path managed %d bit/s",
						after.Goodput, before.Goodput)
				}
				if after.Goodput > 3*200_000 {
					return fmt.Errorf("the goodput is %d bit/s, well past the 200000 bit/s ceiling", after.Goodput)
				}
				return nil
			},
		},
	} {
		t.Run(item.name, func(t *testing.T) {
			before := measureAt(t, lab, client, server, item.count, item.slow)
			if item.name == "bandwidth" {
				report(t, "checklist "+item.name+" before", before)
			}
			if before.Received != before.Sent || before.Loss != 0 {
				t.Fatalf("the path is not healthy before the %s fault: %+v", item.name, before)
			}

			spec := item.spec
			spec.Target = client
			fault, err := lab.Chaos().Apply(ctx, "ph12-"+strings.ReplaceAll(item.name, " ", "-"), spec)
			if err != nil {
				t.Fatalf("apply the %s fault: %v", item.name, err)
			}
			if fault.Status != chaos.StatusActive || !fault.Verified {
				t.Fatalf("the %s fault is %q and verified=%v, want an active verified fault",
					item.name, fault.Status, fault.Verified)
			}
			removed := false
			defer func() {
				if removed {
					return
				}
				if _, err := lab.Chaos().Remove(fault.ID); err != nil {
					t.Errorf("remove the %s fault: %v", item.name, err)
				}
			}()

			after := measureAt(t, lab, client, server, item.count, item.slow)
			report(t, "checklist "+item.name, after)
			if err := item.worse(before, after); err != nil {
				t.Errorf("the %s fault did not show in the measurement: %v", item.name, err)
			}

			if _, err := lab.Chaos().Remove(fault.ID); err != nil {
				t.Fatalf("remove the %s fault: %v", item.name, err)
			}
			removed = true
			restored := measureAt(t, lab, client, server, item.count, item.slow)
			report(t, "checklist "+item.name+" after removal", restored)
			if restored.Loss != 0 {
				t.Errorf("after the %s fault was removed the path still lost %.1f%%", item.name, restored.Loss)
			}
		})
	}
}

func TestTheLaboratoryTheChecklistUsedIsTakenApart(t *testing.T) {
	before, err := hostState()
	if err != nil {
		t.Skipf("the host network state is not readable: %v", err)
	}

	lab, plan := audit12Lab(t)
	if err := lab.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy() = %v, want nil", err)
	}
	if err := lab.Destroy(context.Background()); err != nil {
		t.Errorf("destroying an already destroyed laboratory returned %v, want nil", err)
	}
	after, err := hostState()
	if err != nil {
		t.Fatalf("the host network state is not readable: %v", err)
	}
	if before.links != after.links || before.routes != after.routes {
		t.Error("the host did not return to its prior state after the checklist's laboratory")
	}
	if leaks := hostLeaksFor(audit12ID); len(leaks) != 0 {
		t.Errorf("the laboratory left the host carrying its identity:\n%s", strings.Join(leaks, "\n"))
	}
	if _, err := lab.Lookup(plan.Nodes[0].Namespace); err == nil {
		t.Errorf("%s still resolves after the laboratory was destroyed", plan.Nodes[0].Namespace)
	}
}

func hostLeaksFor(id string) (leaks []string) {
	for _, inspect := range [][]string{
		{"ip", "-o", "link", "show"},
		{"ip", "-o", "route", "show"},
		{"ip", "netns", "list"},
		{"tc", "qdisc", "show"},
	} {
		out, err := exec.Command(inspect[0], inspect[1:]...).Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, id) {
				leaks = append(leaks, strings.Join(inspect, " ")+" => "+line)
			}
		}
	}
	return leaks
}
