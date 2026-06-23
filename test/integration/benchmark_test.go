//go:build integration

package integration

import (
	"context"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// TestBenchmark provisions a validator chain + RPC fleet, drives seiload against
// the fleet for the configured duration, and asserts the chain stayed live under
// load. The load suite.
//
// Inputs (env, mirroring k8s_nightly.yml):
//
//	SEI_CHAIN_ID     per-run chain id (e.g. bench-<run-id>)   [required]
//	SEID_IMAGE       seid image under test                    [required]
//	SEILOAD_IMAGE    sei-load benchmark image                 [required]
//	SEI_RUN_ID       unique run id (sei.io/harness-run)       [default: SEI_CHAIN_ID]
//	SEI_NAMESPACE    shared nightly namespace                 [default: SDK default]
//	SEILOAD_PROFILE  profile name in seiload-profiles         [default: nightly_evm_transfer]
//	DURATION_MINUTES seiload run length                       [default: 10]
//	SEILOAD_COMMIT_ID sei-chain commit label for metrics      [default: ""]
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
		chainID:        chainID,
		runID:          envOr("SEI_RUN_ID", chainID),
		namespace:      envOr("SEI_NAMESPACE", ""),
		seidImage:      mustEnv(t, "SEID_IMAGE"),
		validators:     4,
		rpcNodes:       2, // seiload fans across both via the EVM endpoint list
		timeout:        90 * time.Minute,
		seiloadImage:   mustEnv(t, "SEILOAD_IMAGE"),
		seiloadProfile: envOr("SEILOAD_PROFILE", "nightly_evm_transfer"),
		seiloadCommit:  envOr("SEILOAD_COMMIT_ID", ""),
		durationMin:    envInt(t, "DURATION_MINUTES", 10),
		storageConfig:  memiavlStorageConfig,
		// EVM tuning the followers need to absorb the load (matches the load
		// scenario's rpc overrides).
		rpcConfig: map[string]string{
			"evm.worker_pool_size":  "32",
			"evm.worker_queue_size": "4000",
			"evm.max_tx_pool_txs":   "10000",
		},
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
	cs := clientset(t)

	ch, err := provision(ctx, t, c, s)
	cleanupChain(t, ch)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Logf("provisioned %s: %d validators + %d RPC followers; EVM endpoints=%v",
		s.chainID, s.validators, len(ch.rpcNodes), ch.evmEndpoints())

	runSeiload(ctx, t, cs, ch, s)
}
