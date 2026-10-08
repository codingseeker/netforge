package integration

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/codingseeker/netforge/internal/chaos"
	"github.com/codingseeker/netforge/internal/config"
	"github.com/codingseeker/netforge/internal/network"
	"github.com/codingseeker/netforge/internal/probe"
)

type labEnds struct {
	Client string

	ClientDevice string

	Server string
}

func chaosLab(t *testing.T, id string) (*network.Lab, labEnds) {
	t.Helper()
	spec := granted(labSpec(id))
	spec.Chaos = chaos.Limits{
		MaxFaults:    8,
		MaxDuration:  2 * time.Minute,
		MaxLatency:   5 * time.Second,
		MaxBandwidth: 10_000_000,
		MaxQueue:     1024,
		MaxGap:       100,
	}
	spec.Probes = probe.Limits{
		MaxProbes: 100, MaxRate: 200, MaxSize: 1400,

		MaxDuration: 90 * time.Second, MaxConcurrent: 2,
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
	if lab.Chaos() == nil || lab.Measurer() == nil {
		t.Fatal("the laboratory has no fault engine")
	}

	_, from, err := lab.Client(0)
	if err != nil {
		t.Fatal(err)
	}
	_, to, err := lab.Server(0)
	if err != nil {
		t.Fatal(err)
	}
	return lab, labEnds{
		Client:       from.Namespace,
		ClientDevice: from.Interface,
		Server:       to.Address.Addr().String(),
	}
}

func measure(t *testing.T, lab *network.Lab, client, target string, count int) probe.Measurement {
	t.Helper()
	return measureAt(t, lab, client, target, count, 50*time.Millisecond)
}

func measureAt(t *testing.T, lab *network.Lab, client, target string, count int, interval time.Duration) probe.Measurement {
	t.Helper()
	measurement, err := lab.Measure(context.Background(), client, target, probe.Options{
		Count:    count,
		Interval: interval,
		Size:     1400,

		Timeout:  3 * time.Second,
		Deadline: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("measure %s -> %s: %v", client, target, err)
	}
	return measurement
}

func report(t *testing.T, label string, m probe.Measurement) {
	t.Helper()
	t.Logf("%-22s sent=%d received=%d loss=%.1f%% dup=%d unordered=%d min=%s avg=%s max=%s goodput=%dbit/s",
		label, m.Sent, m.Received, m.Loss, m.Duplicates, m.OutOfOrder,
		m.MinRTT, m.AverageRTT, m.MaxRTT, m.Goodput)
}

func TestEveryFaultKindProducesMeasuredBehaviour(t *testing.T) {
	lab, ends := chaosLab(t, "gate")
	ctx := context.Background()

	baseline := measure(t, lab, ends.Client, ends.Server, 20)
	report(t, "baseline", baseline)
	if baseline.Received != 20 || baseline.Loss != 0 {
		t.Fatalf("the path is not healthy to begin with: %+v", baseline)
	}
	if baseline.MinRTT > 20*time.Millisecond {
		t.Fatalf("the path is already %s slow, the measurements below would mean nothing", baseline.MinRTT)
	}
	if baseline.Duplicates != 0 || baseline.OutOfOrder != 0 {
		t.Fatalf("the path already shows %d duplicate and %d out of order replies, "+
			"the measurements below would mean nothing", baseline.Duplicates, baseline.OutOfOrder)
	}

	cases := []struct {
		kind chaos.Kind
		spec chaos.Spec

		count    int
		interval time.Duration

		worse func(before, after probe.Measurement) error
	}{
		{
			kind: chaos.KindLatency,
			spec: chaos.Spec{
				Target: ends.Client, Kind: chaos.KindLatency,
				Parameters: chaos.Parameters{Latency: chaos.Duration(60 * time.Millisecond)},
			},
			count: 20, interval: 50 * time.Millisecond,
			worse: func(before, after probe.Measurement) error {

				floor := before.MinRTT + 50*time.Millisecond
				if after.MinRTT < floor {
					return fmt.Errorf("the fastest reply is %s, a 60ms delay cannot be there", after.MinRTT)
				}
				if after.AverageRTT < after.MinRTT {
					return fmt.Errorf("the average %s is below the minimum %s", after.AverageRTT, after.MinRTT)
				}
				return nil
			},
		},
		{
			kind: chaos.KindJitter,
			spec: chaos.Spec{
				Target: ends.Client, Kind: chaos.KindJitter,
				Parameters: chaos.Parameters{
					Latency: chaos.Duration(60 * time.Millisecond),
					Jitter:  chaos.Duration(30 * time.Millisecond),
				},
			},
			count: 20, interval: 50 * time.Millisecond,
			worse: func(before, after probe.Measurement) error {

				spread := after.MaxRTT - after.MinRTT
				if spread < 10*time.Millisecond {
					return fmt.Errorf("the replies span only %s, which is not a 30ms jitter", spread)
				}
				if after.MaxRTT <= before.MaxRTT+10*time.Millisecond {
					return fmt.Errorf("the slowest reply is %s, the jitter did not reach it", after.MaxRTT)
				}
				return nil
			},
		},
		{
			kind: chaos.KindPacketLoss,
			spec: chaos.Spec{
				Target: ends.Client, Kind: chaos.KindPacketLoss,
				Parameters: chaos.Parameters{Loss: 25},
			},

			count: 60, interval: 50 * time.Millisecond,
			worse: func(before, after probe.Measurement) error {

				if after.Received >= after.Sent {
					return fmt.Errorf("all %d probes came back, a 25%% loss fault dropped nothing", after.Sent)
				}
				if after.Loss < 8 || after.Loss > 45 {
					return fmt.Errorf("the measured loss is %.1f%%, a 25%% fault should land near it", after.Loss)
				}
				return nil
			},
		},
		{
			kind: chaos.KindBandwidth,
			spec: chaos.Spec{
				Target: ends.Client, Kind: chaos.KindBandwidth,
				Parameters: chaos.Parameters{Rate: 200_000},
			},

			count: 100, interval: 10 * time.Millisecond,
			worse: func(before, after probe.Measurement) error {

				if after.Goodput >= before.Goodput/2 {
					return fmt.Errorf("the goodput is %d bit/s against %d bit/s without the fault",
						after.Goodput, before.Goodput)
				}
				if after.Goodput > 3*200_000 {
					return fmt.Errorf("the goodput is %d bit/s, well past the 200000 bit/s ceiling", after.Goodput)
				}
				return nil
			},
		},
		{
			kind: chaos.KindDuplication,
			spec: chaos.Spec{
				Target: ends.Client, Kind: chaos.KindDuplication,
				Parameters: chaos.Parameters{Duplication: 40},
			},
			count: 20, interval: 50 * time.Millisecond,
			worse: func(before, after probe.Measurement) error {

				if after.Duplicates == 0 {
					return fmt.Errorf("no duplicate reply came back over %d requests, a 40%% fault duplicated nothing",
						after.Sent)
				}
				if after.Loss != 0 {
					return fmt.Errorf("the loss is %.1f%%, a duplication fault should not lose probes", after.Loss)
				}
				if after.Received != after.Sent {
					return fmt.Errorf("the replies are %d for %d requests, a duplication fault should not lose any",
						after.Received, after.Sent)
				}
				return nil
			},
		},
		{
			kind: chaos.KindReordering,
			spec: chaos.Spec{
				Target: ends.Client, Kind: chaos.KindReordering,
				Parameters: chaos.Parameters{
					Reordering: 30,
					Latency:    chaos.Duration(40 * time.Millisecond),
					Gap:        2,
				},
			},

			count: 60, interval: 10 * time.Millisecond,
			worse: func(before, after probe.Measurement) error {

				if after.OutOfOrder == 0 {
					return fmt.Errorf("all %d replies arrived in order, reordering 30%% with a gap of 2 reordered nothing",
						after.Received)
				}
				if after.Received == 0 {
					return fmt.Errorf("no reply came back, a reordered path still delivers")
				}
				if after.MinRTT > before.MinRTT+60*time.Millisecond {
					return fmt.Errorf("the fastest reply is %s, far beyond the 40ms delay that was asked for",
						after.MinRTT)
				}
				return nil
			},
		},
	}

	for _, test := range cases {
		t.Run(string(test.kind), func(t *testing.T) {

			before := measureAt(t, lab, ends.Client, ends.Server, test.count, test.interval)
			report(t, string(test.kind)+" before", before)
			if before.Received != test.count || before.Loss != 0 {
				t.Fatalf("the path is not healthy to begin with: %+v", before)
			}

			fault, err := lab.Chaos().Apply(ctx, "gate-"+string(test.kind), test.spec)
			if err != nil {
				t.Fatalf("apply the %s fault: %v", test.kind, err)
			}
			if fault.Status != chaos.StatusActive || !fault.Verified {
				t.Fatalf("the fault is %q and verified=%v, want an active verified fault", fault.Status, fault.Verified)
			}
			defer func() {
				if _, err := lab.Chaos().Remove(fault.ID); err != nil {
					t.Errorf("remove the %s fault: %v", test.kind, err)
				}
			}()

			measured := measureAt(t, lab, ends.Client, ends.Server, test.count, test.interval)
			report(t, string(test.kind), measured)
			if err := test.worse(before, measured); err != nil {
				t.Errorf("the %s fault did not show up in the measurement: %v", test.kind, err)
			}

			if _, err := lab.Chaos().Remove(fault.ID); err != nil {
				t.Fatalf("remove: %v", err)
			}
			recovered := measureAt(t, lab, ends.Client, ends.Server, test.count, test.interval)
			report(t, string(test.kind)+" removed", recovered)
			if recovered.Received != test.count || recovered.Loss != 0 {
				t.Errorf("the path is still impaired after the fault was removed: %+v", recovered)
			}
			if recovered.MinRTT > 20*time.Millisecond {
				t.Errorf("the path is still %s slow after the fault was removed", recovered.MinRTT)
			}
		})
	}
}

func TestTheKernelAgreesWithEveryFault(t *testing.T) {
	lab, ends := chaosLab(t, "kernel")
	ctx := context.Background()

	cases := map[chaos.Kind]chaos.Spec{
		chaos.KindLatency: {
			Target: ends.Client, Kind: chaos.KindLatency,
			Parameters: chaos.Parameters{Latency: chaos.Duration(50 * time.Millisecond), Queue: 500},
		},
		chaos.KindJitter: {
			Target: ends.Client, Kind: chaos.KindJitter,
			Parameters: chaos.Parameters{
				Latency: chaos.Duration(50 * time.Millisecond), Jitter: chaos.Duration(10 * time.Millisecond),
			},
		},
		chaos.KindPacketLoss: {
			Target: ends.Client, Kind: chaos.KindPacketLoss,
			Parameters: chaos.Parameters{Loss: 10},
		},
		chaos.KindBandwidth: {
			Target: ends.Client, Kind: chaos.KindBandwidth,
			Parameters: chaos.Parameters{Rate: 500_000},
		},
		chaos.KindDuplication: {
			Target: ends.Client, Kind: chaos.KindDuplication,
			Parameters: chaos.Parameters{Duplication: 15},
		},
		chaos.KindReordering: {
			Target: ends.Client, Kind: chaos.KindReordering,
			Parameters: chaos.Parameters{
				Reordering: 20, Latency: chaos.Duration(30 * time.Millisecond), Gap: 3,
			},
		},
	}

	for kind, spec := range cases {
		t.Run(string(kind), func(t *testing.T) {
			fault, err := lab.Chaos().Apply(ctx, "kernel-agreement", spec)
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			defer func() { _, _ = lab.Chaos().Remove(fault.ID) }()

			if fault.Observed.Kind == "" || fault.Observed.Handle == "" {
				t.Fatalf("the record has no kernel evidence: %+v", fault.Observed)
			}

			if kind == chaos.KindReordering && fault.Observed.Gap == 0 {
				t.Error("the kernel reported no reorder gap at all")
			}
			if kind == chaos.KindBandwidth && fault.Observed.Rate == 0 {
				t.Error("the kernel reported no rate")
			}
			t.Logf("%-11s configured=%+v observed=%+v", kind, fault.Parameters, fault.Observed)
		})
	}
}

func TestFaultsAreScopedToTheLaboratory(t *testing.T) {
	lab, ends := chaosLab(t, "scope")
	ctx := context.Background()

	hostBefore := hostQdisc(t)
	if hostBefore == "" {
		t.Skip("the host interface cannot be inspected")
	}

	_, err := lab.Chaos().Apply(ctx, "scope", chaos.Spec{
		Target: "nf-host", Kind: chaos.KindLatency,
		Parameters: chaos.Parameters{Latency: chaos.Duration(50 * time.Millisecond)},
	})
	if err == nil {
		t.Fatal("a namespace outside the scope was accepted")
	}
	if !strings.Contains(err.Error(), "scope") {
		t.Errorf("got %v, want the refusal to mention the scope", err)
	}

	fault, err := lab.Chaos().Apply(ctx, "scope", chaos.Spec{
		Target: ends.Client, Kind: chaos.KindLatency,
		Parameters: chaos.Parameters{Latency: chaos.Duration(50 * time.Millisecond)},
	})
	if err != nil {
		t.Fatalf("apply inside the scope: %v", err)
	}
	if _, err := lab.Chaos().Remove(fault.ID); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if after := hostQdisc(t); after != hostBefore {
		t.Errorf("the host queue discipline changed from %q to %q", hostBefore, after)
	}
}

func hostQdisc(t *testing.T) string {
	t.Helper()
	output, err := exec.Command("tc", "-j", "qdisc", "show", "dev", "lo").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func TestTheLaboratoryTakesItsFaultsWithIt(t *testing.T) {
	spec := granted(labSpec("owned"))
	spec.Chaos = chaos.Limits{MaxFaults: 4, MaxDuration: time.Minute, MaxLatency: time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	lab, err := network.Create(ctx, spec)
	if err != nil {
		t.Fatalf("network.Create: %v", err)
	}
	_, from, err := lab.Client(0)
	if err != nil {
		t.Fatal(err)
	}
	fault, err := lab.Chaos().Apply(ctx, "owned", chaos.Spec{
		Target: from.Namespace, Kind: chaos.KindPacketLoss,
		Parameters: chaos.Parameters{Loss: 50},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := lab.Destroy(ctx); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if lab.Chaos().Active() != 0 {
		t.Errorf("the engine still reports %d active faults after the laboratory was destroyed", lab.Chaos().Active())
	}
	record, ok := lab.Chaos().Get(fault.ID)
	if !ok {
		t.Fatal("the record of the fault was thrown away")
	}
	if record.Status != chaos.StatusRemoved {
		t.Errorf("got status %q, want %q", record.Status, chaos.StatusRemoved)
	}
}

func TestAConfigurationFileCanDriveRealFaults(t *testing.T) {
	document := `name: file-driven
scope:
  network: 10.47.0.0/24
  namespaces:
    - nf-client0-file
    - nf-server0-file
    - nf-router-file
limits:
  max_duration: 2m
  max_bandwidth: 1Gbps
faults:
  - target: nf-client0-file
    type: latency
    value: 60ms
    duration: 1m
  - target: nf-client0-file
    direction: "in"
    type: packet_loss
    value: 20%
    duration: 1m
`
	cfg, err := config.Parse([]byte(document))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}

	spec := granted(labSpec("file"))
	spec.Network = "10.47.0.0/24"
	spec.Probes = probe.Limits{
		MaxProbes: 100, MaxRate: 200, MaxSize: 1400,

		MaxDuration: 90 * time.Second, MaxConcurrent: 2,
	}
	spec.Faults = translateFaults(t, cfg)
	if len(spec.Faults) != 2 {
		t.Fatalf("got %d specifications, want 2", len(spec.Faults))
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

	declared := lab.DeclaredFaults()
	if len(declared) != 2 {
		t.Fatalf("the laboratory holds %d declared faults, want 2", len(declared))
	}
	for _, fault := range declared {
		if !fault.Verified || fault.Status != chaos.StatusActive {
			t.Errorf("the declared %s fault is %q and verified=%v", fault.Kind, fault.Status, fault.Verified)
		}
		if fault.Experiment != spec.ID {
			t.Errorf("the fault is recorded under experiment %q, want %q", fault.Experiment, spec.ID)
		}
	}

	_, from, err := lab.Client(0)
	if err != nil {
		t.Fatal(err)
	}
	_, to, err := lab.Server(0)
	if err != nil {
		t.Fatal(err)
	}

	measured := measure(t, lab, from.Namespace, to.Address.Addr().String(), 100)
	report(t, "from the file", measured)
	if measured.MinRTT < 50*time.Millisecond {
		t.Errorf("the fastest reply is %s, the 60ms delay in the file is not there", measured.MinRTT)
	}

	if measured.Loss < 5 || measured.Loss > 40 {
		t.Errorf("the measured loss is %.1f%%, the file asked for 20%%: %+v", measured.Loss, measured)
	}
	if measured.Errors != 0 {
		t.Errorf("the path returned %d errors, a lossy path is not a broken one", measured.Errors)
	}
}

func translateFaults(t *testing.T, cfg *config.Config) []chaos.Spec {
	t.Helper()
	specs := make([]chaos.Spec, 0, len(cfg.Faults))
	for i, fault := range cfg.Faults {
		spec, err := fault.Translate(cfg.Limits)
		if err != nil {
			t.Fatalf("faults[%d].Translate: %v", i, err)
		}
		specs = append(specs, spec)
	}
	return specs
}

func TestALaboratoryRefusesAnImpossibleFaultSet(t *testing.T) {
	spec := granted(labSpec("impossible"))
	spec.Faults = []chaos.Spec{
		{
			Target: "nf-client0-impossible", Kind: chaos.KindLatency,
			Parameters: chaos.Parameters{Latency: chaos.Duration(50 * time.Millisecond)},
		},
		{

			Target: "nf-client0-somewhere-else", Kind: chaos.KindPacketLoss,
			Parameters: chaos.Parameters{Loss: 10},
		},
	}
	lab, err := network.Create(context.Background(), spec)
	if err == nil {
		_ = lab.Destroy(context.Background())
		t.Fatal("network.Create accepted a fault outside the laboratory")
	}
	if !strings.Contains(err.Error(), "removed again") {
		t.Errorf("got %v, want the refusal to say the applied faults were taken back out", err)
	}

	if strings.Contains(err.Error(), "could not be removed") {
		t.Errorf("got %v, want every applied fault to be removable", err)
	}
}
