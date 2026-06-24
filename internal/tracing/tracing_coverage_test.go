//go:build linux

package tracing

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/AitorConS/unikernel-engine/internal/vm"
)

func TestNewProviderWithAddr(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, err := NewProvider(ctx, "localhost:4317", "test")
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	require.NotNil(t, p)
	require.NoError(t, p.Shutdown(ctx))
}

func TestShutdownWithRealTP(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	p := &Provider{
		tp: tp,
		shutdownFn: func(ctx context.Context) error {
			return tp.Shutdown(ctx)
		},
	}
	require.NoError(t, p.Shutdown(context.Background()))
}

func TestProviderShutdownNoop(t *testing.T) {
	p, err := NewProvider(context.Background(), "", "test")
	require.NoError(t, err)
	require.NoError(t, p.Shutdown(context.Background()))
	require.NoError(t, p.Shutdown(context.Background()))
}

func TestTracerReturnsProvider(t *testing.T) {
	setupTestTP(t)
	tr := Tracer()
	require.NotNil(t, tr)
}

func TestStartVMStopSpan(t *testing.T) {
	_, exp := setupTestTP(t)

	span, _ := StartVMStopSpan(context.Background(), "vm-stop-1")
	span.End()

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	require.Equal(t, "vm.stop", spans[0].Name)

	attrs := make(map[string]interface{})
	for _, a := range spans[0].Attributes {
		attrs[string(a.Key)] = a.Value.AsString()
	}
	require.Equal(t, "vm-stop-1", attrs["vm.id"])
}

func TestStartVMKillSpan(t *testing.T) {
	_, exp := setupTestTP(t)

	span, _ := StartVMKillSpan(context.Background(), "vm-kill-1")
	span.End()

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	require.Equal(t, "vm.kill", spans[0].Name)

	attrs := make(map[string]interface{})
	for _, a := range spans[0].Attributes {
		attrs[string(a.Key)] = a.Value.AsString()
	}
	require.Equal(t, "vm-kill-1", attrs["vm.id"])
}

func TestStartVMRemoveSpan(t *testing.T) {
	_, exp := setupTestTP(t)

	span, _ := StartVMRemoveSpan(context.Background(), "vm-remove-1")
	span.End()

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	require.Equal(t, "vm.remove", spans[0].Name)

	attrs := make(map[string]interface{})
	for _, a := range spans[0].Attributes {
		attrs[string(a.Key)] = a.Value.AsString()
	}
	require.Equal(t, "vm-remove-1", attrs["vm.id"])
}

func TestStartVMCreateSpanWithNetwork(t *testing.T) {
	_, exp := setupTestTP(t)

	cfg := vm.Config{
		ImagePath:   "/img/disk.raw",
		Memory:      "512M",
		CPUs:        4,
		Name:        "netapp",
		NetworkName: "mynet",
	}
	span, _ := StartVMCreateSpan(context.Background(), cfg)
	span.End()

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	require.Equal(t, "vm.create", spans[0].Name)

	attrs := make(map[string]interface{})
	for _, a := range spans[0].Attributes {
		attrs[string(a.Key)] = a.Value.AsString()
	}
	require.Equal(t, "mynet", attrs["vm.network"])
	require.Equal(t, "netapp", attrs["vm.name"])
	require.Equal(t, "/img/disk.raw", attrs["vm.image"])
}

func TestSpanWithDuration(t *testing.T) {
	_, exp := setupTestTP(t)

	_, span := Tracer().Start(context.Background(), "op")
	SpanWithDuration(span, 3*time.Second+500*time.Millisecond)
	span.End()

	spans := exp.GetSpans()
	require.Len(t, spans, 1)

	for _, a := range spans[0].Attributes {
		if string(a.Key) == "vm.duration_seconds" {
			require.InDelta(t, 3.5, a.Value.AsFloat64(), 0.01)
			return
		}
	}
	t.Fatal("missing vm.duration_seconds attribute")
}

func TestStartVMLifecycleSpanDifferentStates(t *testing.T) {
	_, exp := setupTestTP(t)

	cases := []struct {
		state vm.State
		event string
	}{
		{vm.StateStarting, "begin"},
		{vm.StateRunning, "healthy"},
		{vm.StateStopped, "clean"},
	}

	for _, tc := range cases {
		v := &vm.VM{ID: "vm-1", Cfg: vm.Config{Name: "app"}, State: tc.state}
		span, _ := StartVMLifecycleSpan(context.Background(), v, tc.event)
		span.End()
	}

	spans := exp.GetSpans()
	require.Len(t, spans, len(cases))

	for i, tc := range cases {
		require.Equal(t, "vm.lifecycle", spans[i].Name)
		attrs := make(map[string]interface{})
		for _, a := range spans[i].Attributes {
			attrs[string(a.Key)] = a.Value.AsString()
		}
		require.Equal(t, string(tc.state), attrs["vm.state"])
		require.Equal(t, tc.event, attrs["vm.event"])
		require.Equal(t, "vm-1", attrs["vm.id"])
	}
}
