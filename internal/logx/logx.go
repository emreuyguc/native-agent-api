// Package logx builds the gateway logger and defines the level policy used in the code.
//
// Level policy:
//
//	DEBUG     details useful while diagnosing (CLI arguments, timings of internal steps)
//	INFO      normal lifecycle events (startup, run started/finished, workspace changes)
//	WARN      the request or environment is wrong but the gateway is fine
//	          (auth failure, rejected IP, busy session, insecure setting)
//	ERROR     an operation failed because of the gateway or the agent CLI
//	CRITICAL  the gateway cannot keep serving (listener failure, broken startup)
//
// Prompts, answers and credentials are never logged outside the opt-in body log.
package logx

import (
	"context"
	"io"
	"log/slog"

	"github.com/lmittmann/tint"

	"github.com/emreuyguc/native-agent-api/internal/config"
)

// LevelCritical is above slog.LevelError; it is rendered as "CRITICAL".
const LevelCritical = slog.Level(12)

// New returns a JSON or text logger configured from cfg.
func New(w io.Writer, cfg config.Log) *slog.Logger {
	options := &slog.HandlerOptions{
		Level: cfg.Level,
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			if attr.Key == slog.LevelKey {
				if level, ok := attr.Value.Any().(slog.Level); ok && level >= LevelCritical {
					attr.Value = slog.StringValue("CRITICAL")
				}
			}
			return attr
		},
	}
	var handler slog.Handler
	switch cfg.Format {
	case FormatConsole:
		handler = tint.NewTextHandler(w, &tint.Options{Level: cfg.Level, TimeFormat: consoleTime, ReplaceAttr: consoleAttr})
	case FormatText:
		handler = slog.NewTextHandler(w, options)
	default:
		handler = slog.NewJSONHandler(w, options)
	}

	return slog.New(contextHandler{handler})
}

type requestIDKey struct{}

// WithRequestID returns ctx carrying the HTTP request ID. Every record logged with that
// context gets a request_id attribute.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID returns the request ID carried by ctx, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// contextHandler adds the request ID of the record's context.
type contextHandler struct{ slog.Handler }

func (h contextHandler) Handle(ctx context.Context, record slog.Record) error {
	if id := RequestID(ctx); id != "" {
		record.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, record)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}

// Critical logs at LevelCritical.
func Critical(ctx context.Context, logger *slog.Logger, msg string, args ...any) {
	logger.Log(ctx, LevelCritical, msg, args...)
}

// Discard returns a logger that drops everything (tests, optional dependencies).
func Discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// Log formats (NATIVE_AGENT_LOG_FORMAT).
const (
	FormatJSON = "json"
	FormatText = "text"
	// FormatConsole is colored, human-oriented output for an interactive terminal.
	FormatConsole = "console"
)

const consoleTime = "15:04:05.000"

// ANSI 256-color codes used by the console format.
const (
	colorRed     = 9
	colorGreen   = 10
	colorYellow  = 11
	colorMagenta = 13
)

// consoleAttr colors the critical level, error values and HTTP status codes.
func consoleAttr(groups []string, attr slog.Attr) slog.Attr {
	if len(groups) > 0 {
		return attr
	}
	switch value := attr.Value.Any().(type) {
	case slog.Level:
		if attr.Key == slog.LevelKey && value >= LevelCritical {
			return tint.Attr(colorMagenta, slog.String(slog.LevelKey, "CRT"))
		}
	case error:
		return tint.Attr(colorRed, attr)
	}
	if attr.Key == "status" && attr.Value.Kind() == slog.KindInt64 {
		switch status := attr.Value.Int64(); {
		case status >= 500:
			return tint.Attr(colorRed, attr)
		case status >= 400:
			return tint.Attr(colorYellow, attr)
		default:
			return tint.Attr(colorGreen, attr)
		}
	}

	return attr
}
