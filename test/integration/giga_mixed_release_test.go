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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/sei-protocol/sei-k8s-controller/internal/keygen"
	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

// TestNightlyGigaMixedRelease proves giga_store_migration.md's central safety claim —
// "Giga SS Store is a per-node SS change that is invisible to the network" —
// under real conformance load, not just a healthy-boot check. It provisions one
// 4-validator chain with two plain RPC followers, migrates ONE follower to giga
// (evm-ss-split=true) via the same StateSync recipe TestNightlyGigaStoreMigration
// drives, leaves the other at the shipped default (every node ships with
// ss-enable=true already; evm-ss-split=false is the only thing distinguishing a
// "giga" node from a plain one), then runs the external release-test conformance
// harness against BOTH followers concurrently, each signing with its own funded
// admin account so the two runs never race a shared account's tx sequence.
// Parity is: both runs pass identically — the same 749-case suite, the same exit
// code — regardless of which storage layout answered the queries.
//
// Honest ceiling: the release-test image is external and opaque to this suite
// (no source here); if any of its cases assert something about GLOBAL chain
// state (e.g. total supply) rather than state scoped to their own admin account,
// running two instances concurrently against the same chain could cross-talk in
// a way neither run's own log would explain. Reviewed and accepted for the
// nightly run; if cross-talk is ever observed, the fallback is running the two
// harness Jobs sequentially instead of concurrently (accept the added wall-clock
// for a cleaner isolation guarantee).
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

	// The giga migration workflow and two release-test harness runs (each up to
	// 60m per releaseJob's own deadline) can overlap; size well above both.
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Minute)
	defer cancel()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	c := openClient(ctx, t)
	cs := clientset(t)
	hc := &http.Client{Timeout: 10 * time.Second}

	// Two independent admin accounts, one per target node, so the two concurrent
	// harness runs never race a shared account's tx sequence.
	adminV2, err := keygen.Derive()
	if err != nil {
		t.Fatalf("derive v2 admin key: %v", err)
	}
	adminGiga, err := keygen.Derive()
	if err != nil {
		t.Fatalf("derive giga admin key: %v", err)
	}

	// Provision: 4 validators, snapshot-producing (the migration below
	// state-syncs rpcNodes[1] from a network snapshot); 2 plain RPC followers,
	// non-producing (rpcConfig zeroes the interval — a follower is a witness,
	// not a snapshot source). rpcNodes[0] stays at the shipped default (the v2
	// control); rpcNodes[1] is migrated to giga below.
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
			{Address: adminV2.Address, Balance: releaseAdminBalance},
			{Address: adminGiga.Address, Balance: releaseAdminBalance},
		},
	})
	cleanupChain(t, ch)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	net := ch.network
	v2Node := ch.rpcNodes[0]
	gigaNode := ch.rpcNodes[1]
	t.Logf("network %s: ready (4 validators, 2 rpc followers, 2 admins funded)", chainID)

	// A follower can only state-sync from a snapshot that already exists.
	// Advance the network one interval + margin so the migration below has one
	// to fetch (the same wait bringUpStateSyncFollower uses).
	interval, err := strconv.Atoi(snapshotProductionConfig["storage.snapshot_interval"])
	if err != nil {
		t.Fatalf("snapshot_interval not an int: %v", err)
	}
	if err := sei.WaitHeightAdvances(ctx, hc, net.TendermintRPC(), int64(interval)+10); err != nil {
		t.Fatalf("network did not advance past the snapshot interval: %v", err)
	}

	// Namespace for witness service hostnames, derived from the aggregate RPC
	// host the same way TestNightlyWorkflowStateSync does (ns may be "").
	u, err := url.Parse(net.TendermintRPC())
	if err != nil {
		t.Fatalf("parse network TM RPC %q: %v", net.TendermintRPC(), err)
	}
	hostLabels := strings.Split(u.Hostname(), ".")
	if len(hostLabels) < 2 || hostLabels[1] == "" {
		t.Fatalf("cannot derive namespace from aggregate RPC host %q", u.Host)
	}
	witnessNS := hostLabels[1]

	// Migrate rpcNodes[1] to giga. Witnesses are explicit (validator-0 + the v2
	// control node): a plain follower's ResolvedStateSyncers is populated only
	// for nodes created with an explicit StateSync spec, which neither follower
	// has here. A witness serves RPC trust points only; its own storage layout
	// is not consensus-relevant to serving them, so the v2 node is a valid
	// witness for the giga target's migration.
	//
	// Freshness gate: a witness must be at head to serve light blocks at the
	// snapshot height the migration cross-checks; catching_up cannot certify that
	// (a one-way latch), so assert it directly — same gate
	// bringUpStateSyncFollower applies before its own bootstrap.
	vHeight, vOK := sei.LatestHeight(ctx, hc, "http://"+nodeRPC(fmt.Sprintf("%s-0", chainID), witnessNS))
	wHeight, wOK := sei.LatestHeight(ctx, hc, "http://"+nodeRPC(v2Node.Name(), witnessNS))
	if !vOK || !wOK {
		t.Fatalf("witness freshness read: validator ok=%v, v2 follower ok=%v", vOK, wOK)
	}
	if gap := vHeight - wHeight; gap > int64(interval) {
		t.Fatalf("witness %s lags validator head by %d blocks (> interval %d): a diverging follower cannot serve state-sync light blocks", v2Node.Name(), gap, interval)
	}

	witnesses := []string{
		nodeRPC(fmt.Sprintf("%s-0", chainID), witnessNS),
		nodeRPC(v2Node.Name(), witnessNS),
	}
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
	assertGigaConfigPatch(t, wf, "pebbledb")

	if err := gigaNode.WaitReady(ctx); err != nil {
		t.Fatalf("giga node %s not Running after migration: %v", gigaNode.Name(), err)
	}
	if err := sei.WaitCaughtUp(ctx, hc, gigaNode.TendermintRPC()); err != nil {
		t.Fatalf("giga node %s not caught up after migration: %v", gigaNode.Name(), err)
	}
	if err := sei.WaitEVMServing(ctx, hc, gigaNode.EVMRPC()); err != nil {
		t.Fatalf("giga node %s EVM not serving under evm-ss-split=true after migration: %v (a node with an unpopulated EVM-SS layout refuses to boot, so a non-serving EVM here means giga did not go live)", gigaNode.Name(), err)
	}
	t.Logf("giga node %s: caught up + EVM serving under evm-ss-split=true", gigaNode.Name())

	// The v2 control node was never touched by the migration; confirm it is
	// still healthy before handing both nodes to the conformance harness.
	if err := sei.WaitCaughtUp(ctx, hc, v2Node.TendermintRPC()); err != nil {
		t.Fatalf("v2 control node %s not caught up: %v", v2Node.Name(), err)
	}
	t.Logf("v2 control node %s: still caught up, untouched by the migration", v2Node.Name())

	// Launch one release-test Job per node. Job/Secret creation and t.Cleanup
	// registration happen here, on the test's own goroutine — only the
	// wait-and-log step below runs concurrently, and it never touches t.Fatal/
	// t.Cleanup (unsafe outside the test's own goroutine), only t.Logf (documented
	// safe for concurrent use) and plain error returns.
	targets := []struct {
		label string
		node  *sei.Node
		admin keygen.Identity
	}{
		{"v2", v2Node, adminV2},
		{"giga", gigaNode, adminGiga},
	}
	jobNames := make([]string, len(targets))
	for i, tgt := range targets {
		rest := tgt.node.REST()
		if rest == "" {
			t.Fatalf("%s node %q exposes no REST endpoint (release-test needs SEI_REST_ENDPOINT)", tgt.label, tgt.node.Name())
		}
		if err := sei.WaitRESTServing(ctx, hc, rest); err != nil {
			t.Fatalf("%s node %q REST serving: %v", tgt.label, tgt.node.Name(), err)
		}
		t.Logf("%s node %s: REST serving at %s", tgt.label, tgt.node.Name(), rest)

		secretName := "admin-" + tgt.label + "-" + chainID
		createMnemonicSecret(ctx, t, cs, net.Namespace(), secretName, runLabels, tgt.admin.Mnemonic)

		job := releaseJob(releaseParams{
			name:       "release-test-" + tgt.label + "-" + chainID,
			namespace:  net.Namespace(),
			image:      releaseImage,
			runID:      chainID,
			chainID:    chainID,
			adminAddr:  tgt.admin.Address,
			secretName: secretName,
			tmRPC:      tgt.node.TendermintRPC(),
			evmRPC:     tgt.node.EVMRPC(),
			rest:       rest,
		})
		if _, err := cs.BatchV1().Jobs(net.Namespace()).Create(ctx, job, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create %s release-test job: %v", tgt.label, err)
		}
		jobNames[i] = job.Name
		t.Cleanup(func(name string) func() {
			return func() {
				delCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				bg := metav1.DeletePropagationBackground
				_ = cs.BatchV1().Jobs(net.Namespace()).Delete(delCtx, name, metav1.DeleteOptions{PropagationPolicy: &bg})
			}
		}(job.Name))
		t.Logf("%s release-test job launched (%s)", tgt.label, releaseImage)
	}

	type result struct {
		label string
		err   error
	}
	results := make(chan result, len(targets))
	for i, tgt := range targets {
		go func(label, ns, jobName string) {
			results <- result{label, waitJobErr(ctx, cs, ns, jobName)}
		}(tgt.label, net.Namespace(), jobNames[i])
	}

	var failed []string
	for range targets {
		r := <-results
		if r.err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", r.label, r.err))
			continue
		}
		t.Logf("%s release-test job completed", r.label)
	}
	if len(failed) > 0 {
		t.Fatalf("release-test failed: %s", strings.Join(failed, "; "))
	}

	// Both nodes stayed live through the concurrent harness runs.
	if err := sei.WaitCaughtUp(ctx, hc, v2Node.TendermintRPC()); err != nil {
		t.Errorf("post-release v2 node %s not caught up: %v", v2Node.Name(), err)
	}
	if err := sei.WaitCaughtUp(ctx, hc, gigaNode.TendermintRPC()); err != nil {
		t.Errorf("post-release giga node %s not caught up: %v", gigaNode.Name(), err)
	}
	t.Logf("release-test PASSED against both v2 and giga followers — parity confirmed, TestNightlyGigaMixedRelease OK")
}

// waitJobErr mirrors waitJob's polling logic but returns an error instead of
// calling t.Fatalf, so it is safe to run from a goroutine other than the one
// running the test (T.FailNow — which Fatalf calls — must only be called from
// the test's own goroutine; log methods and plain returns carry no such
// restriction).
func waitJobErr(ctx context.Context, cs *kubernetes.Clientset, ns, name string) error {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		job, err := cs.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get job %q: %w", name, err)
		}
		for _, cond := range job.Status.Conditions {
			if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
				return nil
			}
			if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
				// Fresh, short-lived context for the log read: ctx may be near its
				// own deadline here, which would truncate the read that explains why.
				logCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				tail := podLogTail(logCtx, cs, ns, name)
				cancel()
				return fmt.Errorf("job %q failed: %s\n--- pod log (tail) ---\n%s", name, cond.Message, tail)
			}
		}
		select {
		case <-ctx.Done():
			logCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			tail := podLogTail(logCtx, cs, ns, name)
			cancel()
			return fmt.Errorf("job %q did not finish before deadline: %w\n--- pod log (tail) ---\n%s", name, ctx.Err(), tail)
		case <-tick.C:
		}
	}
}
