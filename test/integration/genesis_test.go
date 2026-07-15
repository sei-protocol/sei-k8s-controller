//go:build integration

package integration

import (
	"context"
	"net/http"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

// TestGenesisCeremonyProducesBlocks proves the slice-6 genesis-completion fix end
// to end: a fresh, self-created SeiNetwork collect-gentxs ceremony must boot its
// founding validators and produce blocks past height 1, asserted on the founding
// validators' OWN RPC.
//
// Why this topology (rpcNodes: 0, validators >= 2):
//   - rpcNodes: 0 makes the verdict depend SOLELY on the founding validators. A
//     genesis/InitChain divergence cannot be masked by a follower that merely
//     replays committed blocks — there is no follower. The aggregate TM RPC the
//     assertions hit IS the validator service.
//   - validators >= 2 exercises the ceremony's distinct-power + dedup path:
//     each founding validator is a distinct SeiNode that runs its own
//     ValidateNodeKey + GenerateGentx tasks, so it contributes a distinct
//     consensus key and voting power to the collected gentx set. A single
//     validator would never surface a set-assembly bug.
//
// Why height > 1 on the validators' own RPC is the end-to-end InitChain-parity
// proof (the load-bearing assertion):
//
//	sei-chain's replay.go:224 hard-errors EVERY validator at boot if
//	genDoc.Validators != the InitChain output validator set. So a block committed
//	past height 1 by the founding validators is only possible if the two sets
//	agree — i.e. a committed height > 1 is the executed proto.Equal set-equality
//	witness for the pure-Go validator-set derivation in seictl's
//	assemble_genesis_validators.go. If that derivation drifts from what seid's
//	InitChain produces, consensus never advances past height 1 and WaitCaughtUp
//	times out here rather than being papered over by a follower.
//
// WaitCaughtUp gates height > 1 with catching_up == false (the ceremony produced
// AND the validators joined consensus); WaitHeightAdvances(+2) then proves
// SUSTAINED production, catching a chain that commits a block or two and then
// stalls (a stalled node still reports catching_up == false at a frozen height).
//
// A UNIQUE runChainID per run is mandatory: genesis is keyed by chain id and a
// reused static id collides with a prior run's persisted genesis (harness_test.go
// §runChainID), which itself halts a fresh validator set at height 1 — a false
// positive for the very failure this scenario hunts. runChainID appends a
// nanosecond token so every run gets a fresh genesis and node keys.
//
// memiavl is pinned explicitly: the controller default write-mode is rejected by
// the nightly image, so without it the founding validators never reach Ready
// (harness_test.go §memiavlStorageConfig).
//
// Inputs (env): SEID_IMAGE [required]; SEI_CHAIN_ID (base id, a per-run token is
// appended) [default: genesis-ceremony]; SEI_NAMESPACE [default: SDK default];
// SEI_VALIDATORS (>= 2) [default: 2]; SEI_SIDECAR_IMAGE (pin the seictl sidecar
// build under test) [default: platform default]. Run as the nightly CronJob with
// -test.timeout 0 (see TestNightlyBenchmark for why the scenario ctx, not the test-runner
// alarm, owns the deadline):
//
//	["-test.run", "TestGenesisCeremonyProducesBlocks", "-test.v", "-test.timeout", "0"]
func TestGenesisCeremonyProducesBlocks(t *testing.T) {
	requireCluster(t)

	chainID := runChainID(envOr("SEI_CHAIN_ID", "genesis-ceremony"))
	seid := mustEnv(t, "SEID_IMAGE")
	ns := envOr("SEI_NAMESPACE", "")
	// Pin the seictl sidecar image so the ceremony assembles genesis with the
	// build under test; "" => platform default (the normal nightly path).
	sidecar := envOr("SEI_SIDECAR_IMAGE", "")

	// The scenario's premise is a multi-validator ceremony (distinct-power + dedup);
	// a below-2 override would silently degrade it to a single-validator boot that
	// never exercises set assembly. Fail fast rather than pass vacuously.
	validators := envInt(t, "SEI_VALIDATORS", 2)
	if validators < 2 {
		t.Fatalf("SEI_VALIDATORS=%d: this scenario needs >= 2 to exercise the distinct-power + dedup ceremony path", validators)
	}

	// Short envelope: this is a boot + first-few-blocks assertion, not a load run.
	// The scenario ctx owns the deadline (nested under the CronJob
	// activeDeadlineSeconds); a -test.timeout breach would bypass t.Cleanup.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	// SIGTERM (activeDeadlineSeconds grace, or a manual delete) cancels ctx so the
	// SDK calls unwind and teardown runs before SIGKILL; SIGKILL itself is
	// backstopped by the label-GC sweep.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	c := openClient(ctx, t)

	// rpcNodes: 0 — the founding validators are the sole subject of the verdict.
	ch, err := provision(ctx, t, c, spec{
		chainID:       chainID,
		runID:         chainID,
		namespace:     ns,
		seidImage:     seid,
		sidecarImage:  sidecar,
		validators:    validators,
		rpcNodes:      0,
		storageConfig: memiavlStorageConfig,
	})
	cleanupChain(t, ch)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Logf("network %s: ready (%d founding validators, no followers)", chainID, validators)

	// Assert directly on the founding validators' aggregate RPC — never a follower.
	tmRPC := ch.network.TendermintRPC()
	if tmRPC == "" {
		t.Fatalf("network %s exposes no Tendermint RPC after Ready", chainID)
	}
	hc := &http.Client{Timeout: 10 * time.Second}

	// InitChain-parity proof: a committed height > 1 (not catching up) on the
	// validators' own RPC witnesses genDoc.Validators == InitChain output
	// (replay.go:224 would have hard-errored the validators at boot otherwise).
	if err := sei.WaitCaughtUp(ctx, hc, tmRPC); err != nil {
		t.Fatalf("founding validators never committed past height 1 (genesis/InitChain divergence?): %v", err)
	}
	t.Logf("network %s: founding validators committed past height 1 — genesis validator set matches InitChain output", chainID)

	// Sustained production: +2 heights from the first observed height proves the
	// ceremony chain keeps making blocks, not that it committed once and stalled.
	if err := sei.WaitHeightAdvances(ctx, hc, tmRPC, 2); err != nil {
		t.Fatalf("founding validators stalled after boot (no sustained block production): %v", err)
	}
	t.Logf("network %s: sustained block production confirmed — TestGenesisCeremonyProducesBlocks OK", chainID)
}
