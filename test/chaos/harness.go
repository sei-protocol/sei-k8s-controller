//go:build chaos

// Package chaos is the Go-native chaos/integration test harness. Each suite is a
// standard Go test (TestNightlyLoad, …) that drives the sei SDK to provision a
// SeiNetwork + RPC SeiNodes, runs its payload, and asserts — replacing the bash
// in platform's k8s_nightly.yml. Suites are selected with `go test -run`, shaped
// with standard flags (-timeout, -count=1), and configured by env (the same keys
// the workflow already sets). The `chaos` build tag plus the SEI_CHAOS_CLUSTER
// gate keep these out of unit-test CI: they require a live cluster.
package chaos

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"
)

// config carries the run shape. It mirrors the k8s_nightly.yml workflow inputs so
// the harness is a drop-in for the bash; every field has a nightly-matching
// default so a bare `go test -run TestNightlyLoad` is runnable.
type config struct {
	Namespace      string
	ChainID        string
	SeidImage      string
	ValidatorCount int
	RPCCount       int
	Duration       time.Duration
}

// loadConfig reads the run shape from the environment. SEID_IMAGE has no safe
// default (it pins the build under test), so a missing value skips the suite
// rather than testing a stale image.
func loadConfig(t *testing.T) config {
	t.Helper()
	if os.Getenv("SEI_CHAOS_CLUSTER") == "" {
		t.Skip("SEI_CHAOS_CLUSTER unset — chaos suites require a live cluster")
	}
	img := os.Getenv("SEID_IMAGE")
	if img == "" {
		t.Skip("SEID_IMAGE unset — nothing to test")
	}
	return config{
		Namespace:      getenv("NAMESPACE", "nightly"),
		ChainID:        getenv("CHAIN_ID", "bench-local"),
		SeidImage:      img,
		ValidatorCount: getenvInt(t, "VALIDATOR_COUNT", 4),
		RPCCount:       getenvInt(t, "RPC_COUNT", 2),
		Duration:       time.Duration(getenvInt(t, "DURATION_MINUTES", 10)) * time.Minute,
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(t *testing.T, key string, def int) int {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("%s=%q is not an integer: %v", key, v, err)
	}
	return n
}

// The caught-up readiness gate (height>1 && catching_up==false) now lives in the
// SDK as sei.WaitCaughtUp — a generally-useful, mode-agnostic provisioning
// primitive shared with the seitask Task steps; this harness calls it directly.

// runSeiloadSuite is the load-suite payload seam: render the seiload profile
// against the live RPC endpoints, apply the seiload Job, tail it to Complete,
// gate on success, and upload the report to S3. This is the half with real
// choices (profile templating, success-gating, report shape) — wired next.
func runSeiloadSuite(ctx context.Context, t *testing.T, cfg config, rpcEVMEndpoints []string) error {
	t.Helper()
	if err := ctx.Err(); err != nil {
		return err // caller's deadline already blown before the load step
	}
	// TODO(harness): seiload Job driver + S3 report. Currently a near-no-op seam
	// so the provisioning spine, caught-up gate, and teardown are exercisable on a
	// real cluster before the load driver lands.
	t.Logf("seiload seam (stub): suite=%s endpoints=%d duration=%s", cfg.ChainID, len(rpcEVMEndpoints), cfg.Duration)
	return nil
}
