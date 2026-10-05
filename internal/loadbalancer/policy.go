package loadbalancer

import (
	"fmt"
	"hash/fnv"
	"sort"

	"github.com/netforge/netforge/internal/apperr"
)

// Policy decides which backend takes the next connection. A policy only chooses;
// whether a backend may be chosen at all is decided by its health state, so a
// policy cannot route to a backend that failed, and a policy can never widen
// what the limits allow.
type Policy string

const (
	// PolicyRoundRobin takes the next healthy backend in turn. It is the policy
	// that assumes every backend is the same, which is what a laboratory
	// measuring a path rather than a fleet wants.
	PolicyRoundRobin Policy = "round_robin"
	// PolicyLeastConnections takes the healthy backend with the fewest open
	// connections, which is what a backend that answers at different speeds
	// needs.
	PolicyLeastConnections Policy = "least_connections"
	// PolicyConsistentHash takes the backend the key lands on in a ring, so a
	// client keeps reaching the same backend and adding or removing one moves
	// only the keys that belonged to it.
	PolicyConsistentHash Policy = "consistent_hash"
)

// Valid reports whether the policy is one this build implements.
func (p Policy) Valid() bool {
	switch p {
	case PolicyRoundRobin, PolicyLeastConnections, PolicyConsistentHash:
		return true
	default:
		return false
	}
}

// String returns the name a configuration uses.
func (p Policy) String() string { return string(p) }

// ParsePolicy turns a configured name into a policy, refusing a name this build
// does not implement rather than falling back to a default: a document that
// asks for a policy it will not get is a document whose measurements describe
// something other than what it asked for.
func ParsePolicy(name string) (Policy, error) {
	const op = "loadbalancer.ParsePolicy"
	policy := Policy(name)
	if !policy.Valid() {
		return "", apperr.InvalidArgument(op, fmt.Sprintf(
			"%q is not a policy; the policies are %q, %q and %q", name,
			PolicyRoundRobin, PolicyLeastConnections, PolicyConsistentHash), nil)
	}
	return policy, nil
}

// reserve chooses the backend that should take the next connection and takes one
// of its connection slots, or returns nil when none of them may take it.
//
// The slot is taken under the same lock as the choice, which is what makes the
// per-backend limit exact: two clients chosen at the same moment cannot both be
// placed on a backend that has room for one, because the second choice sees the
// first slot. A caller that fails to place the connection gives the slot back
// with release.
//
// The candidates are the healthy backends that are under their own connection
// limit, and the rotation cursor is advanced past the one that was chosen, so two
// calls in a row do not return the same backend even when another was skipped.
// Which backend a key lands on is therefore fair for the two counting policies
// and stable for the hashing one, and it is the same across restarts of the same
// configuration, because the order is the registration order.
func (b *Balancer) reserve(key string) *backend {
	b.mu.Lock()
	defer b.mu.Unlock()
	candidates := make([]*backend, 0, len(b.backends))
	for _, candidate := range b.backends {
		if !candidate.healthy {
			continue
		}
		if candidate.active >= b.limits.MaxBackendConnections {
			// A backend at its own limit is not a candidate, and the connection
			// is counted against it as passed over rather than refused, because
			// the balancer did serve the client somewhere else.
			candidate.stats.Skipped++
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		return nil
	}

	var chosen *backend
	switch b.config.Policy {
	case PolicyLeastConnections:
		chosen = b.leastLoaded(candidates)
	case PolicyConsistentHash:
		chosen = b.onRing(key, candidates)
	}
	if chosen == nil {
		// Round robin, and the fallback for a ring that could not be built, walk
		// the candidates from the cursor so that a balancer with three equal
		// backends spreads the load instead of sending everything to the first
		// one it finds.
		index := b.cursor % len(candidates)
		if index < 0 {
			index += len(candidates)
		}
		chosen = candidates[index]
		b.cursor = (index + 1) % len(candidates)
	}
	chosen.active++
	return chosen
}

// release gives a reserved slot back, for a client that could not be placed after
// a backend had been chosen.
func (b *Balancer) release(target *backend) {
	if target == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if target.active > 0 {
		target.active--
	}
}

// leastLoaded takes the backend with the fewest open connections, breaking a tie
// from the rotation cursor so that backends with equal load are still visited in
// turn instead of always the same one.
func (b *Balancer) leastLoaded(candidates []*backend) *backend {
	chosen := -1
	for offset := 0; offset < len(candidates); offset++ {
		index := (b.cursor + offset) % len(candidates)
		if index < 0 {
			index += len(candidates)
		}
		if chosen < 0 || candidates[index].active < candidates[chosen].active {
			chosen = index
		}
	}
	if chosen < 0 {
		return nil
	}
	b.cursor = (chosen + 1) % len(candidates)
	return candidates[chosen]
}

// point is one position on a consistent hash ring.
type point struct {
	// hash is where the point sits, and the ring is ordered by it.
	hash uint64
	// index is the backend in the balancer's own order that the point belongs
	// to, so a backend with many points still occupies one slot in the ring
	// table.
	index int
}

// onRing returns the backend a key lands on. The ring holds only the candidates,
// so a key cannot land on a backend that is unhealthy, full, or excluded by an
// earlier failure in the same attempt: it is not on the ring to land on.
func (b *Balancer) onRing(key string, candidates []*backend) *backend {
	// The ring is built from the candidate list, whose positions are the indices
	// its points carry. That is only true while the list is the one the ring was
	// built from, so the two are compared before the ring is used. A change to
	// the healthy set is rare and a rebuild is a sort of a few thousand points,
	// which is cheaper than getting it wrong.
	if !sameBackends(b.ringFor, candidates) {
		b.ring = buildRing(candidates, b.limits.Replicas)
		b.ringFor = addresses(candidates)
	}
	if len(b.ring) == 0 {
		return nil
	}
	target := hashKey(key)
	// The first point at or after the key's position, wrapping to the start of
	// the ring, so every key lands somewhere and a key past the last point does
	// not fall off the end of it.
	start := sort.Search(len(b.ring), func(i int) bool { return b.ring[i].hash >= target })
	return candidates[b.ring[start%len(b.ring)].index]
}

// sameBackends reports whether a ring was built from exactly this set of
// backends, in this order.
func sameBackends(built []string, candidates []*backend) bool {
	if len(built) != len(candidates) {
		return false
	}
	for index, candidate := range candidates {
		if built[index] != candidate.address {
			return false
		}
	}
	return true
}

// addresses lists the backends a ring was built from.
func addresses(candidates []*backend) []string {
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.address)
	}
	return out
}

// buildRing places every candidate the given number of times and returns the
// points in order. The number of points is bounded by the replica ceiling and
// the backend ceiling, so the ring is a table this balancer sized rather than
// one a caller can make it allocate.
func buildRing(candidates []*backend, replicas int) []point {
	ring := make([]point, 0, len(candidates)*replicas)
	for index, candidate := range candidates {
		for replica := 0; replica < replicas; replica++ {
			ring = append(ring, point{
				hash:  hashKey(fmt.Sprintf("%s#%d", candidate.address, replica)),
				index: index,
			})
		}
	}
	sort.Slice(ring, func(i, j int) bool {
		if ring[i].hash != ring[j].hash {
			return ring[i].hash < ring[j].hash
		}
		return ring[i].index < ring[j].index
	})
	return ring
}

// hashKey is FNV-1a over the key, finished with a splitmix64 finalizer. The ring
// needs a hash that is stable across processes and across restarts, because a
// client that keeps reaching the same backend is the property this policy is for,
// and a hash seeded per process would move every key every time the balancer was
// started again.
//
// The finalizer is not decoration. FNV's last step barely mixes the high bits, and
// the strings a ring is built from differ only in a small suffix (the address and
// the replica number), so their hashes come out clustered: without the finalizer
// the points of one backend end up next to each other, one backend owns a long arc
// of the ring, and the keys land on it in proportion to that arc rather than in
// proportion to the backends. It is a spreading property, not a security one.
func hashKey(key string) uint64 {
	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte(key))
	hash := hasher.Sum64()
	hash ^= hash >> 30
	hash *= 0xbf58476d1ce4e5b9
	hash ^= hash >> 27
	hash *= 0x94d049bb133111eb
	hash ^= hash >> 31
	return hash
}
