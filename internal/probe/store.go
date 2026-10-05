package probe

import (
	"sync"

	"github.com/netforge/netforge/internal/apperr"
)

// Store keeps the results of a laboratory's probes. It is bounded twice over:
// by the number of results it holds and by the number of bytes any one result
// may carry, so neither a long laboratory nor a hostile answer can make it grow
// without limit. The oldest result is dropped to make room, and the drop is
// reported rather than hidden.
type Store struct {
	mu      sync.Mutex
	results []Result
	limit   int
	dropped int
}

// NewStore builds a store that keeps at most limit results.
func NewStore(limit int) (*Store, error) {
	const op = "probe.NewStore"
	if limit <= 0 {
		return nil, apperr.Validation(op, "a result store needs a positive limit", nil)
	}
	return &Store{limit: limit, results: make([]Result, 0, min(limit, 64))}, nil
}

// Add records a result and returns how many results have been dropped to make
// room for it.
func (s *Store) Add(result Result) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results = append(s.results, result)
	if len(s.results) <= s.limit {
		return s.dropped
	}
	// Trim in one step rather than one at a time, so a full store costs the
	// same as an empty one.
	overflow := len(s.results) - s.limit
	copy(s.results, s.results[overflow:])
	s.results = s.results[:s.limit]
	s.dropped += overflow
	return s.dropped
}

// Results returns the stored results, oldest first.
func (s *Store) Results() []Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Result, len(s.results))
	copy(out, s.results)
	return out
}

// Len returns how many results are stored.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.results)
}

// Dropped returns how many results were discarded to stay within the limit.
func (s *Store) Dropped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// Latest returns the most recent result for a protocol, if there is one.
func (s *Store) Latest(protocol Protocol) (Result, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.results) - 1; i >= 0; i-- {
		if s.results[i].Protocol == protocol {
			return s.results[i], true
		}
	}
	return Result{}, false
}

// Reset empties the store and forgets the dropped count.
func (s *Store) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results = s.results[:0]
	s.dropped = 0
}
