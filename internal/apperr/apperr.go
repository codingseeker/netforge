package apperr

import (
	"context"
	"errors"
	"strings"
)

type Kind string

const (
	KindInvalidArgument Kind = "invalid_argument"
	KindInvalidConfig   Kind = "invalid_config"
	KindValidation      Kind = "validation"
	KindNotFound        Kind = "not_found"
	KindConflict        Kind = "conflict"
	KindLimitExceeded   Kind = "limit_exceeded"
	KindUnsupported     Kind = "unsupported"
	KindPermission      Kind = "permission_denied"
	KindTimeout         Kind = "timeout"
	KindCanceled        Kind = "canceled"
	KindIO              Kind = "io"
	KindInternal        Kind = "internal"
)

var allKinds = []Kind{
	KindInvalidArgument,
	KindInvalidConfig,
	KindValidation,
	KindNotFound,
	KindConflict,
	KindLimitExceeded,
	KindUnsupported,
	KindPermission,
	KindTimeout,
	KindCanceled,
	KindIO,
	KindInternal,
}

func (k Kind) Valid() bool {
	for _, candidate := range allKinds {
		if k == candidate {
			return true
		}
	}
	return false
}

func (k Kind) String() string {
	return string(k)
}

type Error struct {
	Kind Kind
	Op   string
	Msg  string
	Err  error
}

func New(kind Kind, op, msg string) *Error {
	return &Error{Kind: kind, Op: op, Msg: msg}
}

func Wrap(err error, kind Kind, op, msg string) *Error {
	return &Error{Kind: kind, Op: op, Msg: msg, Err: err}
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	parts := make([]string, 0, 3)
	parts = append(parts, string(e.Kind))
	if e.Op != "" {
		parts = append(parts, e.Op)
	}
	if e.Msg != "" {
		parts = append(parts, e.Msg)
	}
	if e.Err != nil {
		parts = append(parts, e.Err.Error())
	}
	return strings.Join(parts, ": ")
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *Error) Is(target error) bool {
	var other *Error
	if !errors.As(target, &other) {
		return false
	}
	return e.Kind == other.Kind
}

var (
	ErrInvalidArgument = &Error{Kind: KindInvalidArgument}
	ErrInvalidConfig   = &Error{Kind: KindInvalidConfig}
	ErrValidation      = &Error{Kind: KindValidation}
	ErrNotFound        = &Error{Kind: KindNotFound}
	ErrConflict        = &Error{Kind: KindConflict}
	ErrLimitExceeded   = &Error{Kind: KindLimitExceeded}
	ErrUnsupported     = &Error{Kind: KindUnsupported}
	ErrPermission      = &Error{Kind: KindPermission}
	ErrTimeout         = &Error{Kind: KindTimeout}
	ErrCanceled        = &Error{Kind: KindCanceled}
	ErrIO              = &Error{Kind: KindIO}
	ErrInternal        = &Error{Kind: KindInternal}
)

var sentinels = map[Kind]*Error{
	KindInvalidArgument: ErrInvalidArgument,
	KindInvalidConfig:   ErrInvalidConfig,
	KindValidation:      ErrValidation,
	KindNotFound:        ErrNotFound,
	KindConflict:        ErrConflict,
	KindLimitExceeded:   ErrLimitExceeded,
	KindUnsupported:     ErrUnsupported,
	KindPermission:      ErrPermission,
	KindTimeout:         ErrTimeout,
	KindCanceled:        ErrCanceled,
	KindIO:              ErrIO,
	KindInternal:        ErrInternal,
}

func InvalidArgument(op, msg string, cause error) *Error {
	return Wrap(cause, KindInvalidArgument, op, msg)
}

func InvalidConfig(op, msg string, cause error) *Error {
	return Wrap(cause, KindInvalidConfig, op, msg)
}

func Validation(op, msg string, cause error) *Error {
	return Wrap(cause, KindValidation, op, msg)
}

func NotFound(op, msg string, cause error) *Error {
	return Wrap(cause, KindNotFound, op, msg)
}

func Conflict(op, msg string, cause error) *Error {
	return Wrap(cause, KindConflict, op, msg)
}

func LimitExceeded(op, msg string, cause error) *Error {
	return Wrap(cause, KindLimitExceeded, op, msg)
}

func Unsupported(op, msg string, cause error) *Error {
	return Wrap(cause, KindUnsupported, op, msg)
}

func Permission(op, msg string, cause error) *Error {
	return Wrap(cause, KindPermission, op, msg)
}

func Timeout(op, msg string, cause error) *Error {
	return Wrap(cause, KindTimeout, op, msg)
}

func Canceled(op, msg string, cause error) *Error {
	return Wrap(cause, KindCanceled, op, msg)
}

func IO(op, msg string, cause error) *Error {
	return Wrap(cause, KindIO, op, msg)
}

func Internal(op, msg string, cause error) *Error {
	return Wrap(cause, KindInternal, op, msg)
}

func KindOf(err error) (Kind, bool) {
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr.Kind, true
	}
	return "", false
}

func IsKind(err error, kind Kind) bool {
	return Is(err, kind)
}

func Is(err error, kind Kind) bool {
	sentinel, ok := sentinels[kind]
	if err == nil || !ok {
		return false
	}
	return errors.Is(err, sentinel)
}

func FromContext(ctx context.Context, op string) error {
	err := ctx.Err()
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.DeadlineExceeded):
		return Timeout(op, "deadline exceeded", err)
	default:
		return Canceled(op, "canceled", err)
	}
}
