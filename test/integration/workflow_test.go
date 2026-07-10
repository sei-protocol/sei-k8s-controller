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

// snapshotProductionConfig makes the witness validators emit CometBFT state-sync
// snapshots so a wiped follower can re-bootstrap from them. Overlaid on the
// memiavl baseline for the network in this suite. Without it, the resync has
// nothing to sync FROM and the workflow's await-caught-up step never clears.
var snapshotProductionConfig = map[string]string{
	"state-sync.snapshot-interval":    "50",
	"state-sync.snapshot-keep-recent": "3",
}

// TestWorkflowStateSync drives the SeiNodeTaskWorkflow StateSync recipe through
// the SDK end to end: provision a chain + one caught-up RPC follower, then
// CreateWorkflow(StateSync) against the follower and WaitWorkflowTerminal. The
// recipe holds seid, wipes the follower's data directory, re-configures
// state-sync against the witnesses, and releases — so a Complete workflow with
// the follower caught up AGAIN is the re-bootstrap observable (wiped-then-synced,
// no manual data-directory surgery). Acceptance criterion 5.
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
	node := ch.rpcNodes[0]
	hc := &http.Client{Timeout: 10 * time.Second}

	// Re-bootstrap the follower through the StateSync recipe.
	wf, err := c.CreateWorkflow(ctx, sei.WorkflowSpec{
		Name:      "resync-" + chainID,
		Namespace: ns,
		Node:      node.Name(),
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
	t.Logf("workflow %s: created against %s", wf.Name(), node.Name())

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
	if err := sei.WaitCaughtUp(ctx, hc, node.TendermintRPC()); err != nil {
		t.Fatalf("follower %s not caught up after resync: %v", node.Name(), err)
	}
	if err := sei.WaitEVMServing(ctx, hc, node.EVMRPC()); err != nil {
		t.Errorf("follower %s EVM not serving after resync: %v", node.Name(), err)
	}
	t.Logf("follower %s: caught up + EVM serving after state-sync re-bootstrap — TestWorkflowStateSync OK", node.Name())

	// Interrupt-resume variant (deferred): kill the follower pod mid-recipe and
	// assert the workflow resumes to Complete. Deferred here — it needs a
	// mid-step injection hook to kill the pod during a specific recipe step
	// (racing pod-kill against the whole recipe is flaky). The controller/sidecar
	// resume path is covered deterministically by the controller envtest
	// (TestWorkflowLifecycle_ReadoptByUIDAfterRestart) and the sidecar
	// crash-resume tests; the e2e pod-kill variant lands with that hook.
	t.Run("InterruptResume", func(t *testing.T) {
		t.Skip("deferred: needs a mid-recipe pod-kill injection hook; controller-restart resume is covered by the controller envtest")
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
