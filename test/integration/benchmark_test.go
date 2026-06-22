//go:build integration

package integration

import (
	"context"
	"testing"
	"time"
)

// TestBenchmark is the load suite: provision a validator chain + RPC fleet,
// drive seiload against the fleet for the configured duration, and upload the
// report. Replaces the load-test Chaos-Mesh Workflow.
//
// Inputs (env, mirroring k8s_nightly.yml):
//
//	SEI_CHAIN_ID   per-run chain id (e.g. bench-<run-id>)   [required]
//	SEID_IMAGE     seid image under test                    [required]
//	SEI_RUN_ID     unique run id (sei.io/harness-run)       [default: SEI_CHAIN_ID]
//	SEI_NAMESPACE  shared nightly namespace                 [default: SDK default]
func TestBenchmark(t *testing.T) {
	requireCluster(t)

	chainID := mustEnv(t, "SEI_CHAIN_ID")
	s := spec{
		chainID:    chainID,
		runID:      env("SEI_RUN_ID", chainID),
		namespace:  env("SEI_NAMESPACE", ""),
		seidImage:  mustEnv(t, "SEID_IMAGE"),
		validators: 4,
		rpcNodes:   2, // seiload fans across both via the EVM endpoint list
		timeout:    90 * time.Minute,
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	c := openClient(ctx, t)

	ch, err := provision(ctx, c, s)
	cleanupChain(t, ch)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	t.Logf("provisioned %s: %d validators + %d RPC followers; EVM endpoints=%v",
		s.chainID, s.validators, len(ch.rpcNodes), ch.evmEndpoints())

	// TODO(WS-I step 1, next increment): drive seiload as a DECOUPLED unit
	// (decision D3) — apply seiload's own manifest parameterized with
	// ch.evmEndpoints(), stamped sei.io/harness-run=s.runID; wait for
	// completion; read its report from the agreed S3 path; assert TPS/receipts.
	// seiload's Job spec is NOT constructed here.
	t.Skip("seiload drive + report not yet wired (WS-I step 1 next increment)")
}
