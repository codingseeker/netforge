package apperr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestErrorMessageIncludesContext(t *testing.T) {
	cause := errors.New("permission denied")
	err := Permission("create namespace", "cannot create namespace client", cause)

	want := "permission_denied: create namespace: cannot create namespace client: permission denied"
	if got := err.Error(); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestErrorUnwrapsCause(t *testing.T) {
	cause := os.ErrNotExist
	err := NotFound("read config", "configuration file not found", cause)

	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("errors.Is(err, os.ErrNotExist) = false, want true")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("errors.Is(err, ErrNotFound) = false, want true")
	}
	if errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("errors.Is(err, ErrInvalidConfig) = true, want false")
	}
}

func TestErrorOmitsEmptyFields(t *testing.T) {
	err := New(KindInternal, "", "unexpected condition")
	if got, want := err.Error(), "internal: unexpected condition"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestKindOf(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantKind Kind
		wantOK   bool
	}{
		{name: "nil", err: nil},
		{name: "plain", err: errors.New("boom")},
		{name: "typed", err: Conflict("create", "already exists", nil), wantKind: KindConflict, wantOK: true},
		{name: "wrapped", err: fmt.Errorf("outer: %w", LimitExceeded("load", "too large", nil)), wantKind: KindLimitExceeded, wantOK: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kind, ok := KindOf(tc.err)
			if ok != tc.wantOK {
				t.Fatalf("KindOf ok = %v, want %v", ok, tc.wantOK)
			}
			if kind != tc.wantKind {
				t.Fatalf("KindOf kind = %q, want %q", kind, tc.wantKind)
			}
		})
	}
}

func TestIsKindThroughJoin(t *testing.T) {
	joined := errors.Join(
		Internal("report", "render failed", nil),
		LimitExceeded("load", "configuration exceeds 1048576 bytes", nil),
	)

	if !IsKind(joined, KindLimitExceeded) {
		t.Fatalf("IsKind(joined, KindLimitExceeded) = false, want true")
	}
	if !IsKind(joined, KindInternal) {
		t.Fatalf("IsKind(joined, KindInternal) = false, want true")
	}
	if IsKind(joined, KindTimeout) {
		t.Fatalf("IsKind(joined, KindTimeout) = true, want false")
	}
}

func TestIsKindNil(t *testing.T) {
	if IsKind(nil, KindInternal) {
		t.Fatalf("IsKind(nil, KindInternal) = true, want false")
	}
}

func TestFromContext(t *testing.T) {
	t.Run("live context", func(t *testing.T) {
		if err := FromContext(context.Background(), "probe"); err != nil {
			t.Fatalf("FromContext() = %v, want nil", err)
		}
	})

	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := FromContext(ctx, "probe")
		if !IsKind(err, KindCanceled) {
			t.Fatalf("FromContext kind = %v, want canceled (%v)", err, err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("errors.Is(err, context.Canceled) = false, want true")
		}
	})

	t.Run("deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancel()
		<-ctx.Done()

		err := FromContext(ctx, "probe")
		if !IsKind(err, KindTimeout) {
			t.Fatalf("FromContext kind = %v, want timeout (%v)", err, err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("errors.Is(err, context.DeadlineExceeded) = false, want true")
		}
	})
}

func TestEveryKindIsRoutable(t *testing.T) {
	for _, kind := range allKinds {
		if !kind.Valid() {
			t.Fatalf("kind %q reported invalid", kind)
		}
		sentinel, ok := sentinels[kind]
		if !ok {
			t.Fatalf("kind %q has no sentinel", kind)
		}
		err := New(kind, "op", "msg")
		if !IsKind(err, kind) {
			t.Fatalf("IsKind(%v, %q) = false, want true", err, kind)
		}
		if !errors.Is(err, sentinel) {
			t.Fatalf("errors.Is(%v, sentinel) = false, want true", err)
		}
	}
}

func TestInvalidKindIsRejected(t *testing.T) {
	if Kind("nonsense").Valid() {
		t.Fatalf("Kind(nonsense).Valid() = true, want false")
	}
	if Is(errors.New("boom"), Kind("nonsense")) {
		t.Fatalf("Is(err, unknown kind) = true, want false")
	}
}

func TestConstructorsProduceMatchingKinds(t *testing.T) {
	cause := errors.New("root cause")

	tests := []struct {
		name string
		err  *Error
		want Kind
	}{
		{name: "invalid_argument", err: InvalidArgument("op", "msg", cause), want: KindInvalidArgument},
		{name: "invalid_config", err: InvalidConfig("op", "msg", cause), want: KindInvalidConfig},
		{name: "validation", err: Validation("op", "msg", cause), want: KindValidation},
		{name: "not_found", err: NotFound("op", "msg", cause), want: KindNotFound},
		{name: "conflict", err: Conflict("op", "msg", cause), want: KindConflict},
		{name: "limit_exceeded", err: LimitExceeded("op", "msg", cause), want: KindLimitExceeded},
		{name: "unsupported", err: Unsupported("op", "msg", cause), want: KindUnsupported},
		{name: "permission_denied", err: Permission("op", "msg", cause), want: KindPermission},
		{name: "timeout", err: Timeout("op", "msg", cause), want: KindTimeout},
		{name: "canceled", err: Canceled("op", "msg", cause), want: KindCanceled},
		{name: "io", err: IO("op", "msg", cause), want: KindIO},
		{name: "internal", err: Internal("op", "msg", cause), want: KindInternal},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err.Kind != tc.want {
				t.Fatalf("Kind = %q, want %q", tc.err.Kind, tc.want)
			}
			if !IsKind(tc.err, tc.want) {
				t.Fatalf("IsKind(err, %q) = false, want true", tc.want)
			}
			if !errors.Is(tc.err, cause) {
				t.Fatalf("errors.Is(err, cause) = false, want true")
			}
			if got, want := tc.err.Error(), string(tc.want)+": op: msg: root cause"; got != want {
				t.Fatalf("Error() = %q, want %q", got, want)
			}
		})
	}

	if len(tests) != len(allKinds) {
		t.Fatalf("constructor table has %d entries, want %d", len(tests), len(allKinds))
	}
}

func TestConstructorsAllowNoCause(t *testing.T) {
	err := Timeout("experiment", "duration limit reached", nil)
	if got, want := err.Error(), "timeout: experiment: duration limit reached"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if errors.Unwrap(err) != nil {
		t.Fatalf("errors.Unwrap(err) != nil, want nil")
	}
}

func TestKindString(t *testing.T) {
	if got := KindLimitExceeded.String(); got != "limit_exceeded" {
		t.Fatalf("String() = %q, want %q", got, "limit_exceeded")
	}
}

func TestIsMatchesOnlyEqualKind(t *testing.T) {
	err := Validation("load", "name is invalid", nil)
	if err.Is(errors.New("boom")) {
		t.Fatalf("Is(non-apperr) = true, want false")
	}
	if !err.Is(ErrValidation) {
		t.Fatalf("Is(ErrValidation) = false, want true")
	}
	if err.Is(ErrInternal) {
		t.Fatalf("Is(ErrInternal) = true, want false")
	}
}

func TestNilErrorString(t *testing.T) {
	var err *Error
	if got := err.Error(); got != "<nil>" {
		t.Fatalf("(*Error)(nil).Error() = %q, want %q", got, "<nil>")
	}
	if err.Unwrap() != nil {
		t.Fatalf("(*Error)(nil).Unwrap() != nil")
	}
}
