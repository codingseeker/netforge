package integration

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/netforge/netforge/internal/chaos"
	"github.com/netforge/netforge/internal/network"
	"github.com/netforge/netforge/internal/probe"
)

const (
	reliabilityLabID  = "nf-ph12-rel"
	reliabilityCIDR   = "10.59.0.0/24"
	reliabilityRepeat = 4
)

type staleResource struct {
	kind   string
	lookup [][]string
}

var hostResources = []staleResource{
	{kind: "namespace", lookup: [][]string{{"ip", "netns", "list"}}},
	{kind: "interface", lookup: [][]string{{"ip", "-o", "link", "show"}}},
	{kind: "route", lookup: [][]string{{"ip", "-o", "route", "show"}}},
	{kind: "tc rule", lookup: [][]string{{"tc", "qdisc", "show"}, {"tc", "filter", "show", "dev", "lo"}}},
}

func readResource(t *testing.T, resource staleResource) []string {
	t.Helper()
	var lines []string
	for _, argv := range resource.lookup {
		out, err := exec.Command(argv[0], argv[1:]...).Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if strings.TrimSpace(line) != "" {
				lines = append(lines, line)
			}
		}
	}
	return lines
}

func staleAfter(t *testing.T, before map[string][]string) map[string][]string {
	t.Helper()
	var stale map[string][]string
	for _, resource := range hostResources {
		present := map[string]bool{}
		for _, line := range before[resource.kind] {
			present[line] = true
		}
		for _, line := range readResource(t, resource) {
			if present[line] {
				continue
			}
			if !strings.Contains(line, reliabilityLabID) {
				continue
			}
			if stale == nil {
				stale = map[string][]string{}
			}
			stale[resource.kind] = append(stale[resource.kind], line)
		}
	}
	return stale
}

func snapshotResources(t *testing.T) map[string][]string {
	t.Helper()
	before := map[string][]string{}
	for _, resource := range hostResources {
		before[resource.kind] = readResource(t, resource)
	}
	return before
}

func reliabilitySpec(t *testing.T) network.Spec {
	t.Helper()
	plan, err := network.BuildPlan(reliabilityLabID, reliabilityCIDR, 1, 1, 1500)
	if err != nil {
		t.Fatalf("BuildPlan() = %v, want nil", err)
	}
	return network.Spec{
		ID:      reliabilityLabID,
		Network: reliabilityCIDR,
		Clients: 1,
		Servers: 1,
		MTU:     1500,
		Names:   []string{plan.RouterNS, plan.Nodes[0].Namespace, plan.Nodes[1].Namespace},
		Chaos: chaos.Limits{
			MaxFaults: 4, MaxDuration: time.Minute, MaxLatency: time.Second,
			MaxBandwidth: 1_000_000, MaxQueue: 1024, MaxGap: 50,
		},
		Probes: probe.Limits{
			MaxProbes: 20, MaxRate: 100, MaxSize: 512,
			MaxDuration: 30 * time.Second, MaxConcurrent: 2,
		},
	}
}

func TestRepeatedBuildAndTeardownLeaveNothingBehind(t *testing.T) {
	hostBefore, err := hostState()
	if err != nil {
		t.Skipf("the host network state is not readable: %v", err)
	}
	resourcesBefore := snapshotResources(t)

	for round := 0; round < reliabilityRepeat; round++ {
		t.Run("round "+strconv.Itoa(round), func(t *testing.T) {
			spec := reliabilitySpec(t)
			lab, err := network.Create(context.Background(), spec)
			if err != nil {
				t.Fatalf("round %d: network.Create: %v", round, err)
			}

			defer func() {
				if err := lab.Destroy(context.Background()); err != nil {
					t.Errorf("round %d: destroy: %v", round, err)
				}
			}()

			plan := lab.Plan()
			client := plan.Nodes[0].Namespace
			server := plan.Nodes[1].Address.Addr().String()

			measurement, err := lab.Measure(context.Background(), client, server, probe.Options{
				Count: 4, Interval: 50 * time.Millisecond, Size: 64,
				Timeout: time.Second, Deadline: 10 * time.Second,
			})
			if err != nil {
				t.Fatalf("round %d: measure: %v", round, err)
			}
			if measurement.Received != measurement.Sent {
				t.Errorf("round %d: %d of %d probes came back on a healthy path",
					round, measurement.Received, measurement.Sent)
			}

			fault, err := lab.Chaos().Apply(context.Background(), "rel-"+strconv.Itoa(round), chaos.Spec{
				Target:    client,
				Kind:      chaos.KindPacketLoss,
				Direction: chaos.DirectionOut,
				Parameters: chaos.Parameters{
					Loss: 30,
				},
				Duration: chaos.Duration(time.Minute),
			})
			if err != nil {
				t.Fatalf("round %d: apply the fault: %v", round, err)
			}
			if _, err := lab.Chaos().Remove(fault.ID); err != nil {
				t.Fatalf("round %d: remove the fault: %v", round, err)
			}
			for _, remaining := range lab.Chaos().List() {
				if !remaining.Status.Finished() {
					t.Errorf("round %d: fault %s is still %q after it was removed", round, remaining.ID, remaining.Status)
				}
			}

			if err := lab.Destroy(context.Background()); err != nil {
				t.Fatalf("round %d: destroy: %v", round, err)
			}
			if err := lab.Destroy(context.Background()); err != nil {
				t.Errorf("round %d: destroying the laboratory a second time returned %v, want nil", round, err)
			}
		})
		if stale := staleAfter(t, resourcesBefore); len(stale) != 0 {
			t.Errorf("round %d left the host carrying:\n%s", round, describe(stale))
		}
	}

	hostAfter, err := hostState()
	if err != nil {
		t.Fatalf("the host network state is not readable: %v", err)
	}
	if hostBefore.links != hostAfter.links || hostBefore.routes != hostAfter.routes ||
		hostBefore.forwarding != hostAfter.forwarding {
		t.Errorf("after %d rounds the host is not the host it was:\nbefore %+v\nafter  %+v",
			reliabilityRepeat, hostBefore, hostAfter)
	}
}

func TestAFailedLaboratoryIsTornDownAsCompletelyAsASuccessfulOne(t *testing.T) {
	resourcesBefore := snapshotResources(t)

	for round, break_ := range []string{"a namespace nobody granted", "more namespaces than the budget"} {
		t.Run(break_, func(t *testing.T) {
			spec := reliabilitySpec(t)
			switch round {
			case 0:

				spec.Names = []string{"nf-somebody-elses-lab"}
			case 1:

				spec.MaxNames = 2
			}
			lab, err := network.Create(context.Background(), spec)
			if err == nil {

				if err := lab.Destroy(context.Background()); err != nil {
					t.Errorf("destroy: %v", err)
				}
				t.Fatalf("the laboratory was built from a specification that should have been refused")
			}
		})
		if stale := staleAfter(t, resourcesBefore); len(stale) != 0 {
			t.Errorf("the refused build left the host carrying:\n%s", describe(stale))
		}
	}
}

func describe(stale map[string][]string) string {
	var out []string
	for _, resource := range hostResources {
		for _, line := range stale[resource.kind] {
			out = append(out, "  "+resource.kind+": "+line)
		}
	}
	return strings.Join(out, "\n")
}
