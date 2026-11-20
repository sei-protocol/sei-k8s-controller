//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
	"github.com/sei-protocol/sei-k8s-controller/test/chaoscheck"
	"github.com/sei-protocol/sei-k8s-controller/test/manifests/faults"
)

// Scenarios in this file restart a node while it is block syncing or state
// syncing, and the load wrapper that runs any scenario under traffic. Landing a
// restart in a phase is best-effort, because each phase can be shorter than the
// time it takes to notice it and inject a fault. Each scenario therefore checks
// AFTER the kill that it landed in the phase, from evidence the restarted node or
// the killed container left behind, and fails loudly when it did not, instead of
// logging a claim nobody checked.
//
// There is deliberately no WAL-replay scenario. A node replays at most one block
// on start, in milliseconds, and the RPC listener binds last, after every other
// service has started, so "container running, RPC refused" spans the whole of
// startup and says nothing about replay. Replay is exercised only indirectly:
// kill-during-recovery under load kills a node mid-block and again as it restarts.

// snapshotIntervalKey is the sei-config key that sets how often a node emits a
// state-sync snapshot (snapshotProductionConfig sets it on the validators).
const snapshotIntervalKey = "storage.snapshot_interval"

// The two seid log lines that bracket state sync (sei-tendermint
// internal/statesync/reactor.go). They are the only signal that a node is state
// syncing: the state-sync reactor blocks node start, and the RPC listener binds
// after it, so /status is refused for the whole phase.
const (
	stateSyncStartLog = "starting state sync"
	stateSyncDoneLog  = "Finished state sync"
)

// blockSyncEndLog is the seid log line that marks the end of block sync
// (sei-tendermint internal/blocksync/reactor.go): the node hands over to consensus.
// A follower whose log lacks it was still block syncing.
const blockSyncEndLog = "switching to consensus reactor"

// seidLogLimit caps how much of seid's log is read. The log is read from its start,
// not its tail, because what is looked for is an early line ("starting state sync")
// that per-chunk progress logging would push out of a tail.
const seidLogLimit = 16 << 20

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
// the limit on the run would measure the limiter). They are copies of
// benchmark_test.go's rpcConfig and rpcConfigValues; keep the two in step.
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
// The load Job runs CHAOS_LOAD_MINUTES and nothing extends it, so it must outlast
// the scenario. waitIncluded fails with its own message when the Job has ended, so
// a Job that expired mid-scenario is not mistaken for a chain that stopped taking
// traffic.
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
// no sends. phase names the point in the scenario for the failure message. A read
// that fails (the observer briefly unreachable right after a fault) is retried, not
// fatal; only the window expiring, or the load Job having ended, fails.
func (r *chaosRun) waitIncluded(ctx context.Context, t *testing.T, run *seiloadRun, from int64, phase string) {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, loadInclusionWindow)
	defer cancel()
	var total int64
	pollUntil(waitCtx, t, "seiload transactions included in blocks "+phase, func(ctx context.Context) bool {
		if ended, why := seiloadEnded(ctx, r.cs, run); ended {
			t.Fatalf("the seiload Job ended before the scenario did (%s), so no load is running %s: "+
				"raise %s to at least the scenario's budget (see the README)", why, phase, chaosLoadMinutesEnv)
		}
		to, ok := sei.LatestHeight(ctx, run.hc, run.tmRPC)
		if !ok {
			return false
		}
		if to > from {
			n, ok := includedSoft(ctx, run.hc, run.tmRPC, from, to)
			if !ok {
				return false // retry the same range
			}
			total += n
			from = to
		}
		return total > 0
	})
	t.Logf("load is landing %s: >=%d transactions included", phase, total)
}

// seiloadEnded reports whether run's Job has completed or failed, and why. A Job
// that cannot be read is treated as still running; the poll will retry.
func seiloadEnded(ctx context.Context, cs *kubernetes.Clientset, run *seiloadRun) (bool, string) {
	job, err := cs.BatchV1().Jobs(run.ns).Get(ctx, run.job.Name, metav1.GetOptions{})
	if err != nil {
		return false, ""
	}
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return true, "it completed its run"
		case batchv1.JobFailed:
			return true, "it failed: " + c.Message
		}
	}
	return false, ""
}

// includedSoft is includedTxCount without the t.Fatalf: it counts the transactions
// in blocks from+1 through to on the node at tmRPC, and returns ok=false on a page
// that cannot be read, decoded or is short, so a poll can retry. includedTxCount
// fails closed because the nightly calls it on a healthy chain after load; here it
// would run right after a fault, when a few seconds of an unreachable RPC is
// expected, and must not turn a healthy recovery into a harness failure.
func includedSoft(ctx context.Context, hc *http.Client, tmRPC string, from, to int64) (int64, bool) {
	var total int64
	for lo := from + 1; lo <= to; lo += blockchainPageSize {
		hi := min(lo+blockchainPageSize-1, to)
		var page blockchainInfo
		url := fmt.Sprintf("%s/blockchain?minHeight=%d&maxHeight=%d", tmRPC, lo, hi)
		if !getJSONInto(ctx, hc, url, &page) {
			return total, false
		}
		metas := page.metas()
		if int64(len(metas)) != hi-lo+1 {
			return total, false // a short page is a pruned range or an error envelope, not zero
		}
		for _, m := range metas {
			n, err := strconv.ParseInt(m.NumTxs, 10, 64)
			if err != nil {
				return total, false
			}
			total += n
		}
		if total > 0 {
			return total, true
		}
	}
	return total, true
}

// nodePod names the pod of the SeiNode called node, waiting up to 3 minutes for it
// to exist: CreateNode returns before the controller has built the node's
// StatefulSet, so a node created a moment ago has no pod yet.
func (r *chaosRun) nodePod(ctx context.Context, t *testing.T, node string) string {
	t.Helper()
	var name string
	waitCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	pollEvery(waitCtx, t, "exactly one pod for node "+node, time.Second, func(ctx context.Context) bool {
		l, err := r.cs.CoreV1().Pods(r.ns).List(ctx, metav1.ListOptions{LabelSelector: "sei.io/node=" + node})
		if err != nil || len(l.Items) != 1 {
			return false
		}
		name = l.Items[0].Name
		return true
	})
	return name
}

// refreshFollower re-reads the SeiNode called name and puts the fresh handle in
// ch.rpcNodes in place of the one CreateNode returned. That handle carries the
// rendered spec with an empty status, so its endpoints are empty until it is read
// again; the committed-state check needs the follower's Tendermint RPC. It waits,
// within 2 minutes, for the node's status to publish one.
func (r *chaosRun) refreshFollower(ctx context.Context, t *testing.T, name string) *sei.Node {
	t.Helper()
	var fresh *sei.Node
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	pollEvery(waitCtx, t, name+" publishing a Tendermint RPC endpoint", 2*time.Second, func(ctx context.Context) bool {
		n, err := r.c.GetNode(ctx, name, r.ns)
		if err != nil || n.TendermintRPC() == "" {
			return false
		}
		fresh = n
		return true
	})
	for i, n := range r.ch.rpcNodes {
		if n.Name() == name {
			r.ch.rpcNodes[i] = fresh
		}
	}
	return fresh
}

// seidRestarts returns seid's restart count in pod, and whether it is running and
// whether the pod is Ready. ok is false when the pod or its seid status cannot be
// read, which callers must not mistake for "zero restarts": a count read during an
// API blip and kept as a baseline would let a later wait pass before any restart.
func (r *chaosRun) seidRestarts(ctx context.Context, pod string) (restarts int32, running, ready, ok bool) {
	p, err := r.cs.CoreV1().Pods(r.ns).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return 0, false, false, false
	}
	st, found := seidStatus(p)
	if !found {
		return 0, false, false, false
	}
	return st.RestartCount, st.State.Running != nil, podReady(p), true
}

// restartsBefore reads pod's seid restart count as the baseline for a kill,
// retrying within a minute until the pod can be read, and failing if it cannot.
func (r *chaosRun) restartsBefore(ctx context.Context, t *testing.T, pod string) int32 {
	t.Helper()
	var n int32
	waitCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	pollEvery(waitCtx, t, "reading "+pod+"'s seid restart count", time.Second, func(ctx context.Context) bool {
		var ok bool
		n, _, _, ok = r.seidRestarts(ctx, pod)
		return ok
	})
	return n
}

// restartedOnce reports whether pod's seid container has restarted exactly once
// since before. A scenario reads the PREVIOUS container's log to see what the
// killed instance was doing; that is only the killed instance's log if there was
// exactly one restart. More means the node crashed again on its own, and the
// previous log is of a later instance, so the evidence is not the evidence wanted.
// It records the failure and returns false so the caller skips that evidence.
func (r *chaosRun) restartedOnce(ctx context.Context, t *testing.T, pod string, before int32) bool {
	t.Helper()
	n, _, _, ok := r.seidRestarts(ctx, pod)
	if !ok {
		t.Errorf("read %s's seid restart count after the kill", pod)
		return false
	}
	if n != before+1 {
		t.Errorf("%s's seid restarted %d times after a single kill (count %d -> %d): it crashed on its own, "+
			"so the previous container's log is not the killed instance's", pod, n-before, before, n)
		return false
	}
	return true
}

// waitSeidRestarted waits, within recoveryWindow, until pod's seid container has
// restarted more than before times, polling every 200ms. It does not wait for the
// pod to be Ready: a follower's readiness probe is lag-based, so Ready arrives
// only once it is near the tip, and a caller that must read the node right after
// it restarts cannot wait for that.
func (r *chaosRun) waitSeidRestarted(ctx context.Context, t *testing.T, pod string, before int32) {
	t.Helper()
	recCtx, cancel := context.WithTimeout(ctx, recoveryWindow)
	defer cancel()
	pollEvery(recCtx, t, fmt.Sprintf("%s seid restarted", pod), 200*time.Millisecond, func(ctx context.Context) bool {
		n, _, _, ok := r.seidRestarts(ctx, pod)
		return ok && n > before
	})
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
		n, _, ready, ok := r.seidRestarts(ctx, pod)
		return ok && n > before && ready
	})
}

// seidLog returns the start of seid's log in pod (up to seidLogLimit bytes). previous selects the container
// instance that was running before the last restart: kubelet keeps that one's log,
// which is how a scenario checks, after the kill, what the killed instance was
// doing when it died.
func (r *chaosRun) seidLog(ctx context.Context, pod string, previous bool) (string, error) {
	limit := int64(seidLogLimit)
	raw, err := r.cs.CoreV1().Pods(r.ns).GetLogs(pod, &corev1.PodLogOptions{
		Container:  seidContainer,
		Previous:   previous,
		LimitBytes: &limit,
	}).DoRaw(ctx)
	return string(raw), err
}

// firstSync reads /status of node as soon as its RPC answers, polling every 200ms
// within 2 minutes. It is the first look at the node after a restart, before it has
// had time to sync further.
func firstSync(ctx context.Context, t *testing.T, hc *http.Client, node *sei.Node) chaoscheck.SyncInfo {
	t.Helper()
	pollCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var got chaoscheck.SyncInfo
	pollEvery(pollCtx, t, node.Name()+" answering /status after the restart", 200*time.Millisecond,
		func(ctx context.Context) bool {
			s, err := chaoscheck.Sync(ctx, hc, node.TendermintRPC())
			if err != nil {
				return false
			}
			got = s
			return true
		})
	return got
}

// scenarioRestartDuringBlockSync adds a follower to a chain that already has
// CHAOS_SYNC_DEPTH blocks, waits until the follower reports it is catching up,
// SIGKILLs it there, and checks that it resumes block sync, catches up, and
// commits the same state as every other node.
//
// The follower is not a validator, so the kill goes through container-kill-node,
// which pins sei.io/node to its name (followers carry no sei.io/nodedeployment).
//
// The kill is injected a couple of seconds after the follower is observed syncing,
// and on a shallow chain it can finish syncing in that gap. So the scenario checks
// afterwards, from the killed container's own log (kubelet keeps the previous
// instance's): a follower that had finished block sync logged "switching to
// consensus reactor" before it died. If that line is there, the kill missed the
// phase and the scenario fails and says to raise the depth. A comparison of the
// restarted node's height with the chain tip would not do: the RPC binds only after
// the reactors start, so a follower that really was mid-sync keeps syncing, fast,
// for the whole of startup before its height can be read. That first height is
// still used for what it is good for, bounding data loss: it must not be below the
// height the follower had when it was observed, less the block in flight.
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
	var observed chaoscheck.SyncInfo
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
		observed = s
		return true
	})
	cancelSync()
	before := r.restartsBefore(ctx, t, pod)

	f := r.applyWith(ctx, t, "container-kill-node", "", "", func(p *faults.Params) { p.Node = name })
	r.gateInjectedExactly(ctx, t, f, 1)
	r.waitSeidRestarted(ctx, t, pod, before)

	// The killed instance's own log shows whether block sync had already ended.
	if r.restartedOnce(ctx, t, pod, before) {
		prev, err := r.seidLog(ctx, pod, true)
		switch {
		case err != nil:
			t.Errorf("read the killed %s container's log: %v", name, err)
		case strings.Contains(prev, blockSyncEndLog):
			t.Errorf("the killed %s instance had already finished block sync (%q is in its log): the kill landed "+
				"after the phase; raise %s (now %d)", name, blockSyncEndLog, chaosSyncDepthEnv, depth)
		default:
			t.Logf("%s was killed mid block sync: observed at height %d, and the killed instance never logged %q",
				name, observed.Height, blockSyncEndLog)
		}
	}

	first := firstSync(ctx, t, r.hc, node)
	if first.Height < observed.Height-2 {
		t.Errorf("%s resumed at height %d, below the %d it had synced when observed: it lost blocks",
			name, first.Height, observed.Height)
	}

	catchCtx, cancelCatch := context.WithTimeout(ctx, 5*time.Minute)
	if err := sei.WaitCaughtUp(catchCtx, r.hc, node.TendermintRPC()); err != nil {
		t.Errorf("%s did not catch up after the restart: %v", name, err)
	}
	cancelCatch()
	r.waitNodeRestarted(ctx, t, pod, before)
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
// The phase can only be seen in seid's log. The state-sync reactor blocks node
// start and the RPC listener binds after it, so /status is refused for the whole
// phase. The kill waits for "starting state sync" without "Finished state sync" in
// the follower's log, which needs seid logging at info level; if the line never
// appears the scenario fails and says so. After the restart, the killed
// container's own log (kubelet keeps the previous instance's) must show it
// started state sync and never finished: that is the proof the kill landed in the
// phase, not a belief that it did.
func scenarioRestartDuringStateSync(ctx context.Context, t *testing.T, r *chaosRun) {
	interval, err := strconv.Atoi(snapshotProductionConfig[snapshotIntervalKey])
	if err != nil {
		t.Fatalf("snapshot_interval not an int: %v", err)
	}
	// A snapshot has to exist before a follower can state-sync from one.
	advCtx, cancelAdv := context.WithTimeout(ctx, 10*time.Minute)
	if err := sei.WaitHeightAdvances(advCtx, r.hc, r.ch.network.TendermintRPC(), int64(interval)+10); err != nil {
		t.Fatalf("network did not advance past the snapshot interval: %v", err)
	}
	cancelAdv()
	// The witnesses are two distinct full nodes that serve light-client trust
	// points: validator-0 and the observer. Not the first follower, which with load
	// on is the node taking every send and could lag the head.
	witnessNS := witnessNamespace(t, r.ch.network)
	witnesses := []string{
		nodeRPC(fmt.Sprintf("%s-0", r.id), witnessNS),
		nodeRPC(r.follower().Name(), witnessNS),
	}
	assertWitnessFresh(ctx, t, r.hc, witnesses[0], witnesses[1], interval)

	start := time.Now()
	floor := r.floor(ctx, t)
	name := r.id + "-statesync"
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
	pod := r.nodePod(ctx, t, name)

	// Catch it state syncing, from its log. node.WaitReady is not used: it needs the
	// RPC to answer, which only happens after state sync has finished.
	syncCtx, cancelSync := context.WithTimeout(ctx, 5*time.Minute)
	pollEvery(syncCtx, t, fmt.Sprintf("%s logging %q (seid must log at info level or lower, and the suite's "+
		"service account must read pods/log; the RPC is not up during state sync, so the log is the only signal)",
		name, stateSyncStartLog), 2*time.Second, func(ctx context.Context) bool {
		log, err := r.seidLog(ctx, pod, false)
		if err != nil {
			return false // the container has not started yet
		}
		if strings.Contains(log, stateSyncDoneLog) {
			t.Fatalf("%s had already finished state sync when first observed, before a kill could land in it", name)
		}
		return strings.Contains(log, stateSyncStartLog)
	})
	cancelSync()
	before := r.restartsBefore(ctx, t, pod)

	f := r.applyWith(ctx, t, "container-kill-node", "", "", func(p *faults.Params) { p.Node = name })
	r.gateInjectedExactly(ctx, t, f, 1)
	r.waitSeidRestarted(ctx, t, pod, before)

	// The killed instance's own log is the evidence the kill landed in state sync.
	if r.restartedOnce(ctx, t, pod, before) {
		prev, err := r.seidLog(ctx, pod, true)
		switch {
		case err != nil:
			t.Errorf("read the killed %s container's log: %v", name, err)
		case !strings.Contains(prev, stateSyncStartLog):
			t.Errorf("the killed %s instance's log has no %q: it was not state syncing when it died",
				name, stateSyncStartLog)
		case strings.Contains(prev, stateSyncDoneLog):
			t.Errorf("the killed %s instance had already finished state sync (%q is in its log): the kill landed "+
				"after the phase", name, stateSyncDoneLog)
		default:
			t.Logf("%s was killed mid state sync: the killed instance logged %q and never %q", name,
				stateSyncStartLog, stateSyncDoneLog)
		}
	}

	// It must start over, and still come from a snapshot, not replay from genesis.
	// CreateNode returned a handle with no status, so re-read the node for its RPC.
	r.waitNodeRestarted(ctx, t, pod, before)
	node = r.refreshFollower(ctx, t, name)
	catchCtx, cancelCatch := context.WithTimeout(ctx, 5*time.Minute)
	if err := sei.WaitCaughtUp(catchCtx, r.hc, node.TendermintRPC()); err != nil {
		t.Errorf("%s did not catch up after the restart: %v", name, err)
	}
	cancelCatch()
	earliest, ok := sei.EarliestHeight(ctx, r.hc, node.TendermintRPC())
	switch {
	case !ok:
		t.Errorf("read the earliest height of %s after the restart", name)
	case earliest <= 1:
		t.Errorf("%s earliest height is %d after the restart, want > 1: a node that restored a snapshot starts "+
			"above 1, so it block-synced from genesis instead (which also happens if the snapshot was pruned away "+
			"while it was down)", name, earliest)
	}
	r.assertRecovered(ctx, t, start, floor)
}
