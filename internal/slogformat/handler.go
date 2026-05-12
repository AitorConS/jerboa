// Package slogformat provides a structured JSON log handler for the unikernel daemon.
package slogformat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

// JSONHandler outputs structured JSON lines to a writer.
// It ensures every log entry has: ts, level, msg, and optional component/vm_id fields.
type JSONHandler struct {
	w     io.Writer
	mu    sync.Mutex
	attrs []slog.Attr
	group string
}

// NewJSONHandler creates a handler that writes JSON lines to w.
func NewJSONHandler(w io.Writer) *JSONHandler {
	return &JSONHandler{w: w}
}

// Enabled returns true for all levels — level filtering is done by slog itself.
func (h *JSONHandler) Enabled(_ context.Context, level slog.Level) bool {
	return true
}

// Handle formats the log record as a JSON line.
func (h *JSONHandler) Handle(_ context.Context, r slog.Record) error {
	entry := map[string]interface{}{
		"ts":    r.Time.Format(time.RFC3339Nano),
		"level": r.Level.String(),
		"msg":   r.Message,
	}

	for _, a := range h.attrs {
		entry[a.Key] = aValue(a.Value)
	}

	r.Attrs(func(a slog.Attr) bool {
		entry[a.Key] = aValue(a.Value)
		return true
	})

	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("json marshal log: %w", err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if _, err := h.w.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write log: %w", err)
	}
	return nil
}

// WithAttrs returns a new handler with the given attributes appended.
func (h *JSONHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(h.attrs)+len(attrs))
	copy(newAttrs, h.attrs)
	copy(newAttrs[len(h.attrs):], attrs)
	return &JSONHandler{w: h.w, attrs: newAttrs, group: h.group}
}

// WithGroup returns a new handler with the given group prefix.
func (h *JSONHandler) WithGroup(name string) slog.Handler {
	return &JSONHandler{w: h.w, attrs: h.attrs, group: name}
}

func aValue(v slog.Value) interface{} {
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindInt64:
		return v.Int64()
	case slog.KindFloat64:
		return v.Float64()
	case slog.KindBool:
		return v.Bool()
	case slog.KindTime:
		return v.Time().Format(time.RFC3339Nano)
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindGroup:
		group := v.Group()
		m := make(map[string]interface{}, len(group))
		for _, a := range group {
			m[a.Key] = aValue(a.Value)
		}
		return m
	default:
		return v.String()
	}
}
