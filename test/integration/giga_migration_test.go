//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

// taskTypeConfigPatch pins seictl's config-patch task wire type. The workflow
// planner stamps it as PlannedTask.Type when it materializes a ConfigMigration
// into the recipe (sidecar.TaskTypeConfigPatch == "config-patch"); the value is
// a wire contract, so a literal here is the honest coupling: the suite does not
// import internal/planner to borrow the constant (see the harness package doc
// for the dependency boundary).
const taskTypeConfigPatch = "config-patch"

// TestGigaStoreMigration drives the giga SS-store migration through the SDK end
// to end, as a sibling of TestWorkflowStateSync (it reuses that round trip's
// provision + witness + follower bring-up verbatim via bringUpStateSyncFollower)
// but with a ConfigMigration carried inside the StateSync recipe. Four signals,
// in order:
//
//  1. terminal shape — the workflow reaches Complete under a tight child timeout
//     (a wedged giga boot fails fast and legibly, not to the scenario deadline);
//  2. THE ANCHOR — the compiled status.plan carries a config-patch step whose
//     app.toml tree is exactly the giga enabling flags (state-store ss-enable /
//     evm-ss-split / ss-backend, state-commit sc-enable). This proves the
//     ConfigMigration -> render -> planner materialization end to end and cannot
//     false-green: it inspects the persisted plan, not a log line;
//  3. discontinuity — the block-store base jumps >= one snapshot_interval, the
//     same genuine-wipe+resync discriminator the baseline uses;
//  4. giga runtime — after the resync the node is Ready + caught up + EVM-serving
//     WHILE running under evm-ss-split=true. Per the giga migration doc's startup
//     safety checks a node with evm-ss-split=true and an unpopulated EVM-SS layout
//     refuses to boot, so a healthy boot + catch-up + EVM-serving IS the
//     SDK-observable evidence that giga went live.
//
// Assumptions / honest ceiling:
//   - This assumes SEID_IMAGE is a giga-capable seid build (infra owns image
//     selection; there is no image-capability gate or env flag here — the nightly
//     runs it against the latest image).
//   - The stronger "seid HONORED the giga config (vs silently ignored it and
//     booted a legacy layout)" discriminator would read a seid startup log line;
//     it is DEFERRED pending a harness pod-log affordance. Signal 4 is the current
//     ceiling: with evm-ss-split=true a legacy-layout boot fails the safety check,
//     so a served EVM endpoint is strong (not absolute) evidence.
//
// Inputs (env): SEI_CHAIN_ID, SEID_IMAGE [required]; SEI_NAMESPACE,
// SEI_VALIDATORS [optional]. Run as the nightly CronJob:
//
//	["-test.run", "TestGigaStoreMigration", "-test.v", "-test.timeout", "0"]
func TestGigaStoreMigration(t *testing.T) {
	requireCluster(t)
	chainID := runChainID(mustEnv(t, "SEI_CHAIN_ID"))
	seid := mustEnv(t, "SEID_IMAGE")
	ns := envOr("SEI_NAMESPACE", "")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	c := openClient(ctx, t)
	// Four validators, the harness convention — see TestWorkflowStateSync's
	// rationale (consensus-paced production keeps the witness follower at head).
	validators := envInt(t, "SEI_VALIDATORS", 4)

	// Same fixture as the plain-resync baseline: snapshot-producing chain + a
	// state-sync follower verified to have started FROM a snapshot (earliest > 1 —
	// the PRE-migration state).
	f := bringUpStateSyncFollower(ctx, t, c, chainID, ns, seid, validators)

	// Re-bootstrap the follower through StateSync WITH a giga migration. RpcServers
	// nil inherits the node's resolved DISTINCT witnesses (as in the baseline);
	// Backend is pinned pebbledb so the anchor assertion has a concrete value to
	// match, rather than relying on the CRD default having run.
	wf, err := c.CreateWorkflow(ctx, sei.WorkflowSpec{
		Name:      "giga-" + chainID,
		Namespace: ns,
		Node:      f.node.Name(),
		Kind:      sei.WorkflowStateSync,
		Labels:    map[string]string{runLabelKey: chainID},
		StateSync: &sei.StateSyncWorkflow{
			Migration: &sei.ConfigMigration{
				Kind:      sei.ConfigMigrationGigaStore,
				GigaStore: &sei.GigaStoreMigration{Backend: "pebbledb"},
			},
			RpcServers: nil,
		},
	})
	if err != nil {
		t.Fatalf("create giga workflow: %v", err)
	}
	cleanupWorkflow(t, wf)
	t.Logf("workflow %s: created against %s (giga, backend pebbledb)", wf.Name(), f.node.Name())

	// (a) Terminal shape under a tight child timeout — fail fast + legibly.
	awaitWorkflowComplete(ctx, t, wf, workflowWaitTimeout)
	t.Logf("workflow %s: complete", wf.Name())

	// (b) THE ANCHOR: the compiled plan's config-patch step carries exactly the
	// giga app.toml tree. Reads the persisted status.plan (not a log), so it
	// cannot false-green.
	assertGigaConfigPatch(t, wf, "pebbledb")

	// (c) Discontinuity: a genuine wipe + fresh re-sync moves the block-store base
	// by >= one snapshot_interval (base pinned by chain.min_retain_blocks=0, so
	// routine pruning cannot move it) — same discriminator as the baseline.
	// WaitCaughtUp first so the freshly-restored earliest is observable.
	if err := sei.WaitCaughtUp(ctx, f.hc, f.node.TendermintRPC()); err != nil {
		t.Fatalf("follower %s not caught up after giga resync: %v", f.node.Name(), err)
	}
	postEarliest, ok := sei.EarliestHeight(ctx, f.hc, f.node.TendermintRPC())
	if !ok {
		t.Fatalf("read post-workflow earliest height of %q", f.node.Name())
	}
	if postEarliest-f.preEarliest < int64(f.interval) {
		t.Fatalf("follower %s earliest height %d -> %d after giga resync (jump %d), want a jump >= snapshot_interval %d (a genuine wipe + re-sync lands on a snapshot at least one interval higher; a smaller move is routine pruning, not a fresh restore)",
			f.node.Name(), f.preEarliest, postEarliest, postEarliest-f.preEarliest, f.interval)
	}

	// (d) Giga runtime: with evm-ss-split=true a node whose EVM-SS layout is
	// unpopulated refuses to boot (giga doc startup safety checks), so a healthy
	// Ready + caught-up + EVM-serving node IS the SDK-observable evidence giga went
	// live. Re-affirm Ready (the resync cycled seid), then EVM must serve — a hard
	// failure here, unlike the baseline where EVM is a soft t.Errorf, because EVM
	// serving under the split layout is this test's whole point.
	if err := f.node.WaitReady(ctx); err != nil {
		t.Fatalf("follower %s not Running after giga resync: %v", f.node.Name(), err)
	}
	if err := sei.WaitCaughtUp(ctx, f.hc, f.node.TendermintRPC()); err != nil {
		t.Fatalf("follower %s not caught up after giga resync: %v", f.node.Name(), err)
	}
	if err := sei.WaitEVMServing(ctx, f.hc, f.node.EVMRPC()); err != nil {
		t.Fatalf("follower %s EVM not serving under evm-ss-split=true after giga resync: %v (a node with an unpopulated EVM-SS layout refuses to boot, so a non-serving EVM here means giga did not go live)", f.node.Name(), err)
	}
	t.Logf("follower %s: caught up + EVM serving under evm-ss-split=true, block-store base jumped %d -> %d (>= interval %d) — TestGigaStoreMigration OK", f.node.Name(), f.preEarliest, postEarliest, f.interval)
}

// assertGigaConfigPatch is the anchor: it casts the workflow's raw CR, walks the
// compiled status.plan for the config-patch step, unmarshals its opaque Params
// JSON, and asserts the app.toml tree equals exactly the giga enabling flags for
// the given backend. This proves the ConfigMigration was rendered into the plan
// (spec = intent, status.plan = the applied TOML keys — the CRD's own contract),
// materializing the giga_store_migration.md Step-1 flags the controller pins.
func assertGigaConfigPatch(t *testing.T, wf *sei.Workflow, backend string) {
	t.Helper()

	obj, ok := wf.Object().(*v1alpha1.SeiNodeTaskWorkflow)
	if !ok || obj == nil {
		t.Fatalf("workflow Object() is %T, want *v1alpha1.SeiNodeTaskWorkflow", wf.Object())
	}
	if obj.Status.Plan == nil {
		t.Fatalf("workflow %s has no status.plan to inspect", wf.Name())
	}

	var patch *v1alpha1.PlannedTask
	for i := range obj.Status.Plan.Tasks {
		if obj.Status.Plan.Tasks[i].Type == taskTypeConfigPatch {
			patch = &obj.Status.Plan.Tasks[i]
			break
		}
	}
	if patch == nil {
		t.Fatalf("workflow %s plan has no %q task (the giga migration did not materialize a config-patch step)", wf.Name(), taskTypeConfigPatch)
	}
	if patch.Params == nil {
		t.Fatalf("config-patch task %s has nil Params", patch.ID)
	}

	// The planner serializes task.ConfigPatchTask{Files: ...} into Params, so the
	// payload is {"files":{"<file>":{"<section>":{...}}}}.
	var params struct {
		Files map[string]map[string]any `json:"files"`
	}
	if err := json.Unmarshal(patch.Params.Raw, &params); err != nil {
		t.Fatalf("unmarshal config-patch params %q: %v", string(patch.Params.Raw), err)
	}

	got := params.Files["app.toml"]
	want := map[string]any{
		"state-store": map[string]any{
			"ss-enable":    true,
			"evm-ss-split": true,
			"ss-backend":   backend,
		},
		"state-commit": map[string]any{
			"sc-enable": true,
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("workflow %s config-patch app.toml tree mismatch (-want +got):\n%s", wf.Name(), diff)
	}
	t.Logf("workflow %s: config-patch app.toml tree matches giga enabling flags (backend %s)", wf.Name(), backend)
}
