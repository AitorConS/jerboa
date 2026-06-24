//go:build linux

package tracing

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/AitorConS/unikernel-engine/internal/vm"
)

func setupTestTP(t *testing.T) (*sdktrace.TracerProvider, *tracetest.InMemoryExporter) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exp),
	)
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
	})
	return tp, exp
}

func TestNewProviderNoop(t *testing.T) {
	p, err := NewProvider(context.Background(), "", "test")
	require.NoError(t, err)
	require.NotNil(t, p)
	require.NoError(t, p.Shutdown(context.Background()))
}

func TestStartVMLifecycleSpan(t *testing.T) {
	_, exp := setupTestTP(t)

	v := &vm.VM{ID: "test-vm", Cfg: vm.Config{Name: "myapp"}, State: vm.StateRunning}
	span, _ := StartVMLifecycleSpan(context.Background(), v, "stop")
	span.End()

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	require.Equal(t, "vm.lifecycle", spans[0].Name)
}

func TestStartVMCreateSpan(t *testing.T) {
	_, exp := setupTestTP(t)

	cfg := vm.Config{ImagePath: "/path/to/image", Memory: "256M", CPUs: 2, Name: "test-vm"}
	span, _ := StartVMCreateSpan(context.Background(), cfg)
	span.End()

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	require.Equal(t, "vm.create", spans[0].Name)
}

func TestStartVMOperationSpans(t *testing.T) {
	_, exp := setupTestTP(t)

	span, _ := StartVMStartSpan(context.Background(), "vm-1")
	span.End()

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	require.Equal(t, "vm.start", spans[0].Name)
}

func TestRecordError(t *testing.T) {
	_, exp := setupTestTP(t)

	_, span := Tracer().Start(context.Background(), "op")
	RecordError(span, context.DeadlineExceeded)
	span.End()

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	require.Equal(t, "context deadline exceeded", spans[0].Status.Description)
}

func TestSpanWithRetryAttrs(t *testing.T) {
	_, exp := setupTestTP(t)

	_, span := Tracer().Start(context.Background(), "restart")
	SpanWithRetryAttrs(span, vm.RestartAlways, 3)
	span.End()

	spans := exp.GetSpans()
	require.Len(t, spans, 1)

	attrs := make(map[string]interface{})
	for _, a := range spans[0].Attributes {
		attrs[string(a.Key)] = a.Value.AsString()
	}
	require.Equal(t, "always", attrs["vm.restart.policy"])
}
