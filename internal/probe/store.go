package probe

import (
	"sync"

	"github.com/netforge/netforge/internal/apperr"
)

type Store struct {
	mu      sync.Mutex
	results []Result
	limit   int
	dropped int
}

func NewStore(limit int) (*Store, error) {
	const op = "probe.NewStore"
	if limit <= 0 {
		return nil, apperr.Validation(op, "a result store needs a positive limit", nil)
	}
	return &Store{limit: limit, results: make([]Result, 0, min(limit, 64))}, nil
}

func (s *Store) Add(result Result) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results = append(s.results, result)
	if len(s.results) <= s.limit {
		return s.dropped
	}

	overflow := len(s.results) - s.limit
	copy(s.results, s.results[overflow:])
	s.results = s.results[:s.limit]
	s.dropped += overflow
	return s.dropped
}

func (s *Store) Results() []Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Result, len(s.results))
	copy(out, s.results)
	return out
}

func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.results)
}

func (s *Store) Dropped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

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

func (s *Store) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results = s.results[:0]
	s.dropped = 0
}
