//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"os/signal"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/sei-protocol/sei-k8s-controller/internal/keygen"
	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

// TestNightlyGigaMixedRelease proves giga_store_migration.md's central safety
// claim — "Giga SS Store is a per-node SS change that is invisible to the
// network" — under real conformance load rather than a healthy-boot check. It
// provisions one 4-validator, snapshot-producing chain with two plain RPC
// followers, migrates ONE follower to giga (evm-ss-split=true, pebbledb) via the
// same StateSync recipe TestNightlyGigaStoreMigration drives, and leaves the other
// at the shipped default (the v2 control). It then runs the external release-test
// conformance harness against the chain and asserts the invariant:
//
//	a mixed giga/v2 follower set is invisible to consensus — through real
//	transaction load the chain keeps producing blocks and BOTH followers stay at
//	the validator head (a follower that halted or diverged under load would fall
//	behind, so head-parity is the SDK-observable proof of no divergence), and the
//	migrated giga node comes up and serves EVM under evm-ss-split.
//
// Why ONE conformance run, not two: the harness runs against the v2 follower with
// exclusive chain access. Its stateful sequences (EVM filters, send-then-wait)
// assume a single consistent mempool + filter-store view — the same reason
// TestNightlyRelease runs exactly one node. Two concurrent runs (one per follower)
// would cross-talk on GLOBAL block state (the EIP-1559 base fee is per-block,
// driven by every tx in the block, not per-account) and on inclusion timing, which
// no separate-admin isolation prevents; per-node SS read+serve parity is already
// covered by TestNightlyGigaStoreMigration. This suite's distinct job is the
// mixed-fleet-under-load consensus invariant, which needs only one traffic source.
// The harness targets the v2 control follower — the full-history, shipped-default
// node — while the migrated giga follower is exercised by the head-parity check.
//
// Inputs (env): SEI_CHAIN_ID, SEID_IMAGE, RELEASE_TEST_IMAGE [required];
// SEI_NAMESPACE [optional]. Run with -test.timeout 0.
func TestNightlyGigaMixedRelease(t *testing.T) {
	requireCluster(t)
	chainID := runChainID(mustEnv(t, "SEI_CHAIN_ID"))
	seid := mustEnv(t, "SEID_IMAGE")
	releaseImage := mustEnv(t, "RELEASE_TEST_IMAGE")
	ns := envOr("SEI_NAMESPACE", "")
	runLabels := map[string]string{runLabelKey: chainID}

	// Sequential critical path: provision + giga migration + one conformance run.
	// TestNightlyRelease budgets 80m for provision + a single 60m release Job; this
	// suite adds a ~15m giga migration ahead of the same run, so size ~30m above it.
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Minute)
	defer cancel()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	c := openClient(ctx, t)
	cs := clientset(t)
	hc := &http.Client{Timeout: 10 * time.Second}

	// One funded admin: the single conformance run (against the v2 follower) signs
	// with it. The giga follower carries no load of its own here.
	admin, err := keygen.Derive()
	if err != nil {
		t.Fatalf("derive admin key: %v", err)
	}

	// Vesting fixture for the solo-precompile locked-balance test (see
	// vestingFixture* in release_test.go); solo.spec.ts runs in this suite too.
	vesting, err := keygen.Derive()
	if err != nil {
		t.Fatalf("derive vesting key: %v", err)
	}

	// 1. Provision one chain with two plain RPC followers. rpcNodes[0] stays at the
	//    shipped default (the v2 control); rpcNodes[1] is migrated to giga below.
	//    Validators are snapshot-producing so the migration can state-sync from a
	//    snapshot; the followers are non-producing witnesses (interval 0).
	ch, err := provision(ctx, t, c, spec{
		chainID:       chainID,
		runID:         chainID,
		namespace:     ns,
		seidImage:     seid,
		validators:    4,
		rpcNodes:      2,
		storageConfig: mergeConfig(releaseBaseConfig, snapshotProductionConfig),
		rpcConfig:     mergeConfig(releaseRPCConfig, map[string]string{"storage.snapshot_interval": "0"}),
		accounts: []sei.GenesisAccount{
			{Address: admin.Address, Balance: releaseAdminBalance},
			{Address: vesting.Address, Balance: vestingFixtureBalance, Vesting: &sei.GenesisAccountVesting{
				Amount:  vestingFixtureLocked,
				EndTime: vestingFixtureEndTime,
			}},
		},
	})
	cleanupChain(t, ch)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	v2Node := ch.rpcNodes[0]
	gigaNode := ch.rpcNodes[1]
	t.Logf("network %s: ready (4 validators, 2 rpc followers, admin %s funded)", chainID, admin.Address)

	// The migration state-syncs from a snapshot; step 5 reuses the snapshot interval
	// as the follower head-lag tolerance (the same freshness yardstick assertWitnessFresh uses).
	interval, err := strconv.Atoi(snapshotProductionConfig["storage.snapshot_interval"])
	if err != nil {
		t.Fatalf("snapshot_interval not an int: %v", err)
	}

	// 2. Migrate rpcNodes[1] onto the giga SS store in place, witnessed by genesis
	//    validator-0 and the untouched v2 follower.
	migrateFollowerToGiga(ctx, t, c, hc, ch.network, gigaNode, v2Node, interval, runLabels)

	// 3. Confirm the migrated giga follower is live under evm-ss-split, and the v2
	//    control is still healthy, before handing the chain any load.
	assertGigaFollowerServing(ctx, t, hc, gigaNode)
	if err := sei.WaitCaughtUp(ctx, hc, v2Node.TendermintRPC()); err != nil {
		t.Fatalf("v2 control follower %s not caught up before load: %v", v2Node.Name(), err)
	}
	t.Logf("v2 control follower %s: caught up, untouched by the migration", v2Node.Name())

	// Baseline the validator head so step 5 can prove the chain advanced under load
	// (catching_up is a one-way latch and cannot certify liveness — see assertWitnessFresh).
	preHead, ok := sei.LatestHeight(ctx, hc, ch.network.TendermintRPC())
	if !ok {
		t.Fatalf("read validator head before load")
	}

	// 4. Run the conformance harness once, against the v2 follower, with exclusive
	//    chain access. Its exit code is the functional verdict.
	runReleaseTest(ctx, t, cs, releaseRunParams{
		hc:        hc,
		net:       ch.network,
		node:      v2Node,
		admin:     admin,
		vesting:   vesting,
		image:     releaseImage,
		runLabels: runLabels,
		label:     "v2",
	})
	t.Logf("release-test PASSED against the v2 follower")

	// 5. The invariant: through the conformance load the chain kept producing blocks
	//    and BOTH followers stayed at the validator head — the giga migration was
	//    invisible to consensus. Head-parity, not catching_up: the latch already
	//    flipped in steps 1-3, so it would false-green a post-load halt or a giga
	//    follower silently falling behind (the one failure mode this suite exists to catch).
	postHead, ok := sei.LatestHeight(ctx, hc, ch.network.TendermintRPC())
	if !ok {
		t.Fatalf("read validator head after load")
	}
	if postHead <= preHead {
		t.Fatalf("validator head did not advance through the conformance load: %d -> %d (the chain stalled under real traffic)", preHead, postHead)
	}
	assertFollowerAtHead(ctx, t, hc, gigaNode, "giga", postHead, interval)
	assertFollowerAtHead(ctx, t, hc, v2Node, "v2 control", postHead, interval)
	t.Logf("chain advanced %d -> %d and both followers tracked head through conformance load — mixed storage invisible to the network, TestNightlyGigaMixedRelease OK", preHead, postHead)
}

// migrateFollowerToGiga re-bootstraps gigaNode in place onto the giga SS store
// (evm-ss-split=true, pebbledb) via the StateSync + ConfigMigration recipe,
// witnessed by genesis validator-0 and the untouched v2 follower. In order it:
// advances the chain past one snapshot interval (a follower can only state-sync
// from a snapshot that already exists), asserts the witnesses are fresh enough to
// serve that snapshot, drives the workflow to Complete, and anchors on the compiled
// plan carrying exactly the giga app.toml flags. It returns once the migration has
// materialized; the caller gates the node's runtime health separately
// (assertGigaFollowerServing).
func migrateFollowerToGiga(
	ctx context.Context, t *testing.T, c *sei.Client, hc *http.Client,
	net *sei.Network, gigaNode, v2Witness *sei.Node, interval int, runLabels map[string]string,
) {
	t.Helper()
	chainID := net.Name()
	ns := net.Namespace()

	if err := sei.WaitHeightAdvances(ctx, hc, net.TendermintRPC(), int64(interval)+10); err != nil {
		t.Fatalf("network did not advance past the snapshot interval: %v", err)
	}

	// Witnesses are explicit (validator-0 + the v2 control): a plain follower's
	// ResolvedStateSyncers is empty — neither follower was created with a StateSync
	// spec — and a witness serves RPC trust points only, so its own storage layout
	// is not consensus-relevant to serving them. That makes the v2 node a valid
	// witness for the giga target's migration.
	witnessNS := witnessNamespace(t, net)
	witnesses := []string{
		nodeRPC(fmt.Sprintf("%s-0", chainID), witnessNS), // genesis validator-0
		nodeRPC(v2Witness.Name(), witnessNS),             // the v2 control follower
	}
	assertWitnessFresh(ctx, t, hc, witnesses[0], witnesses[1], interval)

	wf, err := c.CreateWorkflow(ctx, sei.WorkflowSpec{
		Name:      "giga-" + chainID,
		Namespace: ns,
		Node:      gigaNode.Name(),
		Kind:      sei.WorkflowStateSync,
		Labels:    runLabels,
		StateSync: &sei.StateSyncWorkflow{
			Migration: &sei.ConfigMigration{
				Kind:      sei.ConfigMigrationGigaStore,
				GigaStore: &sei.GigaStoreMigration{Backend: "pebbledb"},
			},
			RpcServers: witnesses,
		},
	})
	if err != nil {
		t.Fatalf("create giga workflow: %v", err)
	}
	cleanupWorkflow(t, wf)
	t.Logf("workflow %s: created against %s (giga, backend pebbledb)", wf.Name(), gigaNode.Name())

	awaitWorkflowComplete(ctx, t, wf, workflowWaitTimeout)
	t.Logf("workflow %s: complete", wf.Name())

	// Anchor: the compiled plan carries exactly the giga app.toml flags — proof the
	// ConfigMigration materialized end to end (inspects the persisted status.plan,
	// not a log line, so it cannot false-green).
	assertGigaConfigPatch(t, wf, "pebbledb")
}

// assertGigaFollowerServing gates the migrated follower's runtime health: Ready
// (the resync cycled seid), caught up, then EVM serving under evm-ss-split. Per the
// giga migration doc's startup safety checks, a node with evm-ss-split=true and an
// unpopulated EVM-SS layout refuses to boot, so a served EVM endpoint IS the
// SDK-observable evidence that giga went live.
func assertGigaFollowerServing(ctx context.Context, t *testing.T, hc *http.Client, gigaNode *sei.Node) {
	t.Helper()
	if err := gigaNode.WaitReady(ctx); err != nil {
		t.Fatalf("giga follower %s not Running after migration: %v", gigaNode.Name(), err)
	}
	if err := sei.WaitCaughtUp(ctx, hc, gigaNode.TendermintRPC()); err != nil {
		t.Fatalf("giga follower %s not caught up after migration: %v", gigaNode.Name(), err)
	}
	if err := sei.WaitEVMServing(ctx, hc, gigaNode.EVMRPC()); err != nil {
		t.Fatalf("giga follower %s EVM not serving under evm-ss-split=true after migration: %v (a node with an unpopulated EVM-SS layout refuses to boot, so a non-serving EVM here means giga did not go live)", gigaNode.Name(), err)
	}
	t.Logf("giga follower %s: caught up + EVM serving under evm-ss-split=true", gigaNode.Name())
}

// assertFollowerAtHead fails the suite unless the follower is within maxLag blocks
// of the given validator head — proof it tracked consensus rather than halting or
// diverging under load. catching_up is a one-way latch (see assertWitnessFresh) and
// cannot certify this, so freshness is asserted directly on heights, the same way
// assertWitnessFresh gates a state-sync witness. role tags the message.
func assertFollowerAtHead(ctx context.Context, t *testing.T, hc *http.Client, node *sei.Node, role string, head int64, maxLag int) {
	t.Helper()
	h, ok := sei.LatestHeight(ctx, hc, node.TendermintRPC())
	if !ok {
		t.Fatalf("%s follower %s: read height after load", role, node.Name())
	}
	if gap := head - h; gap > int64(maxLag) {
		t.Fatalf("%s follower %s at height %d lags validator head %d by %d blocks (> %d) after conformance load: a follower that fell behind or diverged under real traffic is not invisible to the network", role, node.Name(), h, head, gap, maxLag)
	}
	t.Logf("%s follower %s: at height %d, validator head %d after load", role, node.Name(), h, head)
}
