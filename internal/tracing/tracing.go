//go:build linux

// Package tracing provides OpenTelemetry tracing for the unikernel engine daemon.
package tracing

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Tracer returns the global tracer for the unikernel engine.
func Tracer() trace.Tracer {
	return otel.Tracer("github.com/AitorConS/jerboa")
}

// Provider wraps an OpenTelemetry TracerProvider with shutdown capability.
type Provider struct {
	tp         *sdktrace.TracerProvider
	shutdownFn func(context.Context) error
}

// NewProvider creates an OTLP gRPC TracerProvider that exports to addr.
// If addr is empty, a no-op provider is returned.
func NewProvider(ctx context.Context, addr string, version string) (*Provider, error) {
	if addr == "" {
		return noopProvider(), nil
	}

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(addr),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("tracing: create OTLP exporter: %w", err)
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceNameKey.String("jerboad"),
			semconv.ServiceVersionKey.String(version),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("tracing: merge resources: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	otel.SetTracerProvider(tp)

	return &Provider{
		tp: tp,
		shutdownFn: func(ctx context.Context) error {
			if err := tp.Shutdown(ctx); err != nil {
				return fmt.Errorf("tracing: tracer provider shutdown: %w", err)
			}
			return nil
		},
	}, nil
}

// Shutdown gracefully shuts down the tracer provider, flushing any pending spans.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p.shutdownFn != nil {
		return p.shutdownFn(ctx)
	}
	return nil
}

func noopProvider() *Provider {
	return &Provider{
		tp:         sdktrace.NewTracerProvider(),
		shutdownFn: nil,
	}
}
