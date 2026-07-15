//go:build integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/sei-protocol/sei-k8s-controller/internal/keygen"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
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
// execution engine is a per-node concern that stays consensus-compatible with a
// giga-executor validator set ACROSS THE RELEASE CONFORMANCE WORKLOAD. It provisions
// one 4-validator, snapshot-producing chain whose validators run the giga executor
// (the shipped default), brings up a single RPC follower flipped to the v2 executor
// — via the ConfigPatch task + a standard StateSync re-bootstrap, then VERIFIES on
// disk that the flip actually took (bringUpV2Follower asserts [giga_executor] is off,
// so a silently-dropped patch cannot make the gate vacuous) — and runs the external
// release-test harness against that follower. The gate is: the v2 follower stays in
// consensus with the giga-executor validators (caught up and advancing) throughout
// the suite.
//
// Scope of the claim: this proves "no v2/giga divergence ON THE RELEASE WORKLOAD,"
// not "v2 ≡ giga" in general. The gate only fires if the suite's tx mix actually
// exercises a diverging code path; a divergence confined to a tx type the conformance
// suite never emits would slip through. Un-defer trigger: if a v2/giga divergence
// ever recurs in a tx type this suite does not exercise, add targeted diverging-tx
// vectors here rather than trusting the conformance workload's coverage.
//
// A regression that reintroduced a CON-368-class divergence on a tx the suite emits
// would make the v2 follower compute a different result than the validators
// committed, halting it at the diverging block; the post-suite advance assertion
// would then fail (and attributeStall separates that consensus halt from an unrelated
// infra crash). Storage layout is deliberately out of scope (all nodes run the
// shipped default) so the executor is the only variable under test.
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
	assertRPCHealthy(ctx, t, env.cs, env.hc, v2Node)
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

	// VERIFY the variable-under-test: giga is the shipped default AND the validators
	// run giga, so "caught up" alone cannot prove the follower is on v2 — a silently
	// dropped ConfigPatch would leave it on giga, in consensus, and the gate would
	// pass while testing nothing. Read app.toml off disk and assert giga is off.
	assertFollowerV2(ctx, t, env.cs, node)
	t.Logf("v2 follower %s: caught up in v2 executor mode", node.Name())
	return node
}

// assertFollowerV2 reads app.toml off the follower's seid container and asserts the
// executor actually flipped to v2 (giga_executor off). This is the positive check
// that closes the vacuous-pass hole: the flip is VERIFIED on disk, not inferred from
// the follower merely staying in consensus with a giga validator set.
func assertFollowerV2(ctx context.Context, t *testing.T, cs *kubernetes.Clientset, node *sei.Node) {
	t.Helper()
	// A SeiNode's StatefulSet is replicas=1, so its sole pod is <node>-0.
	pod := node.Name() + "-0"
	appTomlPath := platform.DataDir + "/config/app.toml"
	out, err := execInPod(ctx, t, cs, node.Namespace(), pod, containerSeid, "cat", appTomlPath)
	if err != nil {
		t.Fatalf("read %s on follower %s: %v", appTomlPath, node.Name(), err)
	}
	var cfg struct {
		GigaExecutor struct {
			Enabled    bool `toml:"enabled"`
			OCCEnabled bool `toml:"occ_enabled"`
		} `toml:"giga_executor"`
	}
	if _, err := toml.Decode(out, &cfg); err != nil {
		t.Fatalf("parse app.toml from follower %s: %v\n--- app.toml ---\n%s", node.Name(), err, out)
	}
	if cfg.GigaExecutor.Enabled || cfg.GigaExecutor.OCCEnabled {
		t.Fatalf("follower %s still running giga after ConfigPatch+re-bootstrap "+
			"([giga_executor] enabled=%t occ_enabled=%t) — the executor flip did not survive; "+
			"the gate would be vacuous", node.Name(), cfg.GigaExecutor.Enabled, cfg.GigaExecutor.OCCEnabled)
	}
	t.Logf("follower %s: verified v2 on disk ([giga_executor] enabled=false, occ_enabled=false)", node.Name())
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
// (t.Errorf, not Fatalf) so every node is always evaluated. A stall failure carries
// attributeStall's root-cause read so a red nightly is actionable at a glance —
// divergence (the thing under test) vs an infra crash (a known flake source).
func assertRPCHealthy(
	ctx context.Context, t *testing.T, cs *kubernetes.Clientset, hc *http.Client, nodes ...*sei.Node,
) {
	t.Helper()
	for _, n := range nodes {
		if err := sei.WaitCaughtUp(ctx, hc, n.TendermintRPC()); err != nil {
			t.Errorf("post-release follower %s not caught up: %v\n%s", n.Name(), err, attributeStall(cs, n))
			continue
		}
		if err := sei.WaitHeightAdvances(ctx, hc, n.TendermintRPC(), rpcLivenessBlocks); err != nil {
			t.Errorf("post-release follower %s height not advancing (frozen?): %v\n%s", n.Name(), err, attributeStall(cs, n))
			continue
		}
		t.Logf("post-release follower %s: caught up and advancing", n.Name())
	}
}

// containerSeid is the seid container name in a SeiNode pod
// (noderesource.containerNameSeid); the exec/log reads below target it directly.
const containerSeid = "seid"

// consensusMismatchRe broadly matches the app-hash / results-hash / consensus-failure
// log lines a genuine v2/giga divergence emits, WITHOUT pinning one exact
// sei-tendermint literal (the phrasing drifts across releases). A match attributes a
// stall to divergence; its absence — with a clean restart count — points at infra.
var consensusMismatchRe = regexp.MustCompile(
	`(?i)wrong app hash|app\s*hash mismatch|apphash|lastresultshash|last results hash|consensus failure`)

// attributeStall turns an opaque "not advancing" into an actionable root cause. These
// nightlies flake on infra races, so a genuine consensus halt (a divergence — the
// variable under test) and an unrelated OOM/crash otherwise produce the IDENTICAL
// "not advancing" red. It reports the seid container's restart count + last
// termination reason (calling out OOMKilled explicitly as infra death, not a
// divergence), then tails the seid log and surfaces any consensus-mismatch lines.
// Best-effort and on a FRESH context (the suite ctx may already be drained by the
// WaitHeightAdvances budget): a failed diagnostic read must never mask the stall.
func attributeStall(cs *kubernetes.Clientset, node *sei.Node) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ns, pod := node.Namespace(), node.Name()+"-0"

	var b strings.Builder
	b.WriteString("--- stall attribution ---\n")

	p, err := cs.CoreV1().Pods(ns).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		fmt.Fprintf(&b, "(pod %s/%s status unavailable: %v)\n", ns, pod, err)
	} else {
		for _, st := range p.Status.ContainerStatuses {
			if st.Name != containerSeid {
				continue
			}
			fmt.Fprintf(&b, "seid restartCount=%d ready=%t\n", st.RestartCount, st.Ready)
			if term := st.LastTerminationState.Terminated; term != nil {
				fmt.Fprintf(&b, "seid last termination: reason=%q exitCode=%d\n", term.Reason, term.ExitCode)
				if term.Reason == "OOMKilled" {
					b.WriteString("=> OOMKilled: INFRA death (memory pressure), NOT an executor divergence\n")
				}
			}
		}
	}

	tail := podContainerLogTail(ctx, cs, ns, pod, containerSeid, 200)
	if hits := matchingLines(tail, consensusMismatchRe); len(hits) > 0 {
		b.WriteString("=> consensus-mismatch pattern in seid log (points at v2/giga DIVERGENCE):\n")
		for _, line := range hits {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	} else {
		b.WriteString("(no consensus-mismatch pattern in seid log tail; with a clean restart count " +
			"this reads as an infra stall, not a divergence)\n")
		fmt.Fprintf(&b, "--- seid log (tail) ---\n%s\n", tail)
	}
	return b.String()
}

// matchingLines returns the lines of text that match re, preserving order.
func matchingLines(text string, re *regexp.Regexp) []string {
	var out []string
	for line := range strings.SplitSeq(text, "\n") {
		if re.MatchString(line) {
			out = append(out, line)
		}
	}
	return out
}

// podContainerLogTail returns the tail of a named container's log in a pod,
// best-effort — the diagnostic signal a stall error alone cannot carry.
func podContainerLogTail(ctx context.Context, cs *kubernetes.Clientset, ns, pod, container string, lines int64) string {
	raw, err := cs.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{
		Container: container,
		TailLines: &lines,
	}).DoRaw(ctx)
	if err != nil {
		return fmt.Sprintf("(read logs %s/%s[%s] failed: %v)", ns, pod, container, err)
	}
	return string(raw)
}

// execInPod runs argv (no shell) in a pod container and returns its stdout. This is
// the harness's one reach-into-pod primitive — used only for test-time introspection
// (reading rendered config off disk); the controller's real side-effect channel to a
// node is the seictl sidecar HTTP task API, never exec.
func execInPod(
	ctx context.Context, t *testing.T, cs *kubernetes.Clientset, ns, pod, container string, argv ...string,
) (string, error) {
	t.Helper()
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return "", fmt.Errorf("load kubeconfig: %w", err)
	}
	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(ns).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   argv,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(cfg, http.MethodPost, req.URL())
	if err != nil {
		return "", fmt.Errorf("new SPDY executor: %w", err)
	}
	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		return stdout.String(), fmt.Errorf(
			"exec %v in %s/%s[%s]: %w (stderr: %s)", argv, ns, pod, container, err, stderr.String())
	}
	return stdout.String(), nil
}
