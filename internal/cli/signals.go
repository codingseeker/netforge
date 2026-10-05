package cli

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

type SignalContext struct {
	context context.Context
	cancel  context.CancelFunc

	signals  chan os.Signal
	finished chan struct{}
	stopOnce sync.Once
	stopped  chan struct{}
	mu       sync.Mutex
	received os.Signal
}

func NotifySignals(parent context.Context, signals ...os.Signal) *SignalContext {
	if parent == nil {
		parent = context.Background()
	}
	if len(signals) == 0 {
		signals = []os.Signal{os.Interrupt, syscall.SIGTERM}
	}

	ctx, cancel := context.WithCancel(parent)
	sc := &SignalContext{
		context:  ctx,
		cancel:   cancel,
		signals:  make(chan os.Signal, 1),
		finished: make(chan struct{}),
		stopped:  make(chan struct{}),
	}
	signal.Notify(sc.signals, signals...)

	go func() {
		defer close(sc.finished)
		select {
		case received := <-sc.signals:
			sc.mu.Lock()
			sc.received = received
			sc.mu.Unlock()
			cancel()
			signal.Stop(sc.signals)
		case <-sc.stopped:
		}
	}()

	return sc
}

func (s *SignalContext) Context() context.Context {
	return s.context
}

func (s *SignalContext) Received() os.Signal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.received
}

func (s *SignalContext) ExitCode(err error) int {
	s.mu.Lock()
	received := s.received
	s.mu.Unlock()
	if received != nil {
		return SignalExitCode(received)
	}
	return ExitCode(err)
}

func (s *SignalContext) Stop() {
	s.stopOnce.Do(func() {
		signal.Stop(s.signals)
		close(s.stopped)
		s.cancel()
	})
	<-s.finished
}
