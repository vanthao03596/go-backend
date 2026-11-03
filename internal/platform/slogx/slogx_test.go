package slogx

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func spanContext(t *testing.T) (context.Context, trace.SpanContext) {
	t.Helper()
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x0a, 0xf7, 0x65, 0x19, 0x16, 0xcd, 0x43, 0xdd, 0x84, 0x48, 0xeb, 0x21, 0x1c, 0x80, 0x31, 0x9c},
		SpanID:     trace.SpanID{0xb7, 0xad, 0x6b, 0x71, 0x69, 0x20, 0x33, 0x31},
		TraceFlags: trace.FlagsSampled,
	})
	return trace.ContextWithSpanContext(context.Background(), sc), sc
}

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("decode %q: %v", buf.String(), err)
	}
	return m
}

func TestTraceIDsAddedInsideSpan(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, slog.LevelInfo, false).With("component", "test")
	ctx, sc := spanContext(t)

	log.InfoContext(ctx, "hello", "k", "v")

	m := decode(t, &buf)
	if m[TraceIDKey] != sc.TraceID().String() {
		t.Errorf("%s = %v, want %s", TraceIDKey, m[TraceIDKey], sc.TraceID())
	}
	if m[SpanIDKey] != sc.SpanID().String() {
		t.Errorf("%s = %v, want %s", SpanIDKey, m[SpanIDKey], sc.SpanID())
	}
	if m["component"] != "test" || m["k"] != "v" {
		t.Errorf("record lost its own attributes: %v", m)
	}
}

func TestNoTraceIDsWithoutSpan(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, slog.LevelInfo, false)

	log.InfoContext(context.Background(), "hello")
	log.Info("no context at all")

	for line := range bytes.Lines(buf.Bytes()) {
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		if _, ok := m[TraceIDKey]; ok {
			t.Errorf("unexpected %s in %v", TraceIDKey, m)
		}
	}
}

func TestLevelFilteringIsPreserved(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, slog.LevelWarn, false)
	ctx, _ := spanContext(t)

	log.InfoContext(ctx, "dropped")
	if buf.Len() != 0 {
		t.Errorf("info logged at warn level: %s", buf.String())
	}
}

// A record with more attributes than slog stores inline must be cloned before
// the ids are added; slog marks unsafe additions with a !BUG attribute.
func TestManyAttributesAreNotCorrupted(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, slog.LevelInfo, false)
	ctx, _ := spanContext(t)

	log.LogAttrs(ctx, slog.LevelInfo, "many",
		slog.Int("a", 1), slog.Int("b", 2), slog.Int("c", 3), slog.Int("d", 4),
		slog.Int("e", 5), slog.Int("f", 6), slog.Int("g", 7))

	m := decode(t, &buf)
	if _, ok := m["!BUG"]; ok {
		t.Fatalf("record was mutated unsafely: %v", m)
	}
	if m["g"] != float64(7) || m[TraceIDKey] == nil {
		t.Errorf("attributes = %v", m)
	}
}
