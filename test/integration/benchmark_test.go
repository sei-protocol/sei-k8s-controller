//go:build integration

package integration

import (
	"context"
	"os/signal"
	"syscall"
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
//
// Deadlines: the CronJob MUST run this with `-test.timeout 0` (or safely above
// the scenario timeout). A -test.timeout breach panics and bypasses t.Cleanup,
// so the scenario ctx below — not the test-runner alarm — must own the deadline,
// nested inside the CronJob activeDeadlineSeconds (the SIGKILL backstop the
// label-GC sweep covers).
func TestBenchmark(t *testing.T) {
	requireCluster(t)

	chainID := mustEnv(t, "SEI_CHAIN_ID")
	s := spec{
		chainID:    chainID,
		runID:      envOr("SEI_RUN_ID", chainID),
		namespace:  envOr("SEI_NAMESPACE", ""),
		seidImage:  mustEnv(t, "SEID_IMAGE"),
		validators: 4,
		rpcNodes:   2, // seiload fans across both via the EVM endpoint list
		timeout:    90 * time.Minute,
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	// SIGTERM (the activeDeadlineSeconds grace period, or a manual pod delete)
	// cancels ctx so the SDK calls unwind and t.Cleanup teardown runs before the
	// kubelet SIGKILLs the pod. SIGKILL itself still bypasses cleanup — that is
	// what the label-GC sweep backstops.
	ctx, stopSignals := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()

	c := openClient(ctx, t)

	ch, err := provision(ctx, t, c, s)
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
	t.Skipf("provisioned %s (%d validators + %d followers); seiload drive + report not yet wired — tearing down",
		s.chainID, s.validators, len(ch.rpcNodes))
}
