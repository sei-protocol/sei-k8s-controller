package main

import (
	"context"
	"fmt"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// buildVersion is set via ldflags at build time.
var buildVersion = "dev"

// initMeterProvider creates an OTel MeterProvider with two readers:
//   - A Prometheus exporter that registers into controller-runtime's metrics.Registry,
//     making OTel instruments appear on the existing /metrics endpoint alongside
//     controller-runtime's own workqueue/reconcile metrics.
//   - An OTLP exporter (when OTEL_EXPORTER_OTLP_ENDPOINT is set) that pushes to
//     whatever backend the user configures.
func initMeterProvider(ctx context.Context) (*sdkmetric.MeterProvider, error) {
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName("sei-k8s-controller"),
			semconv.ServiceVersion(buildVersion),
			attribute.String("k8s.pod.name", os.Getenv("POD_NAME")),
			attribute.String("k8s.namespace.name", os.Getenv("POD_NAMESPACE")),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("building OTel resource: %w", err)
	}

	var opts []sdkmetric.Option
	opts = append(opts, sdkmetric.WithResource(res))

	// Prometheus reader: bridges OTel instruments into controller-runtime's
	// existing /metrics endpoint. WithoutScopeInfo and WithoutTargetInfo
	// suppress otel_scope_info and target_info metrics that would add noise
	// alongside controller-runtime's own Prometheus output.
	promReader, err := promexporter.New(
		promexporter.WithRegisterer(crmetrics.Registry),
		promexporter.WithoutScopeInfo(),
		promexporter.WithoutTargetInfo(),
	)
	if err != nil {
		return nil, fmt.Errorf("creating Prometheus exporter: %w", err)
	}
	opts = append(opts, sdkmetric.WithReader(promReader))

	// OTLP reader: optional, enabled by standard OTel env var.
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" {
		otlpExporter, err := otlpmetricgrpc.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("creating OTLP exporter: %w", err)
		}
		opts = append(opts, sdkmetric.WithReader(
			sdkmetric.NewPeriodicReader(otlpExporter),
		))
	}

	mp := sdkmetric.NewMeterProvider(opts...)
	otel.SetMeterProvider(mp)

	return mp, nil
}
