package integration

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/codingseeker/netforge/internal/network"
	"github.com/codingseeker/netforge/internal/probe"
	"github.com/codingseeker/netforge/internal/service"
)

func probeLabSpec(t *testing.T, id, cidr string) (network.Spec, network.Plan) {
	t.Helper()
	plan, err := network.BuildPlan(id, cidr, 1, 1, 1500)
	if err != nil {
		t.Fatalf("BuildPlan() = %v, want nil", err)
	}
	names := []string{plan.RouterNS}
	for _, node := range plan.Nodes {
		names = append(names, node.Namespace)
	}
	spec := network.Spec{
		ID:      id,
		Network: cidr,
		Clients: 1,
		Servers: 1,
		MTU:     1500,
		Names:   names,
		Services: []network.ServiceSpec{{
			Namespace:  plan.Nodes[1].Namespace,
			TCPPort:    9100,
			UDPPort:    9101,
			DNSPort:    9102,
			TLSPort:    9103,
			HTTPPort:   9104,
			DNSNames:   map[string]string{"service.lab": plan.Nodes[1].Address.Addr().String()},
			HTTPPrefix: "/health",
			HTTPBody:   "ok",
		}},
	}
	return spec, plan
}

func TestEveryProtocolMeasuresAgainstRealService(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	spec, plan := probeLabSpec(t, "phase5", "10.55.0.0/24")

	lab, err := network.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create() = %v, want nil", err)
	}
	defer destroy(t, lab)

	client := plan.Nodes[0].Namespace
	server := plan.Nodes[1].Address.Addr().String()
	cases := []struct {
		name    string
		request probe.Request
		want    string
	}{
		{"icmp", probe.Request{Protocol: probe.ProtocolICMP, Target: server, Payload: 32}, "replies"},
		{"tcp", probe.Request{Protocol: probe.ProtocolTCP, Target: server, Port: 9100, Payload: 64}, "echoed"},
		{"udp", probe.Request{Protocol: probe.ProtocolUDP, Target: server, Port: 9101, Payload: 64}, "bytes"},
		{"dns", probe.Request{Protocol: probe.ProtocolDNS, Target: server, Port: 9102, Name: "service.lab"}, "service.lab"},
		{"tls", probe.Request{Protocol: probe.ProtocolTLS, Target: server, Port: 9103, Name: "netforge.lab", Payload: 32}, "tls"},
		{"http", probe.Request{Protocol: probe.ProtocolHTTP, Target: server, Port: 9104, Path: "/health"}, "HTTP 200"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := test.request
			request.Namespace = client
			request.Experiment = "phase5"
			request.Timeout = 5 * time.Second

			result, err := lab.Probe(ctx, request)
			if err != nil {
				t.Fatalf("Probe(%s) = %v, want nil", test.name, err)
			}
			if !result.OK {
				t.Errorf("Probe(%s).OK = false, want true (failure %q, error %q)",
					test.name, result.Failure, result.Error)
			}
			if result.Protocol != request.Protocol {
				t.Errorf("Probe(%s).Protocol = %q, want %q", test.name, result.Protocol, request.Protocol)
			}
			if result.Target != request.Target {
				t.Errorf("Probe(%s).Target = %q, want %q", test.name, result.Target, request.Target)
			}
			if result.Namespace != client {
				t.Errorf("Probe(%s).Namespace = %q, want %q", test.name, result.Namespace, client)
			}
			if result.RTT <= 0 {
				t.Errorf("Probe(%s).RTT = %s, want a measured round trip", test.name, result.RTT)
			}
			if result.Duration <= 0 {
				t.Errorf("Probe(%s).Duration = %s, want a measured duration", test.name, result.Duration)
			}
			if !strings.Contains(result.Detail, test.want) {
				t.Errorf("Probe(%s).Detail = %q, want it to mention %q", test.name, result.Detail, test.want)
			}
		})
	}
}

func TestTheServiceReportsWhatItIsServing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	spec, _ := probeLabSpec(t, "phase5-status", "10.55.1.0/24")

	lab, err := network.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create() = %v, want nil", err)
	}
	defer destroy(t, lab)

	services := lab.Services()
	if len(services) != 1 {
		t.Fatalf("Services() = %d services, want 1", len(services))
	}
	if services[0].TCPPort != 9100 || services[0].HTTPPort != 9104 {
		t.Errorf("Services()[0] ports = tcp %d http %d, want 9100 and 9104",
			services[0].TCPPort, services[0].HTTPPort)
	}

	status, err := lab.ServiceStatus(0)
	if err != nil {
		t.Fatalf("ServiceStatus(0) = %v, want nil", err)
	}
	if status.State != service.StateRunning {
		t.Errorf("ServiceStatus(0).State = %q, want %q", status.State, service.StateRunning)
	}
	if status.PID <= 0 {
		t.Errorf("ServiceStatus(0).PID = %d, want a real process", status.PID)
	}

	bound := strings.Join(status.Bound, " ")
	for _, want := range []string{
		"tcp/[::]:9100", "udp/[::]:9101", "dns/[::]:9102",
		"tls/[::]:9103", "http/[::]:9104",
	} {
		if !strings.Contains(bound, want) {
			t.Errorf("ServiceStatus(0).Bound = %v, want it to include %q", status.Bound, want)
		}
	}

	if _, err := lab.ServiceStatus(1); err == nil {
		t.Errorf("ServiceStatus(1) = nil error, want a not found error")
	}
}

func TestAProbeCannotLeaveTheLaboratory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	spec, plan := probeLabSpec(t, "phase5-scope", "10.55.2.0/24")

	lab, err := network.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create() = %v, want nil", err)
	}
	defer destroy(t, lab)

	host := netip.AddrFrom4([4]byte{127, 0, 0, 1})
	cases := []struct {
		name    string
		request probe.Request
	}{
		{"a host loopback address", probe.Request{
			Protocol: probe.ProtocolTCP, Target: host.String(), Port: 9100,
		}},
		{"a host namespace", probe.Request{
			Protocol: probe.ProtocolTCP, Target: plan.Nodes[1].Address.Addr().String(), Port: 9100,
			Namespace: "host",
		}},
		{"an address outside the network", probe.Request{
			Protocol: probe.ProtocolTCP, Target: "203.0.113.7", Port: 9100,
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := test.request
			if request.Namespace == "" {
				request.Namespace = plan.Nodes[0].Namespace
			}
			request.Experiment = "phase5-scope"
			request.Timeout = 2 * time.Second

			result, err := lab.Probe(ctx, request)
			if err == nil {
				t.Fatalf("Probe(%s) = %+v with a nil error, want a refusal", test.name, result)
			}
			if result.OK {
				t.Errorf("Probe(%s).OK = true, want false", test.name)
			}
		})
	}
}

func TestAProbeIsBounded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	spec, plan := probeLabSpec(t, "phase5-bounds", "10.55.3.0/24")

	spec.Probes = probe.Limits{
		MaxResponseSize:  512,
		MaxSize:          256,
		MaxStoredResults: 4,
		MaxConcurrent:    1,
	}

	lab, err := network.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create() = %v, want nil", err)
	}
	defer destroy(t, lab)

	if got := lab.Prober().Limits().MaxResponseSize; got != 512 {
		t.Errorf("Limits().MaxResponseSize = %d, want 512", got)
	}
	client := plan.Nodes[0].Namespace
	server := plan.Nodes[1].Address.Addr().String()

	for i := range 6 {
		request := probe.Request{
			Protocol: probe.ProtocolHTTP, Target: server, Port: 9104, Path: "/health",
			Namespace: client, Experiment: "phase5-bounds", Timeout: 5 * time.Second,
			Payload: i,
		}
		if _, err := lab.Probe(ctx, request); err != nil {
			t.Fatalf("Probe() = %v, want nil", err)
		}
	}
	results := lab.Prober().Results()
	if len(results) > 4 {
		t.Errorf("Results() = %d results, want at most the 4 the store keeps", len(results))
	}

	oversized := probe.Request{
		Protocol: probe.ProtocolTCP, Target: server, Port: 9100, Payload: 4096,
		Namespace: client, Experiment: "phase5-bounds", Timeout: 2 * time.Second,
	}
	result, err := lab.Probe(ctx, oversized)
	if err == nil {
		t.Errorf("Probe() with a 4096 byte payload = %+v with a nil error, want a refusal", result)
	}
}

func TestAClosedServiceIsVisible(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	spec, plan := probeLabSpec(t, "phase5-closed", "10.55.4.0/24")

	lab, err := network.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create() = %v, want nil", err)
	}
	defer destroy(t, lab)

	client := plan.Nodes[0].Namespace
	server := plan.Nodes[1].Address.Addr().String()

	dead := probe.Request{
		Protocol: probe.ProtocolTCP, Target: server, Port: 9999,
		Namespace: client, Experiment: "phase5-closed", Timeout: 2 * time.Second,
	}
	result, err := lab.Probe(ctx, dead)
	if err != nil {
		t.Fatalf("Probe() = %v, want nil: a refused connection is a result, not an error", err)
	}
	if result.OK {
		t.Errorf("Probe() = %+v, want a failed result for a closed port", result)
	}
	if result.Failure == probe.FailureNone {
		t.Errorf("Probe().Failure = %q, want a classified failure", result.Failure)
	}
}

func destroy(t *testing.T, lab *network.Lab) {
	t.Helper()
	if err := lab.Destroy(context.Background()); err != nil {
		t.Errorf("Destroy() = %v, want nil", err)
	}
}
