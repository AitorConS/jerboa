//go:build linux || (darwin && arm64)

// Package tracing provides OpenTelemetry tracing for the unikernel engine daemon.
package tracing

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/credentials"
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

// exportInsecure reports whether spans may go to addr unencrypted: only to a
// collector on this host, or when OTEL_EXPORTER_OTLP_INSECURE=true asks for
// it. Spans name VMs, images and networks, so they use TLS otherwise.
func exportInsecure(addr string) bool {
	if v, err := strconv.ParseBool(os.Getenv("OTEL_EXPORTER_OTLP_INSECURE")); err == nil {
		return v
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// NewProvider creates an OTLP gRPC TracerProvider that exports to addr.
// If addr is empty, a no-op provider is returned.
func NewProvider(ctx context.Context, addr string, version string) (*Provider, error) {
	if addr == "" {
		return noopProvider(), nil
	}

	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(addr)}
	if exportInsecure(addr) {
		opts = append(opts, otlptracegrpc.WithInsecure())
	} else {
		opts = append(opts, otlptracegrpc.WithTLSCredentials(credentials.NewClientTLSFromCert(nil, "")))
	}
	exporter, err := otlptracegrpc.New(ctx, opts...)
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
