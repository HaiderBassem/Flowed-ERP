// Package logger builds the structured logger and carries it through request
// context. Log records are the second half of the audit story: the audit_log
// table records what changed in the database, and these records explain what
// the process was doing around it.
package logger

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"flowed/internal/platform/config"
)

type contextKey struct{}

var loggerKey = contextKey{}

// New builds a logger from configuration.
func New(cfg config.Log, appName, version, environment string) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level:     parseLevel(cfg.Level),
		AddSource: cfg.IncludeSource,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			// Never let a value that looks like a credential reach the log,
			// whatever a caller passes.
			if isSensitiveKey(a.Key) {
				return slog.String(a.Key, "[redacted]")
			}
			return a
		},
	}

	var handler slog.Handler
	if cfg.Format == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}

	return slog.New(handler).With(
		slog.String("service", appName),
		slog.String("version", version),
		slog.String("env", environment),
	)
}

// NewNop returns a logger that discards everything, for tests.
func NewNop() *slog.Logger {
	return slog.New(slog.NewTextHandler(discard{}, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// Into stores a logger in the context so downstream layers log with the
// request's correlation fields already attached.
func Into(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, l)
}

// From retrieves the context logger, falling back to the default logger when
// none was installed.
func From(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

var sensitiveKeys = []string{
	"password", "passwd", "secret", "token", "authorization",
	"jwt", "api_key", "apikey", "credential", "dsn",
}

func isSensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	for _, s := range sensitiveKeys {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}
