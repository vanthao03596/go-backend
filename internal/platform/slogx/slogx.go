// Package slogx configures the process-wide structured logger.
package slogx

import (
	"io"
	"log/slog"
)

// New returns a logger writing to w. In dev mode it uses the human-readable
// text handler; otherwise it emits one JSON object per line.
func New(w io.Writer, level slog.Level, dev bool) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level, AddSource: level <= slog.LevelDebug}
	var h slog.Handler
	if dev {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(h)
}
