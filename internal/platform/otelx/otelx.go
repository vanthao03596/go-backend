// Package otelx bootstraps OpenTelemetry for the process: a tracer provider
// and a meter provider that export over OTLP gRPC, the W3C propagators, and
// Go runtime metrics.
//
// Exporters, sampling and extra resource attributes come from the standard
// OTEL_* environment variables, which the SDK reads itself:
// OTEL_EXPORTER_OTLP_ENDPOINT (default https://localhost:4317; an http://
// scheme disables TLS), OTEL_EXPORTER_OTLP_HEADERS, OTEL_TRACES_SAMPLER and
// OTEL_TRACES_SAMPLER_ARG, OTEL_METRIC_EXPORT_INTERVAL, OTEL_SERVICE_NAME and
// OTEL_RESOURCE_ATTRIBUTES.
//
// The package is named otelx, like slogx, so callers can import it next to
// go.opentelemetry.io/otel without an alias.
package otelx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"uuid"

	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// Config describes the service to the telemetry backend.
type Config struct {
	// Enabled builds exporting providers. When false Setup returns no-op
	// providers and touches no global state.
	Enabled bool
	// ServiceName is the default service.name; OTEL_SERVICE_NAME overrides it.
	ServiceName string
	// ServiceVersion becomes service.version. See Version.
	ServiceVersion string
	// Environment is the application environment (dev, test or prod),
	// reported as deployment.environment.name.
	Environment string
	// Log receives errors the SDK cannot return to a caller, such as failed
	// exports. Nil uses slog.Default.
	Log *slog.Logger
}

// Telemetry holds the providers the application reports to. Pass them to
// components explicitly; Setup also installs them as the otel globals.
type Telemetry struct {
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider

	shutdown func(context.Context) error
}

// Shutdown flushes buffered spans and metrics and stops the exporters. Call
// it once, after the server has drained, with a context that bounds how long
// the final export may take.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	if t.shutdown == nil {
		return nil
	}
	return t.shutdown(ctx)
}

// Disabled returns no-op providers. Nothing is recorded or exported.
func Disabled() *Telemetry {
	return &Telemetry{
		TracerProvider: tracenoop.NewTracerProvider(),
		MeterProvider:  metricnoop.NewMeterProvider(),
	}
}

// Setup builds the tracer and meter providers, installs them and the W3C
// trace-context and baggage propagators as the otel globals, starts runtime
// metrics and routes SDK errors to cfg.Log. The exporters connect lazily, so
// an unreachable collector does not fail startup; it shows up as logged
// export errors instead.
//
// If Setup returns an error, everything it started has been shut down.
func Setup(ctx context.Context, cfg Config) (*Telemetry, error) {
	if !cfg.Enabled {
		return Disabled(), nil
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}

	res, err := newResource(ctx, cfg)
	if err != nil {
		return nil, err
	}

	var shutdowns []func(context.Context) error
	shutdown := func(ctx context.Context) error {
		var errs []error
		for _, fn := range slices.Backward(shutdowns) {
			errs = append(errs, fn(ctx))
		}
		shutdowns = nil
		return errors.Join(errs...)
	}
	fail := func(err error) (*Telemetry, error) {
		return nil, errors.Join(err, shutdown(ctx))
	}

	traceExp, err := otlptracegrpc.New(ctx)
	if err != nil {
		return fail(fmt.Errorf("otlp trace exporter: %w", err))
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
	)
	shutdowns = append(shutdowns, tp.Shutdown)

	metricExp, err := otlpmetricgrpc.New(ctx)
	if err != nil {
		return fail(fmt.Errorf("otlp metric exporter: %w", err))
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
		sdkmetric.WithResource(res),
	)
	shutdowns = append(shutdowns, mp.Shutdown)

	if err := runtime.Start(runtime.WithMeterProvider(mp)); err != nil {
		return fail(fmt.Errorf("runtime metrics: %w", err))
	}

	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		log.Warn("opentelemetry", slog.Any("err", err))
	}))
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)

	return &Telemetry{TracerProvider: tp, MeterProvider: mp, shutdown: shutdown}, nil
}

// newResource describes this process. The defaults come first and the
// environment last, so OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES win.
func newResource(ctx context.Context, cfg Config) (*resource.Resource, error) {
	res, err := resource.New(ctx,
		resource.WithSchemaURL(semconv.SchemaURL),
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.ServiceVersion),
			// Distinguishes replicas, so cumulative metrics from two
			// instances never land on the same series.
			semconv.ServiceInstanceID(uuid.NewV7().String()),
			semconv.DeploymentEnvironmentNameKey.String(environmentName(cfg.Environment)),
		),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
		resource.WithFromEnv(),
	)
	if err != nil {
		// A partial resource is still usable; anything else is not.
		if errors.Is(err, resource.ErrPartialResource) && res != nil {
			return res, nil
		}
		return nil, fmt.Errorf("otel resource: %w", err)
	}
	return res, nil
}

// environmentName maps APP_ENV onto the well-known values of
// deployment.environment.name.
func environmentName(env string) string {
	switch env {
	case "dev":
		return "development"
	case "prod":
		return "production"
	default:
		return env
	}
}

// Version picks the service.version to report: override when set (from
// -ldflags "-X main.version=..."), else the main module version stamped by
// go build, else the VCS revision, else "dev". Builds outside a checkout
// (the container build has no .git) rely on the override.
func Version(override string) string {
	if override != "" {
		return override
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var rev string
	dirty := false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "dev"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}
