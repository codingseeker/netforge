package cli

import (
	"os"
	"syscall"

	"github.com/codingseeker/netforge/internal/apperr"
)

const (
	ExitSuccess     = 0
	ExitFailure     = 1
	ExitUsage       = 2
	ExitConfig      = 3
	ExitLimit       = 4
	ExitIO          = 5
	ExitTimeout     = 124
	ExitInterrupted = 130
	ExitTerminated  = 143
)

func ExitCode(err error) int {
	if err == nil {
		return ExitSuccess
	}
	switch {
	case apperr.IsKind(err, apperr.KindInvalidArgument), apperr.IsKind(err, apperr.KindUnsupported):
		return ExitUsage
	case apperr.IsKind(err, apperr.KindInvalidConfig), apperr.IsKind(err, apperr.KindValidation):
		return ExitConfig
	case apperr.IsKind(err, apperr.KindLimitExceeded):
		return ExitLimit
	case apperr.IsKind(err, apperr.KindNotFound), apperr.IsKind(err, apperr.KindIO), apperr.IsKind(err, apperr.KindPermission):
		return ExitIO
	case apperr.IsKind(err, apperr.KindTimeout):
		return ExitTimeout
	case apperr.IsKind(err, apperr.KindCanceled):
		return ExitInterrupted
	case apperr.IsKind(err, apperr.KindConflict), apperr.IsKind(err, apperr.KindInternal):
		return ExitFailure
	default:
		return ExitFailure
	}
}

func SignalExitCode(signal os.Signal) int {
	switch signal {
	case syscall.SIGINT:
		return ExitInterrupted
	case syscall.SIGTERM:
		return ExitTerminated
	default:
		return ExitFailure
	}
}
