package otelx

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

func resourceAttr(t *testing.T, cfg Config, key attribute.Key) string {
	t.Helper()
	res, err := newResource(t.Context(), cfg)
	if err != nil {
		t.Fatalf("newResource: %v", err)
	}
	v, ok := res.Set().Value(key)
	if !ok {
		t.Fatalf("resource has no %s: %v", key, res)
	}
	return v.AsString()
}

func TestResourceDescribesTheService(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
	cfg := Config{ServiceName: "shortener", ServiceVersion: "v1.2.3", Environment: "prod"}

	for key, want := range map[attribute.Key]string{
		semconv.ServiceNameKey:               "shortener",
		semconv.ServiceVersionKey:            "v1.2.3",
		semconv.DeploymentEnvironmentNameKey: "production",
		semconv.TelemetrySDKLanguageKey:      "go",
	} {
		if got := resourceAttr(t, cfg, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if got := resourceAttr(t, cfg, semconv.ServiceInstanceIDKey); got == "" {
		t.Error("service.instance.id is empty")
	}
}

func TestEnvironmentOverridesResourceDefaults(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "shortener-canary")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.version=override,team=links")
	cfg := Config{ServiceName: "shortener", ServiceVersion: "v1.2.3", Environment: "dev"}

	if got := resourceAttr(t, cfg, semconv.ServiceNameKey); got != "shortener-canary" {
		t.Errorf("service.name = %q, want OTEL_SERVICE_NAME", got)
	}
	if got := resourceAttr(t, cfg, semconv.ServiceVersionKey); got != "override" {
		t.Errorf("service.version = %q, want OTEL_RESOURCE_ATTRIBUTES", got)
	}
	if got := resourceAttr(t, cfg, "team"); got != "links" {
		t.Errorf("team = %q", got)
	}
	if got := resourceAttr(t, cfg, semconv.DeploymentEnvironmentNameKey); got != "development" {
		t.Errorf("deployment.environment.name = %q, want development", got)
	}
}

func TestVersionPrefersOverride(t *testing.T) {
	if got := Version("v9.9.9"); got != "v9.9.9" {
		t.Errorf("Version(override) = %q", got)
	}
	// Test binaries carry no module version or VCS stamp worth asserting on;
	// whatever the fallback finds, it must not be empty.
	if got := Version(""); got == "" {
		t.Error("Version fallback is empty")
	}
}

func TestDisabledRecordsNothing(t *testing.T) {
	tel, err := Setup(t.Context(), Config{Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	_, span := tel.TracerProvider.Tracer("t").Start(t.Context(), "op")
	defer span.End()
	if span.IsRecording() {
		t.Error("disabled telemetry produced a recording span")
	}
	if err := tel.Shutdown(t.Context()); err != nil {
		t.Errorf("Shutdown = %v", err)
	}
}

// Setup must not need a reachable collector: the exporters dial lazily.
func TestSetupWithoutCollector(t *testing.T) {
	prevTP, prevMP := otel.GetTracerProvider(), otel.GetMeterProvider()
	prevProp, prevHandler := otel.GetTextMapPropagator(), otel.GetErrorHandler()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetMeterProvider(prevMP)
		otel.SetTextMapPropagator(prevProp)
		otel.SetErrorHandler(prevHandler)
	})
	// Nothing listens on port 1.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")

	tel, err := Setup(t.Context(), Config{Enabled: true, ServiceName: "test", ServiceVersion: "v0", Environment: "test"})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	_, span := otel.Tracer("t").Start(t.Context(), "op")
	if !span.IsRecording() {
		t.Error("global tracer provider was not installed")
	}
	span.End()

	// The final export cannot succeed; Shutdown must still return once its
	// context ends.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = tel.Shutdown(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown ignored its context")
	}
}
