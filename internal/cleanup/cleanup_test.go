package cleanup

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codingseeker/netforge/internal/apperr"
)

func TestDeferRejectsInvalidInput(t *testing.T) {
	stack := New()

	if err := stack.Defer("", func(context.Context) error { return nil }); err == nil {
		t.Fatal("Defer(\"\") = nil, want error")
	}
	if err := stack.Defer("action", nil); err == nil {
		t.Fatal("Defer(nil) = nil, want error")
	}
	if got := stack.Len(); got != 0 {
		t.Fatalf("Len() = %d, want 0", got)
	}
}

func TestNewWithTimeoutValidation(t *testing.T) {
	if _, err := NewWithTimeout(0); err == nil {
		t.Fatal("NewWithTimeout(0) = nil error, want error")
	}
	if _, err := NewWithTimeout(-time.Second); err == nil {
		t.Fatal("NewWithTimeout(-1s) = nil error, want error")
	}
	if _, err := NewWithTimeout(MaxTimeout + time.Second); err == nil {
		t.Fatalf("NewWithTimeout(%s) = nil error, want error", MaxTimeout+time.Second)
	}
	stack, err := NewWithTimeout(time.Second)
	if err != nil {
		t.Fatalf("NewWithTimeout(1s) = %v, want nil", err)
	}
	if got := stack.Timeout(); got != time.Second {
		t.Fatalf("Timeout() = %s, want 1s", got)
	}
	if got := New().Timeout(); got != DefaultTimeout {
		t.Fatalf("New().Timeout() = %s, want %s", got, DefaultTimeout)
	}
}

func TestRunExecutesInReverseOrder(t *testing.T) {
	var mu sync.Mutex
	var order []string

	stack := New()
	for _, name := range []string{"first", "second", "third"} {
		if err := stack.Defer(name, func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, name)
			return nil
		}); err != nil {
			t.Fatalf("Defer(%q) = %v, want nil", name, err)
		}
	}

	if err := stack.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}

	want := []string{"third", "second", "first"}
	if len(order) != len(want) {
		t.Fatalf("cleanup order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("cleanup order = %v, want %v", order, want)
		}
	}
}

func TestRunIsIdempotent(t *testing.T) {
	calls := 0
	stack := New()
	if err := stack.Defer("action", func(context.Context) error {
		calls++
		return nil
	}); err != nil {
		t.Fatalf("Defer() = %v, want nil", err)
	}

	first := stack.Run(context.Background())
	second := stack.Run(context.Background())
	third := stack.Run(context.Background())

	if calls != 1 {
		t.Fatalf("cleanup action calls = %d, want 1", calls)
	}
	if first != nil || second != nil || third != nil {
		t.Fatalf("Run() errors = %v, %v, %v, want nil", first, second, third)
	}
	if got := stack.Len(); got != 0 {
		t.Fatalf("Len() after Run = %d, want 0", got)
	}
}

func TestDeferAfterRunIsRejected(t *testing.T) {
	stack := New()
	if err := stack.Defer("action", func(context.Context) error { return nil }); err != nil {
		t.Fatalf("Defer() = %v, want nil", err)
	}
	if err := stack.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}

	err := stack.Defer("late", func(context.Context) error { return nil })
	if !apperr.IsKind(err, apperr.KindConflict) {
		t.Fatalf("Defer after Run = %v, want conflict", err)
	}
}

func TestRunContinuesFailureJoinsErrors(t *testing.T) {
	executed := make([]string, 0, 3)
	stack := New()
	actions := []struct {
		name string
		fail bool
	}{
		{name: "first"},
		{name: "second", fail: true},
		{name: "third"},
	}
	for _, action := range actions {
		action := action
		if err := stack.Defer(action.name, func(context.Context) error {
			executed = append(executed, action.name)
			if action.fail {
				return errors.New("teardown failed")
			}
			return nil
		}); err != nil {
			t.Fatalf("Defer(%q) = %v, want nil", action.name, err)
		}
	}

	err := stack.Run(context.Background())
	if err == nil {
		t.Fatal("Run() = nil, want error")
	}
	if !strings.Contains(err.Error(), "teardown failed") {
		t.Fatalf("Run() = %v, want cause preserved", err)
	}
	if !strings.Contains(err.Error(), `cleanup action "second" failed`) {
		t.Fatalf("Run() = %v, want action name", err)
	}
	want := []string{"third", "second", "first"}
	for i := range want {
		if executed[i] != want[i] {
			t.Fatalf("executed = %v, want %v", executed, want)
		}
	}

	if second := stack.Run(context.Background()); second == nil {
		t.Fatal("second Run() = nil, want the first result")
	}
}

func TestRunSurvivesCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	stack := New()
	if err := stack.Defer("action", func(runCtx context.Context) error {
		if runCtx.Err() != nil {
			return errors.New("cleanup context was canceled")
		}
		return nil
	}); err != nil {
		t.Fatalf("Defer() = %v, want nil", err)
	}

	if err := stack.Run(ctx); err != nil {
		t.Fatalf("Run() = %v, want nil; cleanup must run on a live context", err)
	}
}

func TestRunBoundsCleanupWithTimeout(t *testing.T) {
	stack, err := NewWithTimeout(20 * time.Millisecond)
	if err != nil {
		t.Fatalf("NewWithTimeout() = %v, want nil", err)
	}

	started := time.Now()
	if err := stack.Defer("slow", func(runCtx context.Context) error {
		<-runCtx.Done()
		return runCtx.Err()
	}); err != nil {
		t.Fatalf("Defer() = %v, want nil", err)
	}

	runErr := stack.Run(context.Background())
	elapsed := time.Since(started)

	if runErr == nil {
		t.Fatal("Run() = nil, want deadline error")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Run() took %s, want the cleanup bound to be enforced", elapsed)
	}
}

func TestRunHandlesNilContext(t *testing.T) {
	stack := New()
	if err := stack.Defer("action", func(ctx context.Context) error {
		if ctx == nil {
			return errors.New("nil context")
		}
		return nil
	}); err != nil {
		t.Fatalf("Defer() = %v, want nil", err)
	}
	if err := stack.Run(nil); err != nil {
		t.Fatalf("Run(nil) = %v, want nil", err)
	}
}

func TestRunWithNoActions(t *testing.T) {
	if err := New().Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
}

func TestCleanupPanicDoesNotStopRemainingActions(t *testing.T) {
	executed := false
	stack := New()
	if err := stack.Defer("panics", func(context.Context) error {
		panic("cleanup exploded")
	}); err != nil {
		t.Fatalf("Defer() = %v, want nil", err)
	}
	if err := stack.Defer("survivor", func(context.Context) error {
		executed = true
		return nil
	}); err != nil {
		t.Fatalf("Defer() = %v, want nil", err)
	}

	err := stack.Run(context.Background())
	if err == nil {
		t.Fatal("Run() = nil, want error")
	}
	if !strings.Contains(err.Error(), "cleanup exploded") {
		t.Fatalf("Run() = %v, want panic detail", err)
	}
	if !executed {
		t.Fatal("remaining cleanup action did not run after a panicking action")
	}
}

func TestGuardRunsCleanupOnSuccess(t *testing.T) {
	cleaned := false
	stack := New()
	if err := stack.Defer("action", func(context.Context) error {
		cleaned = true
		return nil
	}); err != nil {
		t.Fatalf("Defer() = %v, want nil", err)
	}

	if err := stack.Guard(context.Background(), func() error { return nil }); err != nil {
		t.Fatalf("Guard() = %v, want nil", err)
	}
	if !cleaned {
		t.Fatal("cleanup did not run after success")
	}
}

func TestGuardRunsCleanupOnFailure(t *testing.T) {
	cleaned := false
	stack := New()
	if err := stack.Defer("action", func(context.Context) error {
		cleaned = true
		return nil
	}); err != nil {
		t.Fatalf("Defer() = %v, want nil", err)
	}

	failure := errors.New("operation failed")
	err := stack.Guard(context.Background(), func() error { return failure })

	if !errors.Is(err, failure) {
		t.Fatalf("Guard() = %v, want the operation failure", err)
	}
	if !cleaned {
		t.Fatal("cleanup did not run after failure")
	}
}

func TestGuardRecoversPanic(t *testing.T) {
	cleaned := false
	stack := New()
	if err := stack.Defer("action", func(context.Context) error {
		cleaned = true
		return nil
	}); err != nil {
		t.Fatalf("Defer() = %v, want nil", err)
	}

	err := stack.Guard(context.Background(), func() error {
		panic("operation exploded")
	})

	if err == nil {
		t.Fatal("Guard() = nil, want error")
	}
	if !apperr.IsKind(err, apperr.KindInternal) {
		t.Fatalf("Guard() = %v, want internal error", err)
	}
	if !strings.Contains(err.Error(), "operation exploded") {
		t.Fatalf("Guard() = %v, want panic detail", err)
	}
	if !cleaned {
		t.Fatal("cleanup did not run after panic")
	}
}

func TestGuardJoinsFailureAndCleanupFailure(t *testing.T) {
	stack := New()
	if err := stack.Defer("action", func(context.Context) error {
		return errors.New("teardown failed")
	}); err != nil {
		t.Fatalf("Defer() = %v, want nil", err)
	}

	err := stack.Guard(context.Background(), func() error { return errors.New("operation failed") })

	if err == nil {
		t.Fatal("Guard() = nil, want error")
	}
	if !strings.Contains(err.Error(), "operation failed") {
		t.Fatalf("Guard() = %v, want operation failure", err)
	}
	if !strings.Contains(err.Error(), "teardown failed") {
		t.Fatalf("Guard() = %v, want cleanup failure", err)
	}
}

func TestGuardRunsCleanupExactlyOnce(t *testing.T) {
	calls := 0
	stack := New()
	if err := stack.Defer("action", func(context.Context) error {
		calls++
		return nil
	}); err != nil {
		t.Fatalf("Defer() = %v, want nil", err)
	}

	for i := 0; i < 3; i++ {
		_ = stack.Guard(context.Background(), func() error { return nil })
	}
	if calls != 1 {
		t.Fatalf("cleanup calls = %d, want 1", calls)
	}
}

func TestConcurrentRunIsSafe(t *testing.T) {
	stack := New()
	var mu sync.Mutex
	calls := 0
	if err := stack.Defer("action", func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return nil
	}); err != nil {
		t.Fatalf("Defer() = %v, want nil", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := stack.Run(context.Background()); err != nil {
				t.Errorf("Run() = %v, want nil", err)
			}
		}()
	}
	wg.Wait()

	if calls != 1 {
		t.Fatalf("cleanup calls = %d, want 1", calls)
	}
}

func TestConcurrentDeferAndRunIsSafe(t *testing.T) {
	stack := New()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := stack.Run(context.Background()); err != nil {
			t.Errorf("Run() = %v, want nil", err)
		}
	}()

	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := stack.Defer("action", func(context.Context) error { return nil })
			if err != nil && !apperr.IsKind(err, apperr.KindConflict) {
				t.Errorf("Defer() = %v, want nil or conflict", err)
			}
		}()
	}
	wg.Wait()
}

func TestNoGoroutinesLeaked(t *testing.T) {
	before := runtime.NumGoroutine()

	for i := 0; i < 50; i++ {
		stack := New()
		if err := stack.Defer("action", func(context.Context) error { return nil }); err != nil {
			t.Fatalf("Defer() = %v, want nil", err)
		}
		if err := stack.Run(context.Background()); err != nil {
			t.Fatalf("Run() = %v, want nil", err)
		}
	}

	runtime.GC()
	time.Sleep(10 * time.Millisecond)

	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("goroutines before = %d, after = %d, want no growth", before, after)
	}
}
