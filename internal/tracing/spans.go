package tracing

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/AitorConS/unikernel-engine/internal/vm"
)

const (
	VMLifecycleSpanName = "vm.lifecycle"
	VMCreateSpanName    = "vm.create"
	VMStartSpanName     = "vm.start"
	VMStopSpanName      = "vm.stop"
	VMKillSpanName      = "vm.kill"
	VMSignalSpanName    = "vm.signal"
	VMRemoveSpanName    = "vm.remove"
	VMMonitorSpanName   = "vm.monitor"
)

// StartVMLifecycleSpan creates a span for a VM lifecycle event.
func StartVMLifecycleSpan(ctx context.Context, v *vm.VM, event string) (trace.Span, context.Context) {
	ctx, span := Tracer().Start(ctx, VMLifecycleSpanName,
		trace.WithAttributes(
			attribute.String("vm.id", v.ID),
			attribute.String("vm.name", v.Cfg.Name),
			attribute.String("vm.state", string(v.State)),
			attribute.String("vm.event", event),
		),
	)
	return span, ctx
}

// StartVMCreateSpan creates a span for a VM creation.
func StartVMCreateSpan(ctx context.Context, cfg vm.Config) (trace.Span, context.Context) {
	attrs := []attribute.KeyValue{
		attribute.String("vm.image", cfg.ImagePath),
		attribute.String("vm.memory", cfg.Memory),
		attribute.Int("vm.cpus", cfg.CPUs),
	}
	if cfg.Name != "" {
		attrs = append(attrs, attribute.String("vm.name", cfg.Name))
	}
	if cfg.NetworkName != "" {
		attrs = append(attrs, attribute.String("vm.network", cfg.NetworkName))
	}
	ctx, span := Tracer().Start(ctx, VMCreateSpanName,
		trace.WithAttributes(attrs...),
	)
	return span, ctx
}

// StartVMStartSpan creates a span for starting a VM.
func StartVMStartSpan(ctx context.Context, id string) (trace.Span, context.Context) {
	ctx, span := Tracer().Start(ctx, VMStartSpanName,
		trace.WithAttributes(attribute.String("vm.id", id)),
	)
	return span, ctx
}

// StartVMStopSpan creates a span for stopping a VM.
func StartVMStopSpan(ctx context.Context, id string) (trace.Span, context.Context) {
	ctx, span := Tracer().Start(ctx, VMStopSpanName,
		trace.WithAttributes(attribute.String("vm.id", id)),
	)
	return span, ctx
}

// StartVMKillSpan creates a span for killing a VM.
func StartVMKillSpan(ctx context.Context, id string) (trace.Span, context.Context) {
	ctx, span := Tracer().Start(ctx, VMKillSpanName,
		trace.WithAttributes(attribute.String("vm.id", id)),
	)
	return span, ctx
}

// StartVMRemoveSpan creates a span for removing a VM.
func StartVMRemoveSpan(ctx context.Context, id string) (trace.Span, context.Context) {
	ctx, span := Tracer().Start(ctx, VMRemoveSpanName,
		trace.WithAttributes(attribute.String("vm.id", id)),
	)
	return span, ctx
}

// RecordError records an error on the span and sets its status to Error.
func RecordError(span trace.Span, err error) {
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// SpanWithRetryAttrs adds retry-related attributes to a span.
func SpanWithRetryAttrs(span trace.Span, policy vm.RestartPolicy, count int) {
	span.SetAttributes(
		attribute.String("vm.restart.policy", string(policy)),
		attribute.Int("vm.restart.count", count),
	)
}

// SpanWithDuration adds a duration attribute to a span.
func SpanWithDuration(span trace.Span, d time.Duration) {
	span.SetAttributes(
		attribute.Float64("vm.duration_seconds", d.Seconds()),
	)
}
