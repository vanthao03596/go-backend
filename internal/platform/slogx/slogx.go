// Package slogx configures the process-wide structured logger.
package slogx

import (
	"context"
	"io"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// Attribute keys added to records logged inside a span.
const (
	TraceIDKey = "trace_id"
	SpanIDKey  = "span_id"
)

// New returns a logger writing to w. In dev mode it uses the human-readable
// text handler; otherwise it emits one JSON object per line. Records logged
// with a context that carries a valid span get trace_id and span_id, so a
// log line can be looked up in the tracing UI.
func New(w io.Writer, level slog.Level, dev bool) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level, AddSource: level <= slog.LevelDebug}
	var h slog.Handler
	if dev {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(WithTraceContext(h))
}

// WithTraceContext wraps h so every record logged through a *Context method
// (InfoContext, LogAttrs and so on) whose context carries a valid span gets
// trace_id and span_id attributes. Records without a span pass through
// unchanged. Like any attribute added at Handle time, the ids land inside the
// innermost group opened with WithGroup.
func WithTraceContext(h slog.Handler) slog.Handler {
	return traceHandler{inner: h}
}

type traceHandler struct {
	inner slog.Handler
}

func (h traceHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		// Clone: the record may share attribute storage with the caller's
		// copy, and slog requires a clone before adding to it.
		r = r.Clone()
		r.AddAttrs(
			slog.String(TraceIDKey, sc.TraceID().String()),
			slog.String(SpanIDKey, sc.SpanID().String()),
		)
	}
	return h.inner.Handle(ctx, r) //nolint:wrapcheck // decorator; the inner handler's error is the error
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{inner: h.inner.WithGroup(name)}
}
