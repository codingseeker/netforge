package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/netforge/netforge/internal/apperr"
)

type Format string

const (
	FormatText Format = "text"
	FormatJSON Format = "json"
)

const (
	FieldExperimentID = "experiment_id"
	FieldComponent    = "component"
	FieldEvent        = "event"
)

const DefaultComponent = "netforge"

type Options struct {
	Level     slog.Level
	Format    Format
	Component string
}

func ParseLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, apperr.InvalidArgument("logging.ParseLevel", fmt.Sprintf("unsupported log level %q; use debug, info, warn or error", value), nil)
	}
}

func ParseFormat(value string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "text", "":
		return FormatText, nil
	case "json":
		return FormatJSON, nil
	default:
		return "", apperr.InvalidArgument("logging.ParseFormat", fmt.Sprintf("unsupported log format %q; use text or json", value), nil)
	}
}

func New(w io.Writer, opts Options) (*slog.Logger, error) {
	if w == nil {
		return nil, apperr.InvalidArgument("logging.New", "writer must not be nil", nil)
	}
	if opts.Format == "" {
		opts.Format = FormatText
	}
	if opts.Format != FormatText && opts.Format != FormatJSON {
		return nil, apperr.InvalidArgument("logging.New", fmt.Sprintf("unsupported log format %q", opts.Format), nil)
	}
	handlerOptions := &slog.HandlerOptions{
		Level:       opts.Level,
		ReplaceAttr: replaceAttr,
	}

	var handler slog.Handler
	switch opts.Format {
	case FormatJSON:
		handler = slog.NewJSONHandler(w, handlerOptions)
	default:
		handler = slog.NewTextHandler(w, handlerOptions)
	}

	logger := slog.New(handler)
	if opts.Component != "" {
		logger = logger.With(slog.String(FieldComponent, opts.Component))
	}
	return logger, nil
}

func WithComponent(logger *slog.Logger, component string) *slog.Logger {
	if logger == nil {
		return nil
	}
	return logger.With(slog.String(FieldComponent, component))
}

func WithExperiment(logger *slog.Logger, experimentID string) *slog.Logger {
	if logger == nil {
		return nil
	}
	return logger.With(slog.String(FieldExperimentID, experimentID))
}

func replaceAttr(groups []string, attr slog.Attr) slog.Attr {
	if len(groups) != 0 {
		return attr
	}
	switch attr.Key {
	case slog.TimeKey:
		return slog.String("timestamp", attr.Value.Time().UTC().Format(time.RFC3339Nano))
	case slog.MessageKey:
		return slog.String(FieldEvent, attr.Value.String())
	case slog.LevelKey:
		return slog.String("level", strings.ToLower(attr.Value.String()))
	default:
		return attr
	}
}
