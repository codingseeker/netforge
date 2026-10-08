package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/codingseeker/netforge/internal/network"
	"github.com/codingseeker/netforge/internal/probe"
)

const (
	phase17LabID   = "nfp17"
	phase17Client  = "nf-client0-" + phase17LabID
	phase17Server  = "nf-server0-" + phase17LabID
	phase17Router  = "nf-router-" + phase17LabID
	phase17CIDR    = "10.47.0.0/24"
	phase17ServerI = "10.47.0.6"
	phase17Port    = 9800
)

func phase17Document(t *testing.T, loss string) string {
	t.Helper()
	document := "name: phase17-rudp\n" +
		"seed: 99\n" +
		"scope:\n" +
		"  network: " + phase17CIDR + "\n" +
		"  namespaces:\n" +
		"    - " + phase17Client + "\n" +
		"    - " + phase17Server + "\n" +
		"    - " + phase17Router + "\n" +
		"limits:\n" +
		"  max_namespaces: 8\n" +
		"  max_interfaces: 16\n" +
		"  max_duration: 5m\n" +
		"  max_bandwidth: 1Gbps\n" +
		"  max_packet_rate: 100000\n" +
		"  max_concurrent_probes: 64\n"
	if loss != "" {
		document += "faults:\n" +
			"  - target: " + phase17Server + "\n" +
			"    type: packet_loss\n" +
			"    value: " + loss + "\n"
	}
	return writeFile(t, "phase17.yaml", document)
}

type phase17Measurement struct {
	Sent          int     `json:"sent"`
	Received      int     `json:"received"`
	Loss          float64 `json:"loss"`
	Retransmitted int     `json:"retransmitted"`
	AverageRTT    any     `json:"average_rtt"`
	Transport     *struct {
		MaxPayload int `json:"max_payload"`
		MaxWindow  int `json:"max_window"`
	} `json:"transport"`
}

func phase17Run(t *testing.T, protocol, loss string, rounds int) phase17Measurement {
	t.Helper()
	document := phase17Document(t, loss)
	result := execNetforge(t, "",
		"experiment", "run",
		"--id", phase17LabID,
		"--namespace", phase17Client,
		"--target", phase17ServerI,
		"--protocol", protocol,
		"--port", itoaTest(phase17Port),
		"--service-ports", "1",
		"--service-base-port", itoaTest(phase17Port),
		"--count", itoaTest(rounds),
		"--timeout", "400ms",
		"--deadline", "12s",
		"--after-fault", "300ms",
		"-recover", "remove_fault:"+phase17Server,
		"-authorize",
		"-why", "the run removes the fault its own document declared",
		"--output", "json",
		document)
	if result.exitCode != 0 {
		t.Fatalf("the %s run failed: exit %d\n%s\n%s", protocol, result.exitCode, result.stdout, result.stderr)
	}
	report := decodeRun(t, result.stdout)
	if len(report.Observed) == 0 {
		t.Fatalf("the %s run reported no observation:\n%s", protocol, result.stdout)
	}
	return report.Observed[0]
}

func TestAProbeCanMeasureOverTheReliableTransport(t *testing.T) {
	measured := phase17Run(t, "rudp", "90%", 12)
	if measured.Sent == 0 || measured.Received == 0 {
		t.Fatalf("the reliable transport reported sent=%d received=%d", measured.Sent, measured.Received)
	}
	if measured.Transport == nil {
		t.Fatal("the measurement carries no transport limits, so a window figure would not be reproducible")
	}
	if measured.Transport.MaxWindow < 1 || measured.Transport.MaxPayload < 1 {
		t.Errorf("the transport limits reported are %+v, want a window and a payload", *measured.Transport)
	}

	if measured.Received != measured.Sent {
		t.Errorf("the reliable transport delivered %d of %d probes under 90%% loss; "+
			"that is the whole point of it", measured.Received, measured.Sent)
	}
	if measured.Retransmitted == 0 {
		t.Errorf("the transport reported no retransmissions under 90%% loss, so it either "+
			"suffered none or is not counting: %+v", measured)
	}
}

func TestTheReliableTransportCostsNothingOnACleanPath(t *testing.T) {
	lab, err := network.Create(context.Background(), network.Spec{
		ID:      phase17LabID,
		Network: phase17CIDR,
		Clients: 1,
		Servers: 1,
		MTU:     1500,
		Names: []string{
			network.NamespaceName("client0", phase17LabID),
			network.NamespaceName("server0", phase17LabID),
			network.NamespaceName("router", phase17LabID),
		},
		Services: []network.ServiceSpec{{
			Namespace: network.NamespaceName("server0", phase17LabID),
			RUDPPort:  phase17Port,
		}},
	})
	if err != nil {
		t.Fatalf("create a laboratory: %v", err)
	}
	t.Cleanup(func() { _ = lab.Destroy(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for round := 0; round < 5; round++ {
		result, err := lab.Probe(ctx, probe.Request{
			Experiment: phase17LabID,
			Namespace:  network.NamespaceName("client0", phase17LabID),
			Target:     phase17ServerI,
			Port:       phase17Port,
			Protocol:   probe.ProtocolRUDP,
			Payload:    32,
			Timeout:    2 * time.Second,
		})
		if err != nil {
			t.Fatalf("round %d: probe: %v", round, err)
		}
		if !result.OK {
			t.Fatalf("round %d: the probe reported %q", round, result.Detail)
		}
		if result.Retransmitted != 0 {
			t.Fatalf("round %d: the transport retransmitted %d segments on a path with no "+
				"fault at all, so the figure is not read from the wire", round, result.Retransmitted)
		}
	}
}

func TestTheSameFaultIsLossOnUDPAndInvisibleOnTheReliableTransport(t *testing.T) {
	datagram := phase17Run(t, "udp", "90%", 6)
	reliable := phase17Run(t, "rudp", "90%", 12)

	if datagram.Loss <= 0 {
		t.Errorf("a plain datagram path under 90%% loss reported %v%% loss, so the fault "+
			"was not on this path at all", datagram.Loss)
	}
	if reliable.Loss != 0 {
		t.Errorf("the reliable transport reported %v%% loss under the same fault, so it "+
			"is not carrying the exchange reliably", reliable.Loss)
	}
	if reliable.Retransmitted == 0 {
		t.Error("the reliable path showed no loss and no retransmissions, which means it " +
			"never met a lost segment")
	}
}

func TestTheReliableProtocolIsRefusedWhereItCannotBeServed(t *testing.T) {
	document := phase17Document(t, "")
	result := execNetforge(t, "",
		"experiment", "run",
		"--id", phase17LabID,
		"--namespace", phase17Client,
		"--target", phase17ServerI,
		"--protocol", "rudp",
		"--port", itoaTest(phase17Port),
		"--count", "2",
		"--timeout", "300ms",
		"--deadline", "4s",
		document)
	if result.exitCode == 0 {
		t.Fatal("a reliable observation with no listener was accepted")
	}
	if !strings.Contains(result.stderr, "-service-ports") {
		t.Errorf("the refusal does not name the flag that would answer it:\n%s", result.stderr)
	}
}

func TestTheReliableProtocolAppearsInHelpAndActions(t *testing.T) {

	refused := execNetforge(t, "",
		"experiment", "plan",
		"--namespace", "service-a",
		"--target", "10.42.0.3",
		"--protocol", "carrier-pigeon",
		testdata("valid-full.yaml"))
	if refused.exitCode == 0 {
		t.Fatal("an unknown protocol was accepted")
	}
	if !strings.Contains(refused.stderr, "icmp") || !strings.Contains(refused.stderr, "rudp") {
		t.Errorf("the refusal does not list the protocols that exist:\n%s", refused.stderr)
	}
}
