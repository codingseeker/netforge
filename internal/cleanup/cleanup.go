package cleanup

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/netforge/netforge/internal/apperr"
)

const (
	DefaultTimeout = 30 * time.Second
	MaxTimeout     = 10 * time.Minute
)

type Func func(context.Context) error

type entry struct {
	name string
	fn   Func
}

type result struct {
	ran bool
	err error
}

type Stack struct {
	mu      sync.Mutex
	entries []entry
	timeout time.Duration
	outcome result
}

func New() *Stack {
	return &Stack{timeout: DefaultTimeout}
}

func NewWithTimeout(timeout time.Duration) (*Stack, error) {
	if timeout <= 0 {
		return nil, apperr.InvalidArgument("cleanup.NewWithTimeout", "timeout must be positive", nil)
	}
	if timeout > MaxTimeout {
		return nil, apperr.LimitExceeded("cleanup.NewWithTimeout", fmt.Sprintf("timeout %s exceeds maximum %s", timeout, MaxTimeout), nil)
	}
	return &Stack{timeout: timeout}, nil
}

func (s *Stack) Timeout() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.timeout
}

func (s *Stack) Defer(name string, fn Func) error {
	if name == "" {
		return apperr.InvalidArgument("cleanup.Defer", "cleanup action name must not be empty", nil)
	}
	if fn == nil {
		return apperr.InvalidArgument("cleanup.Defer", fmt.Sprintf("cleanup action %q must not be nil", name), nil)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.outcome.ran {
		return apperr.Conflict("cleanup.Defer", fmt.Sprintf("cleanup action %q registered after cleanup started", name), nil)
	}
	s.entries = append(s.entries, entry{name: name, fn: fn})
	return nil
}

func (s *Stack) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

func (s *Stack) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	s.mu.Lock()
	if s.outcome.ran {
		outcome := s.outcome
		s.mu.Unlock()
		return outcome.err
	}
	s.outcome = result{ran: true}
	pending := s.entries
	timeout := s.timeout
	s.entries = nil
	s.mu.Unlock()

	if len(pending) == 0 {
		s.mu.Lock()
		s.outcome.err = nil
		outcome := s.outcome
		s.mu.Unlock()
		return outcome.err
	}

	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	var errs []error
	for i := len(pending) - 1; i >= 0; i-- {
		current := pending[i]
		if err := runAction(runCtx, current); err != nil {
			errs = append(errs, err)
		}
	}

	joined := errors.Join(errs...)
	s.mu.Lock()
	s.outcome.err = joined
	s.mu.Unlock()
	return joined
}

func (s *Stack) Guard(ctx context.Context, fn func() error) (err error) {
	panicked := true
	defer func() {
		recovered := recover()
		cleanupErr := s.Run(ctx)
		if panicked && recovered != nil {
			err = errors.Join(
				apperr.Internal("cleanup.Guard", fmt.Sprintf("panic recovered: %v", recovered), nil),
				cleanupErr,
			)
			return
		}
		err = errors.Join(err, cleanupErr)
	}()

	err = fn()
	panicked = false
	return err
}

func runAction(ctx context.Context, e entry) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = apperr.Internal("cleanup.Run", fmt.Sprintf("cleanup action %q panicked: %v", e.name, recovered), nil)
		}
	}()

	if err := e.fn(ctx); err != nil {
		return apperr.Internal("cleanup.Run", fmt.Sprintf("cleanup action %q failed", e.name), err)
	}
	return nil
}
