package loadbalancer

import (
	"errors"
	"fmt"
	"strconv"
	"testing"
)

func mustBalancer(t *testing.T, config Config) *Balancer {
	t.Helper()
	balancer, err := New(config)
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	t.Cleanup(func() { _ = balancer.Close() })
	return balancer
}

func TestTheRoundRobinWalksTheBackendsInOrder(t *testing.T) {
	t.Parallel()
	balancer := mustBalancer(t, testConfig(PolicyRoundRobin, "10.0.0.1:1", "10.0.0.2:2", "10.0.0.3:3"))
	want := []string{"10.0.0.1:1", "10.0.0.2:2", "10.0.0.3:3", "10.0.0.1:1", "10.0.0.2:2", "10.0.0.3:3", "10.0.0.1:1"}
	for index, address := range want {
		chosen := balancer.reserve("key")
		if chosen == nil {
			t.Fatalf("client %d was given no backend, want %s", index, address)
		}
		if chosen.address != address {
			t.Errorf("client %d went to %s, want %s: the round robin is in order and repeats",
				index, chosen.address, address)
		}
		balancer.release(chosen)
	}
}

func TestTheLeastConnectionsPolicyTakesTheBackendWithFewestConnections(t *testing.T) {
	t.Parallel()
	addresses := []string{"10.0.0.1:1", "10.0.0.2:2", "10.0.0.3:3"}
	roundRobin := mustBalancer(t, testConfig(PolicyRoundRobin, addresses...))
	least := mustBalancer(t, testConfig(PolicyLeastConnections, addresses...))

	holdConnection(roundRobin, 0, 5)
	holdConnection(least, 0, 5)

	rotated := roundRobin.reserve("key")
	if rotated == nil || rotated.address != addresses[0] {
		t.Fatalf("the round robin gave the next client %v, want %s: it walks in order and does not look at the load",
			rotated, addresses[0])
	}
	counted := least.reserve("key")
	if counted == nil {
		t.Fatal("the least connections policy gave no backend, want the emptiest one")
	}
	if counted.address == addresses[0] {
		t.Errorf("the least connections policy added to the backend that already held %d connections, want one of the two that held none",
			counted.active-1)
	}
}

func TestTheLeastConnectionsPolicyRespectsThePerBackendLimit(t *testing.T) {
	t.Parallel()
	addresses := []string{"10.0.0.1:1", "10.0.0.2:2", "10.0.0.3:3"}
	config := testConfig(PolicyLeastConnections, addresses...)
	config.Limits.MaxBackendConnections = 2
	balancer := mustBalancer(t, config)

	counts := make(map[string]int, len(addresses))
	for range 6 {
		chosen := balancer.reserve("key")
		if chosen == nil {
			t.Fatal("reserve() gave no backend, want one: there were two slots per backend")
		}
		counts[chosen.address]++
	}
	for address, count := range counts {
		if count > 2 {
			t.Errorf("backend %s was given %d clients, want no more than its limit of 2", address, count)
		}
	}
	if len(counts) != 3 {
		t.Errorf("the clients went to %d backends, want all 3: the least loaded one is the point", len(counts))
	}
}

func TestTheConsistentHashPolicyIsStableAndSpread(t *testing.T) {
	t.Parallel()
	addresses := []string{"10.0.0.1:1", "10.0.0.2:2", "10.0.0.3:3"}
	balancer := mustBalancer(t, testConfig(PolicyConsistentHash, addresses...))

	const keys = 600
	chosen := make(map[string]string, keys)
	counts := make(map[string]int, len(addresses))
	for index := range keys {
		key := "session-" + strconv.Itoa(index)
		first := balancer.reserve(key)
		again := balancer.reserve(key)
		if first == nil || again == nil {
			t.Fatalf("key %q was given no backend, want one of the three", key)
		}
		if first.address != again.address {
			t.Fatalf("key %q went to %s and then to %s, want the same backend both times",
				key, first.address, again.address)
		}
		chosen[key] = first.address
		counts[first.address]++
		balancer.release(first)
		balancer.release(again)
	}
	for _, address := range addresses {
		if counts[address] == 0 {
			t.Errorf("backend %s was given no key out of %d, want the keys spread over the backends",
				address, keys)
		}

		if counts[address] < 120 || counts[address] > 280 {
			t.Errorf("backend %s was given %d of %d keys, want roughly a third", address, counts[address], keys)
		}
	}

	other := mustBalancer(t, testConfig(PolicyConsistentHash, addresses...))
	for key, address := range chosen {
		second := other.reserve(key)
		if second == nil {
			t.Fatalf("key %q was given no backend on the second balancer", key)
		}
		if second.address != address {
			t.Fatalf("key %q went to %s on a second balancer, want the %s the first one gave it",
				key, second.address, address)
		}
		other.release(second)
	}
}

func TestTakingABackendOutOfServiceMovesOnlyItsKeys(t *testing.T) {
	t.Parallel()
	addresses := []string{"10.0.0.1:1", "10.0.0.2:2", "10.0.0.3:3"}
	balancer := mustBalancer(t, testConfig(PolicyConsistentHash, addresses...))

	const keys = 600
	before := make(map[string]string, keys)
	onSecond := 0
	for index := range keys {
		key := "session-" + strconv.Itoa(index)
		chosen := balancer.reserve(key)
		if chosen == nil {
			t.Fatalf("key %q was given no backend", key)
		}
		before[key] = chosen.address
		if chosen.address == addresses[1] {
			onSecond++
		}
		balancer.release(chosen)
	}
	if onSecond == 0 {
		t.Fatalf("no key of %d landed on %s, want the second backend to carry some of them", keys, addresses[1])
	}

	balancer.mu.Lock()
	balancer.backends[1].healthy = false
	balancer.mu.Unlock()

	moved, stayed := 0, 0
	for key, address := range before {
		chosen := balancer.reserve(key)
		if chosen == nil {
			t.Fatalf("key %q was given no backend, want the two that are still serving", key)
		}
		if chosen.address == addresses[1] {
			t.Errorf("key %q still went to the backend that was taken out of service", key)
		}
		switch {
		case chosen.address == address:
			stayed++
		case address != addresses[1]:
			t.Errorf("key %q moved from %s, want only the keys of the backend that went away to move",
				key, address)
		default:
			moved++
		}
		balancer.release(chosen)
	}
	if moved != onSecond {
		t.Errorf("%d keys moved, want the %d that were on the backend that went away", moved, onSecond)
	}
	if stayed == 0 {
		t.Error("every key moved, want the keys of the backends that stayed to keep their backend")
	}
}

func TestNoBackendAvailableIsReportedAsSuch(t *testing.T) {
	t.Parallel()
	balancer := mustBalancer(t, testConfig(PolicyRoundRobin, "10.0.0.1:1", "10.0.0.2:2"))
	balancer.mu.Lock()
	balancer.backends[0].healthy = false
	balancer.backends[1].healthy = false
	balancer.mu.Unlock()

	if chosen := balancer.reserve("key"); chosen != nil {
		t.Fatalf("reserve() = %s, want nothing while every backend is out of service", chosen.address)
	}

	if _, _, err := balancer.place("key"); !errors.Is(err, ErrNoBackend) {
		t.Errorf("place() = %v, want %v", err, ErrNoBackend)
	}
}

func TestAPolicyIsCheckedBeforeAnythingIsServed(t *testing.T) {
	t.Parallel()
	if _, err := New(testConfig("policy_from_the_future", "10.0.0.1:1")); err == nil {
		t.Error("New() with an unknown policy = nil, want a refusal")
	} else if _, err := ParsePolicy("policy_from_the_future"); err == nil {
		t.Error("ParsePolicy() with an unknown policy = nil, want a refusal")
	}
	balancer := mustBalancer(t, testConfig("", "10.0.0.1:1"))
	if balancer.Policy() != PolicyRoundRobin {
		t.Errorf("policy = %s, want %s for a document that named none", balancer.Policy(), PolicyRoundRobin)
	}
	for _, policy := range []Policy{PolicyRoundRobin, PolicyLeastConnections, PolicyConsistentHash} {
		if !policy.Valid() {
			t.Errorf("%s.Valid() = false, want true", policy)
		}
		parsed, err := ParsePolicy(policy.String())
		if err != nil || parsed != policy {
			t.Errorf("ParsePolicy(%q) = %s, %v, want %s, nil", policy, parsed, err, policy)
		}

		if formatted := fmt.Sprint(policy); formatted != policy.String() {
			t.Errorf("%%v of %s = %q, want %q", policy, formatted, policy.String())
		}
	}
	if Policy("").Valid() {
		t.Error("the empty policy reports itself valid, want false")
	}
	if Policy("").String() != "" {
		t.Errorf("the empty policy reports itself as %q, want the empty string", Policy("").String())
	}
}

func TestTheHashIsTheDocumentedOne(t *testing.T) {
	t.Parallel()
	for _, pair := range []struct {
		key  string
		want uint64
	}{
		{"", 0x0f52a15e9a9b5e89b},
		{"a", 0x2c0bdbf481420f8},
		{"foobar", 0x404da9e3b74078c2},
		{"10.0.0.1:1#0", 0x6ada41892e717f9},
	} {
		if got := hashKey(pair.key); got != pair.want {
			t.Errorf("hashKey(%q) = %#x, want %#x", pair.key, got, pair.want)
		}
	}
}

func TestTheRingIsBoundedAndCoversTheRegisteredBackends(t *testing.T) {
	t.Parallel()
	balancer := mustBalancer(t, testConfig(PolicyConsistentHash, "10.0.0.1:1", "10.0.0.2:2"))
	balancer.reserve("key")

	ring := balancer.ring
	if len(ring) == 0 {
		t.Fatal("the ring is empty, want it built from the backends")
	}
	if want := 2 * balancer.limits.Replicas; len(ring) != want {
		t.Errorf("the ring holds %d points, want %d: one per replica per backend", len(ring), want)
	}
	if len(ring) > MaxBackendCeiling*MaxReplicaCeiling {
		t.Errorf("the ring holds %d points, want no more than the product of the two ceilings", len(ring))
	}
	covered := make(map[string]bool)
	for _, spot := range ring {
		if spot.index < 0 || spot.index >= len(balancer.backends) {
			t.Fatalf("the ring holds a point for backend %d, want one of the %d that are registered",
				spot.index, len(balancer.backends))
		}
		covered[balancer.backends[spot.index].address] = true
	}
	if len(covered) != 2 {
		t.Errorf("the ring covers %d backends, want the 2 that were registered", len(covered))
	}

	for index := 1; index < len(ring); index++ {
		if ring[index].hash < ring[index-1].hash {
			t.Fatalf("the ring is out of order at %d, want the points sorted by hash", index)
		}
	}

	before := balancer.ringFor
	balancer.reserve("another key")
	if len(balancer.ringFor) != len(before) {
		t.Errorf("the ring was rebuilt from %d backends, want the same %d", len(balancer.ringFor), len(before))
	}
}

func TestAKeyPastTheEndOfTheRingWrapsOntoIt(t *testing.T) {
	t.Parallel()
	balancer := mustBalancer(t, testConfig(PolicyConsistentHash, "10.0.0.1:1", "10.0.0.2:2"))

	balancer.reserve("key")
	ring := balancer.ring
	last := ring[len(ring)-1].hash
	chosen := balancer.onRing(strconv.FormatUint(last+1, 10), balancer.backends)
	if chosen == nil {
		t.Fatalf("onRing() for a key past the last point = nil, want the first point on the ring")
	}
	if chosen != balancer.backends[0] && chosen != balancer.backends[1] {
		t.Errorf("onRing() = %v, want one of the registered backends", chosen)
	}
}
