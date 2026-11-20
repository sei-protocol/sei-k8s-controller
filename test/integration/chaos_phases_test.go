//go:build integration

package integration

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
	"github.com/sei-protocol/sei-k8s-controller/test/chaoscheck"
	"github.com/sei-protocol/sei-k8s-controller/test/manifests/faults"
)

// Scenarios in this file restart a node while it is in a specific phase of its
// life (replaying on start, block syncing, state syncing), and the load wrapper
// that runs any scenario under traffic. Landing a restart in a phase is
// best-effort, because each phase can be shorter than the time it takes to
// notice it and inject a fault. Every scenario here fails loudly when it could not
// create the phase, and logs what it observed at the moment of the kill, so a
// green run is not a silent claim that the phase was covered.

// snapshotIntervalKey is the sei-config key that sets how often a node emits a
// state-sync snapshot (snapshotProductionConfig sets it on the validators).
const snapshotIntervalKey = "storage.snapshot_interval"

// loadConfig is the seiload run that goes on while a scenario's fault is injected.
type loadConfig struct {
	image   string
	commit  string
	profile string
	minutes int
}

// loadFromEnv reads CHAOS_LOAD_MINUTES and the seiload inputs. It reports false
// when load is off. The chaos image is the mock_balances build, which seeds every
// EVM address with a balance at genesis, so the default profile needs no funded
// root key.
func loadFromEnv(t *testing.T) (loadConfig, bool) {
	t.Helper()
	minutes := envInt(t, chaosLoadMinutesEnv, 0)
	if minutes <= 0 {
		return loadConfig{}, false
	}
	return loadConfig{
		image:   mustEnv(t, "SEILOAD_IMAGE"),
		commit:  envOr("SEILOAD_COMMIT_ID", ""),
		profile: envOr("SEILOAD_PROFILE", "nightly_evm_transfer"),
		minutes: minutes,
	}, true
}

// loadRPCConfig and loadRPCConfigValues are the follower settings the benchmark
// suite applies so its RPC nodes absorb the load: a bigger EVM worker pool and tx
// pool, and seid's per-client rate limit off (sei-load sends from one pod, so with
// the limit on the run would measure the limiter).
var (
	loadRPCConfig = map[string]string{
		"evm.worker_pool_size":  "32",
		"evm.worker_queue_size": "4000",
		"evm.max_tx_pool_txs":   "10000",
	}
	loadRPCConfigValues = []sei.ConfigValue{
		{FileName: "app.toml", Key: "evm.rate_limiting_enabled", Value: false},
	}
)

// runWithLoad runs a scenario with seiload going for its whole length: the load
// starts before the scenario captures its pre-fault height and keeps running
// through the fault and the recovery gate.
//
// Two checks bracket the scenario, because a green recovery on an idle chain says
// nothing about recovery under traffic:
//   - before the fault, transactions must be landing in blocks, or the load is
//     not real and the scenario would not be a test of anything under load;
//   - after the scenario's own recovery gate, transactions must land again, so a
//     chain that recovered but stopped taking traffic fails.
//
// seiload's own thresholds (its run length and revert ratio) are not applied: a
// fault makes sends fail by design. The Job is stopped once the checks are done,
// and createJob's cleanup deletes it on every other path.
func (r *chaosRun) runWithLoad(ctx context.Context, t *testing.T, s spec, sc extendedScenario) {
	t.Helper()
	run := startSeiload(ctx, t, r.cs, r.ch, s)
	r.waitIncluded(ctx, t, run, run.startHeight, "before the fault")

	sc.run(ctx, t, r)

	// The scenario ended after its recovery gate. Load that is still going must be
	// taken by the chain again.
	from, ok := sei.LatestHeight(ctx, r.hc, run.tmRPC)
	if !ok {
		t.Fatalf("read the tip after the scenario from %s", run.tmRPC)
	}
	r.waitIncluded(ctx, t, run, from, "after recovery")
	run.stop(ctx, t, r.cs)
}

// waitIncluded waits, within loadInclusionWindow, until at least one transaction
// has been included in a block after height from, read from the node that takes
// no sends. phase names the point in the scenario for the failure message.
func (r *chaosRun) waitIncluded(ctx context.Context, t *testing.T, run *seiloadRun, from int64, phase string) {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, loadInclusionWindow)
	defer cancel()
	var total int64
	pollUntil(waitCtx, t, "seiload transactions included in blocks "+phase, func(ctx context.Context) bool {
		to, ok := sei.LatestHeight(ctx, run.hc, run.tmRPC)
		if !ok {
			return false
		}
		if to > from {
			total += includedTxCount(ctx, t, run.hc, run.tmRPC, from, to)
			from = to
		}
		return total > 0
	})
	t.Logf("load is landing %s: >=%d transactions included", phase, total)
}

// nodePod names the pod of the SeiNode called node.
func (r *chaosRun) nodePod(ctx context.Context, t *testing.T, node string) string {
	t.Helper()
	l, err := r.cs.CoreV1().Pods(r.ns).List(ctx, metav1.ListOptions{LabelSelector: "sei.io/node=" + node})
	if err != nil || len(l.Items) != 1 {
		t.Fatalf("want exactly one pod for node %s, got %d (err: %v)", node, len(l.Items), err)
	}
	return l.Items[0].Name
}

// seidRestarts returns seid's restart count in pod, and whether it is Ready.
func (r *chaosRun) seidRestarts(ctx context.Context, t *testing.T, pod string) (restarts int32, running, ready bool) {
	t.Helper()
	p, err := r.cs.CoreV1().Pods(r.ns).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return 0, false, false
	}
	st, ok := seidStatus(p)
	if !ok {
		return 0, false, false
	}
	return st.RestartCount, st.State.Running != nil, podReady(p)
}

// waitNodeRestarted waits, within recoveryWindow, until pod's seid container has
// restarted more than before times and the pod is Ready. A restart count that has
// to rise is what separates "recovered" from "Ready only because the kill had not
// registered yet".
func (r *chaosRun) waitNodeRestarted(ctx context.Context, t *testing.T, pod string, before int32) {
	t.Helper()
	recCtx, cancel := context.WithTimeout(ctx, recoveryWindow)
	defer cancel()
	pollUntil(recCtx, t, fmt.Sprintf("%s seid restarted and Ready", pod), func(ctx context.Context) bool {
		n, _, ready := r.seidRestarts(ctx, t, pod)
		return n > before && ready
	})
}

// scenarioKillDuringReplay SIGKILLs validator-0 repeatedly, each time right after
// its container is running again, watching for the window in which the node is up
// but its RPC listener is not. The node handshakes and replays before it starts
// the RPC server, so a refused RPC connection with a running container is the one
// observable sign of startup, handshake and replay. Whether a kill lands in it is
// best effort: the next kill is injected a moment after the observation, and a
// small chain replays in well under a second.
//
// It fails if no attempt ever saw the node in that window, so a chain too small
// to have a replay phase cannot pass as if it had. The remedies are to run with
// CHAOS_LOAD_MINUTES (kills then land mid-block, and replay has real work) or on
// the flatkv profile (whose four stores can come back at different heights).
func scenarioKillDuringReplay(ctx context.Context, t *testing.T, r *chaosRun) {
	const attempts = 5
	start := time.Now()
	floor := r.floor(ctx, t)
	pod := r.nodePod(ctx, t, r.id+"-0")
	rpc := r.endpoints(ctx, t)[0] // validator-0
	restarts, _, _ := r.seidRestarts(ctx, t, pod)

	// A short per-request timeout: during replay the RPC connection is refused, and
	// a 10s client timeout would hide the window it is meant to see.
	probe := *r.hc
	probe.Timeout = 500 * time.Millisecond

	hits := 0
	for i := 1; i <= attempts; i++ {
		f := r.apply(ctx, t, "container-kill-validator-0", fmt.Sprintf("-p%d", i), "")
		r.gateInjectedExactly(ctx, t, f, 1)

		var rpcDown bool
		var seen int32
		killCtx, cancelKill := context.WithTimeout(ctx, recoveryWindow)
		pollEvery(killCtx, t, fmt.Sprintf("kill %d: %s restarted and running", i, pod), 200*time.Millisecond,
			func(ctx context.Context) bool {
				n, running, _ := r.seidRestarts(ctx, t, pod)
				if n <= restarts || !running {
					return false
				}
				_, err := chaoscheck.Sync(ctx, &probe, rpc)
				rpcDown = err != nil
				seen = n
				return true
			})
		cancelKill()
		restarts = seen
		if rpcDown {
			hits++
		}
		t.Logf("kill %d/%d: container running again, RPC listener up: %v", i, attempts, !rpcDown)
	}
	t.Logf("%d of %d restarts were observed before the RPC listener came up", hits, attempts)
	if hits == 0 {
		t.Errorf("no restart was ever observed before the RPC listener came up, so no kill had a replay "+
			"phase to land in; the chain replays too fast (try %s or the flatkv profile)", chaosLoadMinutesEnv)
	}

	r.waitAllReady(ctx, t)
	r.assertRecovered(ctx, t, start, floor)
}

// scenarioRestartDuringBlockSync adds a follower to a chain that already has
// CHAOS_SYNC_DEPTH blocks, waits until the follower reports it is catching up,
// SIGKILLs it there, and checks that it resumes block sync, catches up, and
// commits the same state as every other node.
//
// The follower is not a validator, so the kill goes through container-kill-node,
// which pins sei.io/node to its name (followers carry no sei.io/nodedeployment).
// If the follower had already caught up by the time it could be observed, the
// phase was missed; the scenario fails and says to raise the depth.
func scenarioRestartDuringBlockSync(ctx context.Context, t *testing.T, r *chaosRun) {
	depth := int64(envInt(t, chaosSyncDepthEnv, defaultSyncDepth))
	waitCtx, cancelWait := context.WithTimeout(ctx, 20*time.Minute)
	pollUntil(waitCtx, t, fmt.Sprintf("chain reaching height %d", depth), func(ctx context.Context) bool {
		h, ok := sei.LatestHeight(ctx, r.hc, r.follower().TendermintRPC())
		return ok && h >= depth
	})
	cancelWait()

	start := time.Now()
	floor := r.floor(ctx, t)
	name := rpcNodeName(r.id, len(r.ch.rpcNodes))
	node, err := createRPCNode(ctx, t, r.c, sei.NodeSpec{
		Name:      name,
		Network:   r.id,
		Namespace: r.ns,
		Image:     r.image,
		Labels:    map[string]string{runLabelKey: r.id},
		Config:    r.storage,
	})
	if node != nil {
		r.ch.rpcNodes = append(r.ch.rpcNodes, node) // so cleanupChain reaps it
	}
	if err != nil {
		t.Fatalf("add follower %s: %v", name, err)
	}
	pod := r.nodePod(ctx, t, name)

	// Catch it syncing: the RPC answers, catching_up is true, and it has height.
	var atKill chaoscheck.SyncInfo
	syncCtx, cancelSync := context.WithTimeout(ctx, 2*time.Minute)
	pollEvery(syncCtx, t, name+" block syncing", 500*time.Millisecond, func(ctx context.Context) bool {
		s, err := chaoscheck.Sync(ctx, r.hc, node.TendermintRPC())
		if err != nil || s.Height < 1 {
			return false // the RPC is not up yet, or nothing has synced yet
		}
		if !s.CatchingUp {
			t.Fatalf("%s had already caught up (height %d) when first observed, before a kill could land in "+
				"block sync; raise %s (now %d)", name, s.Height, chaosSyncDepthEnv, depth)
		}
		atKill = s
		return true
	})
	cancelSync()
	before, _, _ := r.seidRestarts(ctx, t, pod)

	f := r.applyWith(ctx, t, "container-kill-node", "", "", func(p *faults.Params) { p.Node = name })
	r.gateInjectedExactly(ctx, t, f, 1)
	t.Logf("%s killed while block syncing at height %d of ~%d", name, atKill.Height, depth)

	r.waitNodeRestarted(ctx, t, pod, before)
	r.assertFollowerResumed(ctx, t, node, atKill.Height)
	r.assertRecovered(ctx, t, start, floor)
}

// scenarioRestartDuringStateSync adds a follower that bootstraps from a snapshot,
// SIGKILLs it while it is state syncing, and checks that it starts over and ends
// up caught up from a snapshot, not stuck on a half-applied store.
//
// The validators emit snapshots (the chain is provisioned with
// snapshotProductionConfig) and the chain is advanced past a snapshot boundary
// before the follower is created. State sync has no restart point: a snapshot
// restore is not resumable, so the node has to discover a snapshot and restore it
// again, and the committed-state check at the end is what shows it did so cleanly.
//
// The phase is read from /status: while state syncing, the node reports
// catching_up with height 0, then jumps to the snapshot height. A height above 0
// on the first read means the restore had already finished.
func scenarioRestartDuringStateSync(ctx context.Context, t *testing.T, r *chaosRun) {
	interval, err := strconv.Atoi(snapshotProductionConfig[snapshotIntervalKey])
	if err != nil {
		t.Fatalf("snapshot_interval not an int: %v", err)
	}
	// A snapshot has to exist before a follower can state-sync from one.
	if err := sei.WaitHeightAdvances(ctx, r.hc, r.ch.network.TendermintRPC(), int64(interval)+10); err != nil {
		t.Fatalf("network did not advance past the snapshot interval: %v", err)
	}
	witnessNS := witnessNamespace(t, r.ch.network)
	witnesses := []string{
		nodeRPC(fmt.Sprintf("%s-0", r.id), witnessNS), // validator-0
		nodeRPC(rpcNodeName(r.id, 0), witnessNS),      // follower 0
	}
	assertWitnessFresh(ctx, t, r.hc, witnesses[0], witnesses[1], interval)

	start := time.Now()
	floor := r.floor(ctx, t)
	name := "statesync-" + r.id
	node, err := r.c.CreateNode(ctx, sei.NodeSpec{
		Name:      name,
		Network:   r.id,
		Namespace: r.ns,
		Image:     r.image,
		Labels:    map[string]string{runLabelKey: r.id},
		// Pruning off, as on the validators that produced the snapshot, but no
		// snapshots of its own (it is a light-client witness's peer, not a producer),
		// and the block-store base pinned (keep all blocks) so a restart that wiped
		// the store would show as the earliest height moving.
		Config: mergeConfig(mergeConfig(r.storage, snapshotProductionConfig), map[string]string{
			snapshotIntervalKey:       "0",
			"chain.min_retain_blocks": "0",
		}),
		StateSync: &sei.NodeStateSync{RpcServers: witnesses},
	})
	if node != nil {
		r.ch.rpcNodes = append(r.ch.rpcNodes, node) // so cleanupChain reaps it
	}
	if err != nil {
		t.Fatalf("create state-sync node %s: %v", name, err)
	}
	if err := node.WaitReady(ctx); err != nil {
		t.Fatalf("state-sync node %s running: %v", name, err)
	}
	pod := r.nodePod(ctx, t, name)

	// Catch it state syncing: the RPC answers with catching_up and no height yet.
	syncCtx, cancelSync := context.WithTimeout(ctx, 3*time.Minute)
	pollEvery(syncCtx, t, name+" state syncing", 500*time.Millisecond, func(ctx context.Context) bool {
		s, err := chaoscheck.Sync(ctx, r.hc, node.TendermintRPC())
		if err != nil {
			return false // the RPC is not up yet
		}
		if s.Height > 0 {
			t.Fatalf("%s had already restored a snapshot (height %d) when first observed, before a kill could "+
				"land in state sync", name, s.Height)
		}
		return s.CatchingUp
	})
	cancelSync()
	before, _, _ := r.seidRestarts(ctx, t, pod)

	f := r.applyWith(ctx, t, "container-kill-node", "", "", func(p *faults.Params) { p.Node = name })
	r.gateInjectedExactly(ctx, t, f, 1)
	t.Logf("%s killed while state syncing (catching up, no snapshot restored yet)", name)

	r.waitNodeRestarted(ctx, t, pod, before)
	// It must start over and still come from a snapshot, not replay from genesis.
	r.assertFollowerResumed(ctx, t, node, 0)
	hc := r.hc
	earliest, ok := sei.EarliestHeight(ctx, hc, node.TendermintRPC())
	switch {
	case !ok:
		t.Errorf("read the earliest height of %s after the restart", name)
	case earliest <= 1:
		t.Errorf("%s earliest height is %d after the restart, want > 1: it replayed from genesis instead of "+
			"restoring a snapshot", name, earliest)
	}
	r.assertRecovered(ctx, t, start, floor)
}

// assertFollowerResumed checks that a follower killed mid-sync catches up, within
// 5 minutes, and that it did not lose the blocks it had already synced: its height
// after the restart is not below atKill by more than a block or two (the one in
// flight when it died). Failures are recorded with t.Errorf so the rest of the
// recovery gate still runs.
func (r *chaosRun) assertFollowerResumed(ctx context.Context, t *testing.T, node *sei.Node, atKill int64) {
	t.Helper()
	catchCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := sei.WaitCaughtUp(catchCtx, r.hc, node.TendermintRPC()); err != nil {
		t.Errorf("%s did not catch up after the restart: %v", node.Name(), err)
		return
	}
	h, ok := sei.LatestHeight(ctx, r.hc, node.TendermintRPC())
	if !ok {
		t.Errorf("read the height of %s after the restart", node.Name())
		return
	}
	if h < atKill-2 {
		t.Errorf("%s is at height %d after the restart, below the %d it had synced when killed: "+
			"it lost blocks", node.Name(), h, atKill)
	}
}
