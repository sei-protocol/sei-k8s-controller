package main

import (
	"context"
	"testing"
)

// The controller exits at startup when initMeterProvider fails, so a resource
// that cannot be built takes the whole manager down. resource.Default() carries
// the OTel SDK's own semconv schema URL; merging attributes stamped with a
// different semconv version fails with "conflicting Schema URL". A dependency
// bump that moves the SDK's schema without moving the semconv import here
// breaks startup, and this test is what catches it before a deploy does.
func TestInitMeterProvider_BuildsResource(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	mp, err := initMeterProvider(context.Background())
	if err != nil {
		t.Fatalf("initMeterProvider: %v", err)
	}
	if err := mp.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}
