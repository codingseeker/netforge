// The reliability half of the Phase 12 audit. The security audit proves the
// boundaries before privileged work and the behavioural audit proves what an
// interruption leaves behind; this file proves the third thing the phase asks
// for, which is that the same laboratory can be built and torn down again and
// again without the host drifting.
//
// Every check here is a comparison against the host as it was before the
// laboratory existed, taken over four resources by name. A leak that is only
// ever reported as "something is left" cannot be told from a leak that was
// always there, so each resource is asked about separately and the answer is
// compared with the answer from before.
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

// staleResource is one kind of object the host could be left carrying. The
// inspection is asked for by name so a failure says what leaked rather than
// that something did.
type staleResource struct {
	kind   string
	lookup [][]string
}

// hostResources are the four things a laboratory can leave behind on a host it
// was supposed to be invisible to.
var hostResources = []staleResource{
	{kind: "namespace", lookup: [][]string{{"ip", "netns", "list"}}},
	{kind: "interface", lookup: [][]string{{"ip", "-o", "link", "show"}}},
	{kind: "route", lookup: [][]string{{"ip", "-o", "route", "show"}}},
	{kind: "tc rule", lookup: [][]string{{"tc", "qdisc", "show"}, {"tc", "filter", "show", "dev", "lo"}}},
}

// readResource returns every line the host's own tools report for one kind of
// object. A tool that will not answer yields nothing: the report is about what
// the laboratory left, and a host that cannot be asked is not evidence of a
// leak.
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

// staleAfter reports, for each kind of object, the lines that name the laboratory
// and were not there before it was built.
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

// reliabilitySpec is the laboratory each round builds: a client, a router and a
// server, with a fault to apply and remove and a measurement to take, so that a
// round exercises the whole lifecycle rather than only the teardown.
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

// TestRepeatedBuildAndTeardownLeaveNothingBehind builds the same laboratory
// several times in a row, measures it, applies a fault, removes the fault and
// destroys it, and asks the host after every round whether anything carrying the
// laboratory's identity appeared. The repetition is the point: a teardown that
// leaks only on the third round, or only when a fault was in place, passes a
// suite that only runs once.
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
			// The teardown runs even if a step below fails, and its own failure
			// is reported: a laboratory left half removed is worse than one that
			// was never built.
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

			// The laboratory is taken apart here rather than in the deferred
			// call, so the host is asked about it while it is still down and the
			// deferred call then proves that a second teardown is safe.
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

// TestAFailedLaboratoryIsTornDownAsCompletelyAsASuccessfulOne is the other half
// of repetition: a laboratory that failed part way through must leave the same
// nothing behind as one that succeeded. A teardown that runs on the happy path
// and is skipped on the failing one is the leak that a single successful run
// never shows.
func TestAFailedLaboratoryIsTornDownAsCompletelyAsASuccessfulOne(t *testing.T) {
	resourcesBefore := snapshotResources(t)

	for round, break_ := range []string{"a namespace nobody granted", "more namespaces than the budget"} {
		t.Run(break_, func(t *testing.T) {
			spec := reliabilitySpec(t)
			switch round {
			case 0:
				// A name the laboratory was not granted: refused at the boundary,
				// so nothing was created at all.
				spec.Names = []string{"nf-somebody-elses-lab"}
			case 1:
				// A budget too small for the laboratory the document describes.
				spec.MaxNames = 2
			}
			lab, err := network.Create(context.Background(), spec)
			if err == nil {
				// A laboratory that should not have been built must not be left
				// behind just because it turned out to be buildable.
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

// describe renders a stale-resource report by kind, so a failure names what
// leaked rather than only that something did.
func describe(stale map[string][]string) string {
	var out []string
	for _, resource := range hostResources {
		for _, line := range stale[resource.kind] {
			out = append(out, "  "+resource.kind+": "+line)
		}
	}
	return strings.Join(out, "\n")
}
