//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

// snapshotProductionConfig makes the witness nodes emit CometBFT state-sync
// snapshots so a wiped follower can re-bootstrap from them. Overlaid on the
// memiavl baseline for the network in this suite, it reaches BOTH witnesses: it
// is applied to the network (so the genesis validator inherits it) and to the
// rpc follower via provision's storageConfig, so both witnesses serve chunks.
// Without it, the resync has nothing to sync FROM and the node-side
// WaitCaughtUp assertion after workflow Complete never clears (the workflow
// itself completes at mark-ready and does not gate on catch-up).
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
// The provision + witness + follower bring-up is shared verbatim with
// TestGigaStoreMigration via bringUpStateSyncFollower (that sibling reruns this
// same round trip WITH a giga config migration). This test is the plain-resync
// baseline: it stays independent and carries no migration.
//
// Witnesses: the follower's RpcServers are TWO DISTINCT full-node RPC endpoints —
// genesis validator-0 and the provisioned rpc follower (rpcNodes[0]) — each a
// caught-up node that serves snapshot chunks (nodeRPC below). Two DISTINCT
// witnesses drawn from different nodes (rather than two equal validators) keeps
// liveness robust: the chain needs only its single validator online, and there is
// no 2-validator genesis blocksync-bootstrap deadlock to stall on. The
// >=2-DISTINCT requirement is NOT a CometBFT property — it is the served CRD field
// (SnapshotSource.RpcServers is a MinItems=2 listType=set) plus the controller's
// canonical-syncer floor, which sort+dedups before counting. A duplicated set is
// rejected at admission or collapses below the floor, so no state-sync plan is
// built (fail CLOSED) and the follower falls back to a genesis block-sync — which
// the earliest>1 assertion would then catch. Distinct light-client sources are
// also the point of a witness set; an aggregate round-robin service counted twice
// is unsound.
//
// Cluster dependencies the CronJob wiring owns (documented, not asserted here):
//   - the deployed controller + seid image carry the hold-aware start gate and
//     the mark-not-ready/stop-seid/reset-data sidecar tasks;
//   - both witnesses (validator-0 and the rpc follower) produce state-sync
//     snapshots (snapshotProductionConfig, applied to both — see above).
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

	// State-sync needs >=2 DISTINCT light-client witnesses (nodeRPC below). This
	// suite draws them from TWO DIFFERENT nodes — the single genesis validator and
	// the one provisioned rpc follower — so a single validator suffices. A second
	// equal validator would be strictly more fragile (both must stay online for
	// 2/3, and some seid images stall a 2-validator genesis in a blocksync
	// bootstrap deadlock), so keep the default at one.
	validators := envInt(t, "SEI_VALIDATORS", 1)

	// Provision the snapshot-producing chain and bring up a plain state-sync
	// follower bootstrapped from the two DISTINCT witnesses, verified to have
	// started FROM a snapshot (earliest > 1 — the pre-wipe floor).
	f := bringUpStateSyncFollower(ctx, t, c, chainID, ns, seid, validators)

	// Re-bootstrap that state-sync follower through the StateSync recipe. Leave the
	// recipe's RpcServers nil: the workflow planner inherits the node's already-
	// resolved DISTINCT witnesses (node.Status.ResolvedStateSyncers, set from this
	// ssNode's spec rpcServers = the two witnesses above) when the recipe carries
	// none (internal/planner/workflow.go BuildPlan). Passing the aggregate
	// round-robin RPC twice would be an unsound duplicate witness set that the
	// planner's raw-len count does not dedup — nil sidesteps it and reuses the
	// vetted distinct set. No migration: this is the plain-resync baseline.
	wf, err := c.CreateWorkflow(ctx, sei.WorkflowSpec{
		Name:      "resync-" + chainID,
		Namespace: ns,
		Node:      f.node.Name(),
		Kind:      sei.WorkflowStateSync,
		Labels:    map[string]string{runLabelKey: chainID},
		StateSync: &sei.StateSyncWorkflow{RpcServers: nil},
	})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	cleanupWorkflow(t, wf)
	t.Logf("workflow %s: created against %s", wf.Name(), f.node.Name())

	// Fast-fail terminal-shape gate: bound WaitTerminal by a TIGHT child timeout
	// (not the 60m scenario ctx) so a wedged recipe fails FAST and legibly with the
	// recorded status instead of hanging to the scenario deadline.
	awaitWorkflowComplete(ctx, t, wf, workflowWaitTimeout)
	t.Logf("workflow %s: complete", wf.Name())

	// Re-bootstrap observable: after the wipe + resync the follower rejoins and
	// catches up again (it cannot catch up to a chain it is not synced with, so
	// this covers the wiped-then-synced round trip) and serves EVM again.
	if err := sei.WaitCaughtUp(ctx, f.hc, f.node.TendermintRPC()); err != nil {
		t.Fatalf("follower %s not caught up after resync: %v", f.node.Name(), err)
	}
	if err := sei.WaitEVMServing(ctx, f.hc, f.node.EVMRPC()); err != nil {
		t.Errorf("follower %s EVM not serving after resync: %v", f.node.Name(), err)
	}

	// Prove a genuine wipe + fresh re-sync via a DISCONTINUITY in the block-store
	// base, not merely a rising earliest: on a running full node routine pruning
	// also advances earliest_block_height, so postEarliest > preEarliest alone
	// would false-green a no-op workflow. Two things make the jump conclusive:
	// this follower pins chain.min_retain_blocks=0 (so block-store pruning cannot
	// move the base at all), and a fresh state-sync restore lands on a NEW snapshot
	// at least a full snapshot_interval higher than the previous one. Incremental pruning
	// cannot move the base a whole interval inside the test window; only a wipe +
	// restore can. So require the base to have jumped by >= one interval.
	postEarliest, ok := sei.EarliestHeight(ctx, f.hc, f.node.TendermintRPC())
	if !ok {
		t.Fatalf("read post-workflow earliest height of %q", f.node.Name())
	}
	if postEarliest-f.preEarliest < int64(f.interval) {
		t.Fatalf("follower %s earliest height %d -> %d after resync (jump %d), want a jump >= snapshot_interval %d (a genuine wipe + re-sync lands on a snapshot at least one interval higher; a smaller move is routine pruning, not a fresh restore)",
			f.node.Name(), f.preEarliest, postEarliest, postEarliest-f.preEarliest, f.interval)
	}
	t.Logf("follower %s: caught up + EVM serving, block-store base jumped %d -> %d (>= interval %d) after state-sync re-bootstrap — TestWorkflowStateSync OK", f.node.Name(), f.preEarliest, postEarliest, f.interval)

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

// stateSyncFollower is the pre-workflow fixture the state-sync suites share: a
// snapshot-producing chain plus a plain RPC follower bootstrapped from two
// DISTINCT witnesses, verified to have started FROM a snapshot. preEarliest is
// the pre-wipe block-store floor the post-workflow discontinuity assertion must
// exceed; hc/interval carry the poll client and the snapshot cadence both suites
// reuse.
type stateSyncFollower struct {
	node        *sei.Node
	hc          *http.Client
	interval    int
	preEarliest int64
}

// bringUpStateSyncFollower provisions the snapshot-producing chain (1 validator +
// N rpc followers, all memiavl + snapshot-producing) and brings up a plain
// state-sync RPC follower against TWO DISTINCT witnesses, blocking until it is
// running + caught up and verified to have started FROM a snapshot (earliest > 1).
// It registers teardown for everything it creates and t.Fatalf's on any failure,
// so the caller gets a ready fixture or a failed test — never a half-built one.
//
// The two DISTINCT full-node RPC endpoints (bare host:port, the CRD
// SnapshotSource.RpcServers shape) are genesis validator-0 (service <chainID>-0)
// and the provisioned rpc follower (rpcNodeName(chainID,0) = <chainID>-rpc-0).
// The witness namespace is derived from the aggregate RPC host rather than the ns
// input, which may be "" — the SDK resolves an empty namespace to a
// kubeconfig/SA default that only the served endpoint reflects. The aggregate
// host is <chainID>-internal.<ns>.svc[.cluster.local]; chainID carries no dots,
// so the second dotted label is the namespace.
func bringUpStateSyncFollower(ctx context.Context, t *testing.T, c *sei.Client, chainID, ns, seid string, validators int) stateSyncFollower {
	t.Helper()

	ch, err := provision(ctx, t, c, spec{
		chainID:       chainID,
		runID:         chainID,
		namespace:     ns,
		seidImage:     seid,
		validators:    validators,
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

	u, err := url.Parse(ch.network.TendermintRPC())
	if err != nil {
		t.Fatalf("parse network TM RPC %q: %v", ch.network.TendermintRPC(), err)
	}
	hostLabels := strings.Split(u.Hostname(), ".")
	if len(hostLabels) < 2 || hostLabels[1] == "" {
		t.Fatalf("cannot derive namespace from aggregate RPC host %q", u.Host)
	}
	witnessNS := hostLabels[1]
	witnesses := []string{
		nodeRPC(fmt.Sprintf("%s-0", chainID), witnessNS), // genesis validator-0
		nodeRPC(rpcNodeName(chainID, 0), witnessNS),      // the provisioned rpc follower
	}

	// Bring up a state-sync-bootstrapped follower: it must fetch a snapshot from a
	// peer rather than replay from genesis. Appended to ch.rpcNodes so cleanupChain
	// (registered above, evaluated at teardown) reaps it too.
	//
	// Pin the block-store base with chain.min_retain_blocks=0. earliest_block_height
	// is the CometBFT BLOCK-store base, governed by min-retain-blocks (0 = keep all,
	// no Tendermint block pruning); storage.pruning is sei-config STATE-store pruning,
	// a different subsystem that does NOT move the block-store base. We set
	// min_retain_blocks explicitly (it defaults to 0, but stating it makes the
	// base-pin a fact of the recipe, not an inherited default) so earliest moves
	// ONLY on a genuine wipe + fresh restore — which is what the discontinuity
	// assertion relies on. storage.pruning=nothing is kept for the state store
	// (unpruned state is also what snapshot production needs). memiavl matches the
	// network's write-mode (the nightly image rejects the controller default; see
	// memiavlStorageConfig).
	ssNode, err := c.CreateNode(ctx, sei.NodeSpec{
		Name:      "statesync-" + chainID,
		Network:   chainID,
		Namespace: ns,
		Image:     seid,
		Labels:    map[string]string{runLabelKey: chainID},
		Config: mergeConfig(memiavlStorageConfig, map[string]string{
			"storage.pruning":         "nothing", // state-store pruning off
			"chain.min_retain_blocks": "0",       // block-store base pin (keep all blocks)
		}),
		StateSync: &sei.NodeStateSync{RpcServers: witnesses},
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
	// Capture the tip too: the post-workflow discontinuity check is about the base
	// (earliest) jumping, not the tip advancing — logging both makes the jump
	// legible against ordinary block production.
	preLatest, ok := sei.LatestHeight(ctx, hc, ssNode.TendermintRPC())
	if !ok {
		t.Fatalf("read latest height of %q", ssNode.Name())
	}
	t.Logf("state-sync node %s: bootstrapped from snapshot (earliest %d, latest %d)", ssNode.Name(), preEarliest, preLatest)

	return stateSyncFollower{node: ssNode, hc: hc, interval: interval, preEarliest: preEarliest}
}

// workflowWaitTimeout is the TIGHT child budget for one resync workflow's
// WaitTerminal — well under the 60m scenario ctx so a wedged recipe step fails
// FAST and legibly with the recorded status instead of stalling to the scenario
// deadline. The recipe ends at mark-ready (Complete == released, catch-up is
// asserted node-side below), so a healthy run completes in minutes and this
// budget bounds only the mutation steps, of which reset-data is the slowest.
const workflowWaitTimeout = 15 * time.Minute

// awaitWorkflowComplete blocks on wf.WaitTerminal under a tight child timeout and
// asserts the terminal shape is Complete. On failure OR timeout it renders the
// workflow's recorded status (phase + failed-task detail + Failed condition) into
// the t.Fatalf so the cause is legible in the pod log rather than a bare
// deadline. Shared by both state-sync suites.
func awaitWorkflowComplete(ctx context.Context, t *testing.T, wf *sei.Workflow, timeout time.Duration) {
	t.Helper()
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := wf.WaitTerminal(wctx); err != nil {
		t.Fatalf("workflow %s did not reach Complete: %v%s", wf.Name(), err, workflowFailureDetail(wf))
	}
	if got := wf.Phase(); got != sei.WorkflowPhaseComplete {
		t.Fatalf("workflow %s terminal phase = %q, want Complete%s", wf.Name(), got, workflowFailureDetail(wf))
	}
}

// workflowFailureDetail renders the workflow's recorded status for a failure
// message: status.phase, the failed task's type+error, and the Failed condition's
// reason+message. Returns "" if the raw object is unavailable. Reads the workflow's
// raw CR via Object() to render the recorded status.
func workflowFailureDetail(wf *sei.Workflow) string {
	obj, ok := wf.Object().(*v1alpha1.SeiNodeTaskWorkflow)
	if !ok || obj == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, " (status.phase=%q", obj.Status.Phase)
	if p := obj.Status.Plan; p != nil && p.FailedTaskDetail != nil {
		fmt.Fprintf(&b, "; failed task %q: %s", p.FailedTaskDetail.Type, p.FailedTaskDetail.Error)
	}
	for i := range obj.Status.Conditions {
		if cnd := obj.Status.Conditions[i]; cnd.Type == v1alpha1.ConditionSeiNodeTaskWorkflowFailed && cnd.Status == metav1.ConditionTrue {
			fmt.Fprintf(&b, "; Failed=%s: %s", cnd.Reason, cnd.Message)
		}
	}
	b.WriteString(")")
	return b.String()
}

// nodeRPC is a SeiNode's CometBFT RPC as a bare host:port state-sync witness (no
// scheme — the CRD SnapshotSource.RpcServers shape, ^[^\s:/,]+:[0-9]{1,5}$).
// service is the controller's per-node headless Service name, which the
// controller creates same-named as the SeiNode exposing CometBFT RPC on 26657
// (noderesource.GenerateHeadlessService), resolvable at
// <service>.<ns>.svc.cluster.local. A SeiNetwork names each child validator
// <chainID>-<ordinal> (seinetwork.seiNodeName); a standalone follower is named by
// the harness (rpcNodeName => <chainID>-rpc-<ordinal>). Distinct services yield
// the distinct witnesses the state-sync floor requires.
func nodeRPC(service, ns string) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local:26657", service, ns)
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
