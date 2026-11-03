// Package otelxtest records spans and metrics in memory so tests can assert
// on the telemetry a component emits. Pass Recorder.TracerProvider and
// Recorder.MeterProvider wherever production code takes providers.
package otelxtest

import (
	"context"
	"slices"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Recorder holds SDK providers backed by an in-memory span recorder and a
// manual metric reader.
type Recorder struct {
	TracerProvider *sdktrace.TracerProvider
	MeterProvider  *sdkmetric.MeterProvider

	spans  *tracetest.SpanRecorder
	reader *sdkmetric.ManualReader
}

// New returns a Recorder that samples every span. Its providers are shut
// down when the test ends.
func New(t testing.TB) *Recorder {
	t.Helper()
	spans := tracetest.NewSpanRecorder()
	reader := sdkmetric.NewManualReader()
	r := &Recorder{
		// AlwaysSample explicitly: the default reads OTEL_TRACES_SAMPLER,
		// which a developer's .env could set.
		TracerProvider: sdktrace.NewTracerProvider(
			sdktrace.WithSampler(sdktrace.AlwaysSample()),
			sdktrace.WithSpanProcessor(spans),
		),
		MeterProvider: sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)),
		spans:         spans,
		reader:        reader,
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_ = r.TracerProvider.Shutdown(ctx)
		_ = r.MeterProvider.Shutdown(ctx)
	})
	return r
}

// Spans returns the spans that have ended so far, in end order.
func (r *Recorder) Spans() []sdktrace.ReadOnlySpan {
	return r.spans.Ended()
}

// SpansNamed returns the ended spans called name.
func (r *Recorder) SpansNamed(name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range r.spans.Ended() {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

// Metric collects and returns the metric called name, or false if no
// instrument of that name has recorded anything.
func (r *Recorder) Metric(t testing.TB, name string) (metricdata.Metrics, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m, true
			}
		}
	}
	return metricdata.Metrics{}, false
}

// Sum adds up the int64 counter data points of name whose attributes include
// every one of attrs. A metric that never recorded sums to zero.
func (r *Recorder) Sum(t testing.TB, name string, attrs ...attribute.KeyValue) int64 {
	t.Helper()
	m, ok := r.Metric(t, name)
	if !ok {
		return 0
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("%s is %T, want an int64 sum", name, m.Data)
	}
	var total int64
	for _, dp := range sum.DataPoints {
		if hasAll(dp.Attributes, attrs) {
			total += dp.Value
		}
	}
	return total
}

// HistogramCount returns how many int64 or float64 measurements were
// recorded on histogram name with attributes including attrs, and the int64
// sum of their values (zero for float64 histograms).
func (r *Recorder) HistogramCount(t testing.TB, name string, attrs ...attribute.KeyValue) (count uint64, sum int64) {
	t.Helper()
	m, ok := r.Metric(t, name)
	if !ok {
		return 0, 0
	}
	switch h := m.Data.(type) {
	case metricdata.Histogram[int64]:
		for _, dp := range h.DataPoints {
			if hasAll(dp.Attributes, attrs) {
				count += dp.Count
				sum += dp.Sum
			}
		}
	case metricdata.Histogram[float64]:
		for _, dp := range h.DataPoints {
			if hasAll(dp.Attributes, attrs) {
				count += dp.Count
			}
		}
	default:
		t.Fatalf("%s is %T, want a histogram", name, m.Data)
	}
	return count, sum
}

// Attr returns the value of key on span s, or an invalid value if absent.
func Attr(s sdktrace.ReadOnlySpan, key attribute.Key) attribute.Value {
	for _, kv := range s.Attributes() {
		if kv.Key == key {
			return kv.Value
		}
	}
	return attribute.Value{}
}

func hasAll(set attribute.Set, attrs []attribute.KeyValue) bool {
	return !slices.ContainsFunc(attrs, func(want attribute.KeyValue) bool {
		got, ok := set.Value(want.Key)
		return !ok || got != want.Value
	})
}
