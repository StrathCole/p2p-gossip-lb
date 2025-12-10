package tracing

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
)

// Config configures OpenTelemetry tracing.
type Config struct {
	Enabled     bool
	ServiceName string
	ServiceVer  string
	Environment string
	Endpoint    string  // OTLP gRPC endpoint
	SampleRate  float64 // 0.0 to 1.0
	Headers     map[string]string
}

// Provider wraps the trace provider for cleanup.
type Provider struct {
	tp *sdktrace.TracerProvider
}

// Shutdown gracefully shuts down the trace provider.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p.tp != nil {
		return p.tp.Shutdown(ctx)
	}
	return nil
}

// NewProvider creates and registers a global trace provider.
func NewProvider(cfg Config) (*Provider, error) {
	if !cfg.Enabled {
		// Return a no-op provider
		otel.SetTracerProvider(trace.NewNoopTracerProvider())
		return &Provider{}, nil
	}

	// Create resource
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.ServiceVer),
			semconv.DeploymentEnvironment(cfg.Environment),
		),
	)
	if err != nil {
		return nil, err
	}

	// Create OTLP exporter
	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
	}

	if len(cfg.Headers) > 0 {
		opts = append(opts, otlptracegrpc.WithHeaders(cfg.Headers))
	}

	// In production, would use WithTLSCredentials
	opts = append(opts, otlptracegrpc.WithInsecure())

	client := otlptracegrpc.NewClient(opts...)
	exporter, err := otlptrace.New(context.Background(), client)
	if err != nil {
		return nil, err
	}

	// Configure sampler
	var sampler sdktrace.Sampler
	if cfg.SampleRate >= 1.0 {
		sampler = sdktrace.AlwaysSample()
	} else if cfg.SampleRate <= 0.0 {
		sampler = sdktrace.NeverSample()
	} else {
		sampler = sdktrace.TraceIDRatioBased(cfg.SampleRate)
	}

	// Create trace provider
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)

	// Set global provider
	otel.SetTracerProvider(tp)

	// Set global propagator for distributed tracing
	otel.SetTextMapPropagator(
		propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		),
	)

	return &Provider{tp: tp}, nil
}

// Tracer returns a tracer for the given name.
func Tracer(name string) trace.Tracer {
	return otel.Tracer(name)
}

// StartSpan starts a new span with the given name in the given context.
func StartSpan(ctx context.Context, name string) (context.Context, trace.Span) {
	return otel.Tracer("mesh").Start(ctx, name)
}
