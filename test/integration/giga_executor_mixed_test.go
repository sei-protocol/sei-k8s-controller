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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/sei-protocol/sei-k8s-controller/internal/keygen"
	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

// rpcLivenessBlocks is the post-suite advance the follower must show to prove it is
// still producing/replaying blocks (not merely caught-up-then-frozen); small so
// the liveness gate resolves quickly on a healthy chain.
const rpcLivenessBlocks = 5

// keySnapshotInterval is the sei-config storage key controlling snapshot cadence;
// the follower zeroes it (a witness serves RPC trust points, not snapshots).
const keySnapshotInterval = "storage.snapshot_interval"

// TestNightlyGigaExecutorMixed is the CON-368 regression gate: it proves the seid
// execution engine is a per-node concern that stays consensus-compatible across the
// full release conformance suite. It provisions one 4-validator, snapshot-producing
// chain whose validators run the giga executor (the shipped default), brings up a
// single RPC follower flipped to the v2 executor — via the ConfigPatch task + a
// standard StateSync re-bootstrap — and runs the external release-test harness
// against that follower. The gate is: the v2 follower stays in consensus with the
// giga-executor validator set (caught up and advancing) throughout the suite.
//
// A regression that reintroduced the CON-368 executor divergence would make the v2
// follower compute a different result than the validators committed, halting it at
// the diverging block; the post-suite advance assertion would then fail. Storage
// layout is deliberately out of scope (all nodes run the shipped default) so the
// executor is the only variable under test.
//
// Inputs (env): SEI_CHAIN_ID, SEID_IMAGE, RELEASE_TEST_IMAGE [required];
// SEI_NAMESPACE [optional]. Run with -test.timeout 0.
func TestNightlyGigaExecutorMixed(t *testing.T) {
	requireCluster(t)
	chainID := runChainID(mustEnv(t, "SEI_CHAIN_ID"))
	seid := mustEnv(t, "SEID_IMAGE")
	releaseImage := mustEnv(t, "RELEASE_TEST_IMAGE")
	ns := envOr("SEI_NAMESPACE", "")
	runLabels := map[string]string{runLabelKey: chainID}

	// The re-bootstrap plus the release-test run (up to 60m per releaseJob's own
	// deadline) can overlap; size well above both, matching the sibling.
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Minute)
	defer cancel()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	c := openClient(ctx, t)
	cs := clientset(t)

	// 1. Provision a snapshot-producing 4-validator network (giga-executor default)
	//    with the release admin funded.
	env := provisionNetwork(ctx, t, c, cs, chainID, ns, seid, runLabels)

	// 2. Bring up one follower flipped to the v2 executor (ConfigPatch + re-bootstrap).
	v2Node := bringUpV2Follower(ctx, t, env)

	// 3. Run the release conformance suite against it.
	runReleaseSuite(ctx, t, env, releaseImage, v2Node)

	// 4. Assert the v2 follower stayed in consensus with the giga validators.
	assertRPCHealthy(ctx, t, env.hc, v2Node)
}

// executorMixedEnv is the shared bring-up context the helpers draw on: the SDK +
// client-go clients, the live network (for witness hostnames), the run identifiers
// + seid image, the poll client and snapshot cadence the resync witnesses are gated
// against, and the funded admin identity the release suite signs with.
type executorMixedEnv struct {
	c         *sei.Client
	cs        *kubernetes.Clientset
	ch        *chain
	net       *sei.Network
	chainID   string
	ns        string
	seid      string
	hc        *http.Client
	interval  int
	runLabels map[string]string
	admin     keygen.Identity
}

// provisionNetwork stands up the fixture: a funded release admin in genesis, a
// 4-validator snapshot-producing chain (no followers — the bring-up helper creates
// its own), advanced past one snapshot interval so the follower can state-sync from
// a snapshot that already exists. It registers teardown and returns the shared
// bring-up context.
func provisionNetwork(
	ctx context.Context, t *testing.T, c *sei.Client, cs *kubernetes.Clientset,
	chainID, ns, seid string, runLabels map[string]string,
) executorMixedEnv {
	t.Helper()

	admin, err := keygen.Derive()
	if err != nil {
		t.Fatalf("derive admin key: %v", err)
	}

	ch, err := provision(ctx, t, c, spec{
		chainID:       chainID,
		runID:         chainID,
		namespace:     ns,
		seidImage:     seid,
		validators:    4,
		rpcNodes:      0,
		storageConfig: mergeConfig(releaseBaseConfig, snapshotProductionConfig),
		accounts: []sei.GenesisAccount{
			{Address: admin.Address, Balance: releaseAdminBalance},
		},
	})
	cleanupChain(t, ch)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	// A follower can only state-sync from a snapshot that already exists; advance the
	// network one interval + margin so the re-bootstrap below has one to fetch.
	// Derive the delta from the config so retuning the interval cannot silently
	// under-wait.
	hc := &http.Client{Timeout: 10 * time.Second}
	interval, err := strconv.Atoi(snapshotProductionConfig[keySnapshotInterval])
	if err != nil {
		t.Fatalf("snapshot_interval not an int: %v", err)
	}
	if err := sei.WaitHeightAdvances(ctx, hc, ch.network.TendermintRPC(), int64(interval)+10); err != nil {
		t.Fatalf("network did not advance past the snapshot interval: %v", err)
	}
	t.Logf("network %s: ready (4 validators, giga-executor default, snapshot-producing, admin funded)", chainID)

	return executorMixedEnv{
		c: c, cs: cs, ch: ch, net: ch.network,
		chainID: chainID, ns: ns, seid: seid,
		hc: hc, interval: interval, runLabels: runLabels,
		admin: admin,
	}
}

// bringUpV2Follower creates a plain RPC follower (which boots with the giga executor
// by default) and flips it to the v2 executor: a ConfigPatch task disables the giga
// executor on disk, then a STANDARD StateSync re-bootstrap (Migration=nil — the
// plain wipe + re-sync) restarts seid so it re-reads the patched config and comes
// back in v2 mode. ConfigPatch only writes config (seid re-reads it on restart), and
// giga_executor.* is not re-asserted on the controller's own plans, so the patch
// survives the re-bootstrap. Returns the reconfigured node.
func bringUpV2Follower(ctx context.Context, t *testing.T, env executorMixedEnv) *sei.Node {
	t.Helper()
	node := bringUpPlainFollower(ctx, t, env, env.chainID+"-v2")

	// Patch the on-disk executor config to v2 (giga executor off). runTaskComplete
	// runs the task, registers its cleanup, and fails the suite if it does not
	// complete. This does NOT restart seid — the StateSync below is what applies it.
	runTaskComplete(ctx, t, env.c, sei.TaskSpec{
		Name:      "v2patch-" + env.chainID,
		Namespace: env.ns,
		Node:      node.Name(),
		Kind:      sei.TaskConfigPatch,
		Labels:    env.runLabels,
		ConfigPatch: &sei.ConfigPatch{Overrides: map[string]string{
			"giga_executor.enabled":     "false",
			"giga_executor.occ_enabled": "false",
		}},
	})
	t.Logf("v2 follower %s: config-patch applied (giga executor disabled)", node.Name())

	// Standard re-bootstrap: restarts seid onto the patched config, in v2 mode.
	witnesses := stateSyncWitnesses(t, env)
	assertWitnessFresh(ctx, t, env.hc, witnesses[0], witnesses[1], env.interval)
	wf, err := env.c.CreateWorkflow(ctx, sei.WorkflowSpec{
		Name:      "v2resync-" + env.chainID,
		Namespace: env.ns,
		Node:      node.Name(),
		Kind:      sei.WorkflowStateSync,
		Labels:    env.runLabels,
		StateSync: &sei.StateSyncWorkflow{Migration: nil, RpcServers: witnesses},
	})
	if err != nil {
		t.Fatalf("create v2 resync workflow: %v", err)
	}
	cleanupWorkflow(t, wf)
	t.Logf("workflow %s: created against %s (v2 re-bootstrap)", wf.Name(), node.Name())

	awaitWorkflowComplete(ctx, t, wf, workflowWaitTimeout)
	if err := node.WaitReady(ctx); err != nil {
		t.Fatalf("v2 follower %s not Running after re-bootstrap: %v", node.Name(), err)
	}
	if err := sei.WaitCaughtUp(ctx, env.hc, node.TendermintRPC()); err != nil {
		t.Fatalf("v2 follower %s not caught up after re-bootstrap: %v", node.Name(), err)
	}
	t.Logf("v2 follower %s: caught up in v2 executor mode", node.Name())
	return node
}

// bringUpPlainFollower creates a plain RPC follower on the network with the
// release-suite config and blocks until it is caught up + EVM-serving. It appends
// the node to the shared chain so cleanupChain reaps it, then returns it ready for
// an executor reconfiguration. The config mirrors the sibling's follower exactly
// (release base + rpc knobs, snapshot production off — a follower is a witness, not
// a snapshot source).
func bringUpPlainFollower(ctx context.Context, t *testing.T, env executorMixedEnv, name string) *sei.Node {
	t.Helper()
	node, err := createRPCNode(ctx, t, env.c, sei.NodeSpec{
		Name:      name,
		Network:   env.chainID,
		Namespace: env.ns,
		Image:     env.seid,
		Labels:    env.runLabels,
		Config: mergeConfig(
			mergeConfig(releaseBaseConfig, snapshotProductionConfig),
			mergeConfig(releaseRPCConfig, map[string]string{keySnapshotInterval: "0"}),
		),
	})
	if node != nil {
		env.ch.rpcNodes = append(env.ch.rpcNodes, node)
	}
	if err != nil {
		t.Fatalf("bring up plain follower %q: %v", name, err)
	}
	if err := gateRPCNode(ctx, t, env.hc, node); err != nil {
		t.Fatalf("plain follower %q not serving: %v", name, err)
	}
	return node
}

// stateSyncWitnesses returns two DISTINCT genesis-validator RPC endpoints
// (validator-0 and validator-1) as light-client witnesses for the follower resync.
// A plain follower carries no resolved state-syncers, so the StateSync workflow must
// pass explicit witnesses; two live validators are always at head and decoupled from
// the follower being reconfigured. Bare host:port, the CRD SnapshotSource.RpcServers
// shape.
func stateSyncWitnesses(t *testing.T, env executorMixedEnv) []string {
	t.Helper()
	witnessNS := witnessNamespace(t, env.net)
	return []string{
		nodeRPC(fmt.Sprintf("%s-0", env.chainID), witnessNS), // genesis validator-0
		nodeRPC(fmt.Sprintf("%s-1", env.chainID), witnessNS), // genesis validator-1
	}
}

// runReleaseSuite launches the release-test Job against the v2 follower, signing
// with the funded admin, and blocks until it completes. The Job name and endpoints
// are logged prominently so a failed run is debuggable from the harness log; the
// suite fails if the run does not complete.
func runReleaseSuite(ctx context.Context, t *testing.T, env executorMixedEnv, releaseImage string, node *sei.Node) {
	t.Helper()
	cs := env.cs
	ns := env.net.Namespace()

	rest := node.REST()
	if rest == "" {
		t.Fatalf("v2 follower %q has no REST endpoint (release-test needs SEI_REST_ENDPOINT)", node.Name())
	}
	if err := sei.WaitRESTServing(ctx, env.hc, rest); err != nil {
		t.Fatalf("v2 follower %q REST serving: %v", node.Name(), err)
	}
	t.Logf("v2 follower %s: REST serving at %s", node.Name(), rest)

	secretName := "admin-" + env.chainID
	createMnemonicSecret(ctx, t, cs, ns, secretName, env.runLabels, env.admin.Mnemonic)

	job := releaseJob(releaseParams{
		name:       "release-test-" + env.chainID,
		namespace:  ns,
		image:      releaseImage,
		runID:      env.chainID,
		chainID:    env.chainID,
		adminAddr:  env.admin.Address,
		secretName: secretName,
		tmRPC:      node.TendermintRPC(),
		evmRPC:     node.EVMRPC(),
		rest:       rest,
	})
	if _, err := cs.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create release-test job: %v", err)
	}
	t.Cleanup(func() {
		delCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		bg := metav1.DeletePropagationBackground
		_ = cs.BatchV1().Jobs(ns).Delete(delCtx, job.Name, metav1.DeleteOptions{PropagationPolicy: &bg})
	})
	t.Logf("release-test job launched: %s (image %s)", job.Name, releaseImage)

	waitJob(ctx, t, cs, ns, job.Name)
	t.Logf("release-test PASSED against the v2 executor follower")
}

// assertRPCHealthy re-affirms the follower survived the release run: still caught up
// AND advancing (not caught-up-then-frozen). Errors are collected per-follower
// (t.Errorf, not Fatalf) so every node is always evaluated.
func assertRPCHealthy(ctx context.Context, t *testing.T, hc *http.Client, nodes ...*sei.Node) {
	t.Helper()
	for _, n := range nodes {
		if err := sei.WaitCaughtUp(ctx, hc, n.TendermintRPC()); err != nil {
			t.Errorf("post-release follower %s not caught up: %v", n.Name(), err)
			continue
		}
		if err := sei.WaitHeightAdvances(ctx, hc, n.TendermintRPC(), rpcLivenessBlocks); err != nil {
			t.Errorf("post-release follower %s height not advancing (frozen?): %v", n.Name(), err)
			continue
		}
		t.Logf("post-release follower %s: caught up and advancing", n.Name())
	}
}
