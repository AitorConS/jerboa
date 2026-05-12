package slogformat

import (
	"bytes"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestJSONHandlerBasicOutput(t *testing.T) {
	var buf bytes.Buffer
	h := NewJSONHandler(&buf)
	logger := slog.New(h)
	logger.Info("test message", "key", "value")

	line := buf.String()
	require.Contains(t, line, `"msg":"test message"`)
	require.Contains(t, line, `"level":"INFO"`)
	require.Contains(t, line, `"key":"value"`)
	require.Contains(t, line, `"ts":"`)
}

func TestJSONHandlerWithError(t *testing.T) {
	var buf bytes.Buffer
	h := NewJSONHandler(&buf)
	logger := slog.New(h)
	logger.Error("something failed", "err", "connection refused")

	line := buf.String()
	require.Contains(t, line, `"level":"ERROR"`)
	require.Contains(t, line, `"msg":"something failed"`)
	require.Contains(t, line, `"err":"connection refused"`)
}

func TestJSONHandlerWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	h := NewJSONHandler(&buf)
	child := h.WithAttrs([]slog.Attr{slog.String("component", "vm")})
	logger := slog.New(child)
	logger.Info("starting")

	line := buf.String()
	require.Contains(t, line, `"component":"vm"`)
	require.Contains(t, line, `"msg":"starting"`)
}

func TestJSONHandlerWithGroup(t *testing.T) {
	var buf bytes.Buffer
	h := NewJSONHandler(&buf)
	child := h.WithGroup("request")
	logger := slog.New(child)
	logger.Info("handled", "method", "GET")

	line := buf.String()
	require.Contains(t, line, `"msg":"handled"`)
}

func TestJSONHandlerMultipleEntries(t *testing.T) {
	var buf bytes.Buffer
	h := NewJSONHandler(&buf)
	logger := slog.New(h)
	logger.Info("first")
	logger.Info("second")

	require.Equal(t, 2, bytes.Count(buf.Bytes(), []byte("\n")))
}

func TestJSONHandlerKinds(t *testing.T) {
	var buf bytes.Buffer
	h := NewJSONHandler(&buf)
	logger := slog.New(h)
	logger.Info("types",
		"str", "hello",
		"int", int64(42),
		"float", 3.14,
		"bool", true,
		"dur", time.Second,
	)

	line := buf.String()
	require.Contains(t, line, `"str":"hello"`)
	require.Contains(t, line, `"int":42`)
	require.Contains(t, line, `"float":3.14`)
	require.Contains(t, line, `"bool":true`)
	require.Contains(t, line, `"dur":"1s"`)
}
