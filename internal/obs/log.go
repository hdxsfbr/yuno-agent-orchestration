// Package obs holds logging, redaction and request correlation helpers.
package obs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// NewLogger returns a JSON logger on stdout whose every attribute and message passes
// through Redact (DESIGN 5.12 rule 4).
func NewLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	inner := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(&redactingHandler{inner: inner})
}

// RedactingHandler wraps inner so every message and attribute passes through Redact,
// as NewLogger's handler does (tests capture redacted logs with it).
func RedactingHandler(inner slog.Handler) slog.Handler {
	return &redactingHandler{inner: inner}
}

type redactingHandler struct {
	inner slog.Handler
}

func (h *redactingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	nr := slog.NewRecord(r.Time, r.Level, Redact(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, nr)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, redactAttr(a))
	}
	return &redactingHandler{inner: h.inner.WithAttrs(out)}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{inner: h.inner.WithGroup(name)}
}

func redactAttr(a slog.Attr) slog.Attr {
	if IsSecretKey(a.Key) {
		return slog.String(a.Key, Redacted)
	}
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, Redact(v.String()))
	case slog.KindGroup:
		in := v.Group()
		out := make([]slog.Attr, 0, len(in))
		for _, g := range in {
			out = append(out, redactAttr(g))
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	case slog.KindAny:
		switch x := v.Any().(type) {
		case error:
			return slog.String(a.Key, Redact(x.Error()))
		case fmt.Stringer:
			return slog.String(a.Key, Redact(x.String()))
		case []byte:
			return slog.String(a.Key, Redact(string(x)))
		default:
			return slog.String(a.Key, Redact(fmt.Sprint(x)))
		}
	}
	return a
}

type ctxKey int

const requestIDKey ctxKey = 1

// WithRequestID stores a request id on the context; RequestID reads it back.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

func RequestID(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}
