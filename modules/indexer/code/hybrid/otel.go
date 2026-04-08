// Copyright 2024 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package hybrid

import (
	"context"
	"os"

	prometheusbridge "go.opentelemetry.io/contrib/bridges/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"forgejo.org/modules/log"
)

const tracerName = "forgejo.org/modules/indexer/code/hybrid"

// tracer is the package-level tracer, initialised by initTracer().
var tracer trace.Tracer = otel.Tracer(tracerName)

// initTracer sets up the OTLP gRPC trace + metrics exporters and installs a W3C
// TraceContext propagator.  Existing Prometheus metrics are bridged to OTel so
// they flow to the same OTLP backend.
//
// Returns a shutdown function the caller must invoke on process exit.
// If OTEL_EXPORTER_OTLP_ENDPOINT is empty the function is a no-op.
func initTracer(ctx context.Context) (shutdown func(context.Context) error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = "nuc01:4317"
	}

	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceNameKey.String("forgejo-hybrid-indexer"),
	)

	// ---- Traces ----
	traceExp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		log.Warn("hybrid: failed to create OTel trace exporter: %v — tracing disabled", err)
		return func(context.Context) error { return nil }
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	tracer = tp.Tracer(tracerName)

	// ---- Metrics (prometheus bridge → OTLP) ----
	metricExp, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpoint(endpoint),
		otlpmetricgrpc.WithInsecure(),
	)
	if err != nil {
		log.Warn("hybrid: failed to create OTel metric exporter: %v — metrics disabled", err)
		return tp.Shutdown
	}

	// Bridge all metrics already registered with the default Prometheus registry
	// (via promauto) into the OTel MeterProvider, then export via OTLP.
	promProducer := prometheusbridge.NewMetricProducer()
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp,
			sdkmetric.WithProducer(promProducer),
		)),
	)
	otel.SetMeterProvider(mp)

	return func(ctx context.Context) error {
		if err := mp.Shutdown(ctx); err != nil {
			log.Warn("hybrid: OTel meter provider shutdown error: %v", err)
		}
		return tp.Shutdown(ctx)
	}
}
