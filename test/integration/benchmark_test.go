//go:build integration

package integration

import (
	"context"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/sei-protocol/sei-k8s-controller/internal/keygen"
	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

// benchmarkWorkload is one load shape TestNightlyBenchmark runs on its own chain.
type benchmarkWorkload struct {
	name        string // the workload label on its metrics, which alerts-nightly judges it by
	seidImage   string // seid image under test
	profile     string // profile name in seiload-profiles
	durationMin int    // seiload run length, minutes
	// fund runs on a real-balance chain: a throwaway sei-load root key is
	// funded at genesis and the profile's funding block disperses from it.
	fund bool
	// minReceiptCoverage is the share of accepted transactions whose execution
	// status sei-load must read for the run to count; 0 skips the check.
	minReceiptCoverage float64
}

// steadyStateRootBalance funds the steady-state root at genesis. Its profile
// disperses 5 SEI to each of 500 accounts (2,500 SEI); the rest covers the
// root's deploys and disperse batches with wide margin.
const steadyStateRootBalance = "1000000000000usei"

// TestNightlyBenchmark runs each load workload against its own fresh validator
// chain + RPC fleet and fails only when the run isn't valid data (see
// runSeiload). Throughput and cost are judged by alerts-nightly, per workload:
//
//   - saturation: unlimited-rate EVM transfers on the mock_balances image, for
//     peak throughput (the < 500 TPS floor).
//   - steady-state: a fixed 200 TPS AMM / ERC20 / transfer mix on the vanilla
//     image with real balances, for what a fixed load costs the chain (included
//     TPS, block interval and validator CPU / memory against recorded values).
//
// Inputs (env, mirroring k8s_nightly.yml):
//
//	SEI_CHAIN_ID     base chain id (a per-run token is appended) [required]
//	SEID_IMAGE_MOCK  seid image for saturation, mock_balances    [required]
//	SEID_IMAGE       seid image for steady-state, vanilla        [required]
//	SEILOAD_IMAGE    sei-load benchmark image                    [required]
//	SEI_NAMESPACE    shared nightly namespace                    [default: SDK default]
//	SEILOAD_PROFILE  saturation profile in seiload-profiles      [default: nightly_evm_transfer]
//	DURATION_MINUTES saturation run length                       [default: 10]
//	SEILOAD_COMMIT_ID sei-chain commit label for metrics         [default: ""]
//
// Deadlines: the CronJob MUST run this with `-test.timeout 0` (or safely above
// the scenario timeout). A -test.timeout breach panics and bypasses t.Cleanup,
// so the scenario ctx below — not the test-runner alarm — must own the deadline,
// nested inside the CronJob activeDeadlineSeconds (the SIGKILL backstop the
// label-GC sweep covers).
func TestNightlyBenchmark(t *testing.T) {
	requireCluster(t)
	base := runChainID(mustEnv(t, "SEI_CHAIN_ID"))
	ns := envOr("SEI_NAMESPACE", "")
	seiloadImage := mustEnv(t, "SEILOAD_IMAGE")
	commit := envOr("SEILOAD_COMMIT_ID", "")

	workloads := []benchmarkWorkload{
		{
			// No receipt coverage floor: at saturation sei-load drops most
			// status reads at its tracker cap by design.
			name:        "saturation",
			seidImage:   mustEnv(t, seidImageMockEnv),
			profile:     envOr("SEILOAD_PROFILE", "nightly_evm_transfer"),
			durationMin: envInt(t, "DURATION_MINUTES", 10),
		},
		{
			name:               "steady-state",
			seidImage:          mustEnv(t, seidImageEnv),
			profile:            "nightly_steady_state",
			durationMin:        30,
			fund:               true,
			minReceiptCoverage: 0.99,
		},
	}
	for _, w := range workloads {
		t.Run(w.name, func(t *testing.T) {
			id := base + "-" + w.name
			// The chain's longest label value is the last follower's pod name,
			// <id>-rpc-1-0 (statefulset.kubernetes.io/pod-name), capped at 63
			// chars; fail loud rather than on an opaque admission rejection.
			if v := rpcNodeName(id, 1) + "-0"; len(v) > 63 {
				t.Fatalf("chain id %q yields label value %q > 63 chars", id, v)
			}
			s := spec{
				chainID:    id,
				runID:      id,
				namespace:  ns,
				seidImage:  w.seidImage,
				validators: 4,
				rpcNodes:   2, // one takes the sends, the other serves receipts (see runSeiload)
				// The load plus 80m for provisioning, catch-up checks and teardown.
				timeout:            time.Duration(w.durationMin+80) * time.Minute,
				seiloadImage:       seiloadImage,
				seiloadProfile:     w.profile,
				seiloadCommit:      commit,
				seiloadWorkload:    w.name,
				durationMin:        w.durationMin,
				minReceiptCoverage: w.minReceiptCoverage,
				storageConfig:      memiavlStorageConfig,
				// EVM tuning the followers need to absorb the load (matches the load
				// scenario's rpc overrides).
				rpcConfig: map[string]string{
					"evm.worker_pool_size":  "32",
					"evm.worker_queue_size": "4000",
					"evm.max_tx_pool_txs":   "10000",
				},
			}
			if w.fund {
				root, err := keygen.DeriveEVM()
				if err != nil {
					t.Fatalf("derive root key: %v", err)
				}
				s.accounts = []sei.GenesisAccount{{Address: root.Address, Balance: steadyStateRootBalance}}
				s.seiloadRootKey = root.PrivateKeyHex
				t.Logf("sei-load root %s (%s) funded %s at genesis", root.EVMAddress, root.Address, steadyStateRootBalance)
			}

			ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
			defer cancel()
			// SIGTERM (the activeDeadlineSeconds grace period, or a manual pod delete)
			// cancels ctx so the SDK calls unwind and t.Cleanup teardown runs before the
			// kubelet SIGKILLs the pod. SIGKILL itself still bypasses cleanup — that is
			// what the label-GC sweep backstops.
			ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
			defer stop()

			c := openClient(ctx, t)
			cs := clientset(t)

			ch, err := provision(ctx, t, c, s)
			cleanupChain(t, ch)
			if err != nil {
				t.Fatalf("provision: %v", err)
			}
			t.Logf("provisioned %s: %d validators + %d RPC followers", s.chainID, s.validators, len(ch.rpcNodes))

			runSeiload(ctx, t, cs, ch, s)
		})
	}
}
