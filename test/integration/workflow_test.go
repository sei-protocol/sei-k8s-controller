//go:build integration

package integration

import (
	"context"
	"net/http"
	"net/url"
	"os/signal"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

// snapshotProductionConfig makes the witness validators emit CometBFT state-sync
// snapshots so a wiped follower can re-bootstrap from them. Overlaid on the
// memiavl baseline for the network in this suite. Without it, the resync has
// nothing to sync FROM and the workflow's await-caught-up step never clears.
//
// Keys are the sei-config storage.* overrides that sei-config.SnapshotGenerationOverrides
// emits (storage.snapshot_interval / storage.snapshot_keep_recent, plus
// storage.pruning=nothing — snapshotting needs unpruned state). They are
// underscore-form enrichment keys the real sidecar validates; the earlier
// hyphenated state-sync.* form was rejected by sei-config as unknown fields
// (found live on harbor — see the config-intent-validation note in the CL).
// snapshot_interval is a small test value (the sei-config default is 2000
// blocks) so a snapshot appears early on an ephemeral chain.
var snapshotProductionConfig = map[string]string{
	"storage.pruning":              "nothing",
	"storage.snapshot_interval":    "50",
	"storage.snapshot_keep_recent": "3",
}

// TestWorkflowStateSync drives the full state-sync procedure through the SDK end
// to end: provision a snapshot-producing chain, wait past the snapshot interval,
// then bring up a state-sync-bootstrapped follower and assert it started FROM a
// snapshot (earliest retained height > 1, not a genesis replay's 1). It then runs
// the SeiNodeTaskWorkflow StateSync recipe against THAT follower — the recipe
// holds seid, wipes the data directory, re-configures state-sync against the
// witnesses, and releases — and asserts a Complete workflow with the follower
// caught up AGAIN and its earliest height advanced past the pre-wipe floor: proof
// of a genuine wipe + fresh re-sync to a newer snapshot, not a no-op. Acceptance
// criterion 5.
//
// Witnesses: the recipe's RpcServers are the network's aggregate TM RPC (the
// validator service), passed as the >=2 the controller's fail-closed floor
// requires; CometBFT dedups identical entries. A dedicated witness topology
// (N distinct RPC endpoints) is a later refinement.
//
// Cluster dependencies the CronJob wiring owns (documented, not asserted here):
//   - the deployed controller + seid image carry the hold-aware start gate and
//     the mark-not-ready/stop-seid/reset-data sidecar tasks;
//   - the witness validators produce state-sync snapshots (snapshotProductionConfig).
//
// Inputs (env): SEI_CHAIN_ID, SEID_IMAGE [required]; SEI_NAMESPACE,
// SEI_VALIDATORS [optional]. Run as the nightly CronJob:
//
//	["-test.run", "TestWorkflowStateSync", "-test.v", "-test.timeout", "0"]
func TestWorkflowStateSync(t *testing.T) {
	requireCluster(t)
	chainID := runChainID(mustEnv(t, "SEI_CHAIN_ID"))
	seid := mustEnv(t, "SEID_IMAGE")
	ns := envOr("SEI_NAMESPACE", "")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	c := openClient(ctx, t)

	ch, err := provision(ctx, t, c, spec{
		chainID:       chainID,
		runID:         chainID,
		namespace:     ns,
		seidImage:     seid,
		validators:    envInt(t, "SEI_VALIDATORS", 1),
		rpcNodes:      1,
		storageConfig: mergeConfig(memiavlStorageConfig, snapshotProductionConfig),
	})
	cleanupChain(t, ch)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	hc := &http.Client{Timeout: 10 * time.Second}

	// A follower can only state-sync from a snapshot that already exists. The
	// witness validators emit one every storage.snapshot_interval blocks
	// (snapshotProductionConfig); advance the aggregate RPC one interval + a
	// margin to guarantee at least one snapshot boundary is crossed before we
	// bootstrap from it. Derive the delta from the config so retuning the
	// interval can't silently under-wait and make the state-sync assertion flaky.
	interval, err := strconv.Atoi(snapshotProductionConfig["storage.snapshot_interval"])
	if err != nil {
		t.Fatalf("snapshot_interval not an int: %v", err)
	}
	if err := sei.WaitHeightAdvances(ctx, hc, ch.network.TendermintRPC(), int64(interval)+10); err != nil {
		t.Fatalf("network did not advance past the snapshot interval: %v", err)
	}

	// Witnesses are the aggregate validator TM RPC as BARE host:port (the CRD
	// SnapshotSource.RpcServers shape); TendermintRPC() is a URL, so strip the
	// scheme. One aggregate witness passed twice satisfies the >=2 floor —
	// CometBFT dedups identical entries; a distinct N-witness topology is a later
	// refinement.
	u, err := url.Parse(ch.network.TendermintRPC())
	if err != nil {
		t.Fatalf("parse network TM RPC %q: %v", ch.network.TendermintRPC(), err)
	}
	witness := u.Host

	// Bring up a state-sync-bootstrapped follower: it must fetch a snapshot from a
	// peer rather than replay from genesis. Appended to ch.rpcNodes so cleanupChain
	// (registered above, evaluated at teardown) reaps it too.
	ssNode, err := c.CreateNode(ctx, sei.NodeSpec{
		Name:      "statesync-" + chainID,
		Network:   chainID,
		Namespace: ns,
		Image:     seid,
		Labels:    map[string]string{runLabelKey: chainID},
		StateSync: &sei.NodeStateSync{RpcServers: []string{witness, witness}},
	})
	if ssNode != nil {
		ch.rpcNodes = append(ch.rpcNodes, ssNode)
	}
	if err != nil {
		t.Fatalf("create state-sync node: %v", err)
	}
	if err := ssNode.WaitReady(ctx); err != nil {
		t.Fatalf("state-sync node %q running: %v", ssNode.Name(), err)
	}
	if err := sei.WaitCaughtUp(ctx, hc, ssNode.TendermintRPC()); err != nil {
		t.Fatalf("state-sync node %q caught up: %v", ssNode.Name(), err)
	}

	// A state-synced node starts from a snapshot, so its earliest retained height
	// is > 1; a genesis replay would report 1. This is the proof the bootstrap
	// took the state-sync path, and the pre-wipe floor the post-workflow earliest
	// must exceed.
	preEarliest, ok := sei.EarliestHeight(ctx, hc, ssNode.TendermintRPC())
	if !ok {
		t.Fatalf("read earliest height of %q", ssNode.Name())
	}
	if preEarliest <= 1 {
		t.Fatalf("state-sync node %q earliest height = %d, want > 1 (a genesis replay reports 1)", ssNode.Name(), preEarliest)
	}
	t.Logf("state-sync node %s: bootstrapped from snapshot (earliest height %d)", ssNode.Name(), preEarliest)

	// Re-bootstrap that state-sync follower through the StateSync recipe.
	wf, err := c.CreateWorkflow(ctx, sei.WorkflowSpec{
		Name:      "resync-" + chainID,
		Namespace: ns,
		Node:      ssNode.Name(),
		Kind:      sei.WorkflowStateSync,
		Labels:    map[string]string{runLabelKey: chainID},
		StateSync: &sei.StateSyncWorkflow{
			RpcServers: []string{ch.network.TendermintRPC(), ch.network.TendermintRPC()},
		},
	})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	cleanupWorkflow(t, wf)
	t.Logf("workflow %s: created against %s", wf.Name(), ssNode.Name())

	if err := wf.WaitTerminal(ctx); err != nil {
		t.Fatalf("workflow %s did not complete: %v", wf.Name(), err)
	}
	if got := wf.Phase(); got != sei.WorkflowPhaseComplete {
		t.Fatalf("workflow terminal phase = %q, want Complete", got)
	}
	t.Logf("workflow %s: complete", wf.Name())

	// Re-bootstrap observable: after the wipe + resync the follower rejoins and
	// catches up again (it cannot catch up to a chain it is not synced with, so
	// this covers the wiped-then-synced round trip) and serves EVM again.
	if err := sei.WaitCaughtUp(ctx, hc, ssNode.TendermintRPC()); err != nil {
		t.Fatalf("follower %s not caught up after resync: %v", ssNode.Name(), err)
	}
	if err := sei.WaitEVMServing(ctx, hc, ssNode.EVMRPC()); err != nil {
		t.Errorf("follower %s EVM not serving after resync: %v", ssNode.Name(), err)
	}

	// A genuine wipe + fresh re-sync lands on a NEW (higher) snapshot, so the
	// earliest retained height must rise past the pre-workflow floor; a no-op
	// recipe would leave earliest unchanged.
	postEarliest, ok := sei.EarliestHeight(ctx, hc, ssNode.TendermintRPC())
	if !ok {
		t.Fatalf("read post-workflow earliest height of %q", ssNode.Name())
	}
	if postEarliest <= preEarliest {
		t.Fatalf("follower %s earliest height = %d after resync, want > %d (a genuine wipe + re-sync lands on a newer snapshot)", ssNode.Name(), postEarliest, preEarliest)
	}
	t.Logf("follower %s: caught up + EVM serving, earliest advanced %d -> %d after state-sync re-bootstrap — TestWorkflowStateSync OK", ssNode.Name(), preEarliest, postEarliest)

	// Interrupt-resume variant (deferred): kill the follower pod mid-recipe and
	// assert the workflow resumes to Complete. Deferred here — it needs a
	// mid-step injection hook to kill the pod during a specific recipe step
	// (racing pod-kill against the whole recipe is flaky). The controller/sidecar
	// resume path is covered deterministically by the controller envtest
	// (TestWorkflowLifecycle_ReadoptByUIDAfterRestart) and the sidecar
	// crash-resume tests; the e2e pod-kill variant lands with that hook.
	t.Run("InterruptResume", func(t *testing.T) {
		t.Skip("deferred: needs a mid-recipe pod-kill hook; restart-resume is covered by the controller envtest")
	})
}

// cleanupWorkflow deletes the workflow on a fresh context (the scenario ctx may
// be expired). A completed workflow's finalizer is already reaped, so the delete
// resolves promptly; best-effort like the chain teardown.
func cleanupWorkflow(t *testing.T, wf *sei.Workflow) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := wf.Delete(ctx); err != nil {
			t.Errorf("delete workflow %q: %v", wf.Name(), err)
		}
	})
}
