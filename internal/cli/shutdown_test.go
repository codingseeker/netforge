package cli

import (
	"context"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/codingseeker/netforge/internal/apperr"
	"github.com/codingseeker/netforge/internal/cleanup"
)

func TestNotifySignalsCancelsContextSigterm(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	signals := NotifySignals(ctx, syscall.SIGTERM)
	defer signals.Stop()

	if err := signals.Context().Err(); err != nil {
		t.Fatalf("Context().Err() = %v before any signal, want nil", err)
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("Kill() = %v, want nil", err)
	}

	select {
	case <-signals.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("context was not canceled within the timeout after SIGTERM")
	}

	if got := signals.Received(); got != syscall.SIGTERM {
		t.Fatalf("Received() = %v, want %v", got, syscall.SIGTERM)
	}
	if got := signals.ExitCode(nil); got != ExitTerminated {
		t.Fatalf("ExitCode(nil) = %d, want %d", got, ExitTerminated)
	}
}

func TestNotifySignalsReportsSigint(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	signals := NotifySignals(ctx, os.Interrupt)
	defer signals.Stop()

	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("Kill() = %v, want nil", err)
	}

	select {
	case <-signals.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("context was not canceled within the timeout after SIGINT")
	}
	if got := signals.ExitCode(nil); got != ExitInterrupted {
		t.Fatalf("ExitCode(nil) = %d, want %d", got, ExitInterrupted)
	}
}

func TestNotifySignalsPropagateParentCancel(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	signals := NotifySignals(parent, syscall.SIGTERM)
	defer signals.Stop()

	cancelParent()

	select {
	case <-signals.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("context was not canceled after the parent was canceled")
	}
	if got := signals.Received(); got != nil {
		t.Fatalf("Received() = %v, want nil for a parent cancelation", got)
	}
}

func TestNotifySignalsStopIdempotent(t *testing.T) {
	before := runtime.NumGoroutine()

	signals := NotifySignals(context.Background(), syscall.SIGTERM)
	if err := signals.Context().Err(); err != nil {
		t.Fatalf("Context().Err() = %v, want nil", err)
	}

	stopped := make(chan struct{})
	go func() {
		signals.Stop()
		signals.Stop()
		signals.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() did not return")
	}

	if signals.Context().Err() == nil {
		t.Fatal("context was not canceled by Stop()")
	}
	if got := signals.Received(); got != nil {
		t.Fatalf("Received() = %v, want nil", got)
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(5 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines before = %d, after = %d, want no leak", before, after)
	}
}

func TestSignalContextExitCodeFallsBack(t *testing.T) {
	signals := NotifySignals(context.Background(), syscall.SIGTERM)
	defer signals.Stop()

	if got := signals.ExitCode(nil); got != ExitSuccess {
		t.Fatalf("ExitCode(nil) = %d, want %d", got, ExitSuccess)
	}
	if got := signals.ExitCode(apperr.NotFound("x", "y", nil)); got != ExitIO {
		t.Fatalf("ExitCode(not found) = %d, want %d", got, ExitIO)
	}
}

func TestExitCodeMapping(t *testing.T) {
	tests := []struct {
		err  error
		want int
	}{
		{err: nil, want: ExitSuccess},
		{err: apperr.InvalidArgument("op", "msg", nil), want: ExitUsage},
		{err: apperr.Unsupported("op", "msg", nil), want: ExitUsage},
		{err: apperr.InvalidConfig("op", "msg", nil), want: ExitConfig},
		{err: apperr.Validation("op", "msg", nil), want: ExitConfig},
		{err: apperr.LimitExceeded("op", "msg", nil), want: ExitLimit},
		{err: apperr.NotFound("op", "msg", nil), want: ExitIO},
		{err: apperr.IO("op", "msg", nil), want: ExitIO},
		{err: apperr.Permission("op", "msg", nil), want: ExitIO},
		{err: apperr.Timeout("op", "msg", nil), want: ExitTimeout},
		{err: apperr.Canceled("op", "msg", nil), want: ExitInterrupted},
		{err: apperr.Conflict("op", "msg", nil), want: ExitFailure},
		{err: apperr.Internal("op", "msg", nil), want: ExitFailure},
		{err: apperr.New(apperr.Kind("unknown_kind"), "op", "msg"), want: ExitFailure},
	}

	for _, tc := range tests {
		if got := ExitCode(tc.err); got != tc.want {
			t.Fatalf("ExitCode(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

func TestExitCodeClassifiesWrappedJoined(t *testing.T) {
	wrapped := apperr.LimitExceeded("config.Parse", "too large", nil)
	if got := ExitCode(wrapped); got != ExitLimit {
		t.Fatalf("ExitCode(wrapped) = %d, want %d", got, ExitLimit)
	}
}

func TestSignalExitCode(t *testing.T) {
	if got := SignalExitCode(syscall.SIGINT); got != ExitInterrupted {
		t.Fatalf("SignalExitCode(SIGINT) = %d, want %d", got, ExitInterrupted)
	}
	if got := SignalExitCode(syscall.SIGTERM); got != ExitTerminated {
		t.Fatalf("SignalExitCode(SIGTERM) = %d, want %d", got, ExitTerminated)
	}
	if got := SignalExitCode(syscall.SIGKILL); got != ExitFailure {
		t.Fatalf("SignalExitCode(SIGKILL) = %d, want %d", got, ExitFailure)
	}
}

func TestCleanupStackRunsOnCanceledContext(t *testing.T) {
	stack := cleanup.New()
	cleaned := make(chan struct{})

	if err := stack.Defer("namespace", func(ctx context.Context) error {
		close(cleaned)
		return nil
	}); err != nil {
		t.Fatalf("Defer() = %v, want nil", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := stack.Guard(ctx, func() error {
		return apperr.FromContext(ctx, "setup")
	})
	if !apperr.IsKind(err, apperr.KindCanceled) {
		t.Fatalf("Guard() = %v, want canceled", err)
	}

	select {
	case <-cleaned:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup did not run after cancellation")
	}
}
