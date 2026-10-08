package loadbalancer

import (
	"fmt"
	"hash/fnv"
	"sort"

	"github.com/codingseeker/netforge/internal/apperr"
)

type Policy string

const (
	PolicyRoundRobin Policy = "round_robin"

	PolicyLeastConnections Policy = "least_connections"

	PolicyConsistentHash Policy = "consistent_hash"
)

func (p Policy) Valid() bool {
	switch p {
	case PolicyRoundRobin, PolicyLeastConnections, PolicyConsistentHash:
		return true
	default:
		return false
	}
}

func (p Policy) String() string { return string(p) }

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

func (b *Balancer) reserve(key string) *backend {
	b.mu.Lock()
	defer b.mu.Unlock()
	candidates := make([]*backend, 0, len(b.backends))
	for _, candidate := range b.backends {
		if !candidate.healthy {
			continue
		}
		if candidate.active >= b.limits.MaxBackendConnections {

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

type point struct {
	hash uint64

	index int
}

func (b *Balancer) onRing(key string, candidates []*backend) *backend {

	if !sameBackends(b.ringFor, candidates) {
		b.ring = buildRing(candidates, b.limits.Replicas)
		b.ringFor = addresses(candidates)
	}
	if len(b.ring) == 0 {
		return nil
	}
	target := hashKey(key)

	start := sort.Search(len(b.ring), func(i int) bool { return b.ring[i].hash >= target })
	return candidates[b.ring[start%len(b.ring)].index]
}

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

func addresses(candidates []*backend) []string {
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.address)
	}
	return out
}

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
