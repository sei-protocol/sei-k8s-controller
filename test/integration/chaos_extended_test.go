//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
	"github.com/sei-protocol/sei-k8s-controller/test/chaoscheck"
	"github.com/sei-protocol/sei-k8s-controller/test/manifests/faults"
)

// Inputs for TestChaosExtended, beyond TestNightlyChaosSuite's.
const (
	// chaosScenariosEnv narrows the run to a comma-separated list of scenario
	// names. Unset runs every scenario.
	chaosScenariosEnv = "CHAOS_EXTENDED_SCENARIOS"
	// chaosStorageProfileEnv picks the state-commit engine the chain runs on:
	// "memiavl" (the default, what the nightly chaos suite runs) or "flatkv".
	chaosStorageProfileEnv = "CHAOS_STORAGE_PROFILE"
	// chaosRequireCleanShutdownEnv, when "true", makes the clean-stop scenario
	// fail if seid was SIGKILLed instead of exiting on SIGTERM. It is off by
	// default: until SIGTERM reaches seid and storage flushes on it, every run
	// would fail by design, and a run that only logs the exit code is still
	// worth having.
	chaosRequireCleanShutdownEnv = "CHAOS_REQUIRE_CLEAN_SHUTDOWN"
	// chaosLoadMinutesEnv, when above 0, runs seiload against the chain while each
	// scenario's fault is injected, for up to that many minutes. It needs
	// SEILOAD_IMAGE, and optionally SEILOAD_PROFILE and SEILOAD_COMMIT_ID, as the
	// benchmark suite does.
	chaosLoadMinutesEnv = "CHAOS_LOAD_MINUTES"
	// chaosSyncDepthEnv is how many blocks the chain has before restart-during-
	// blocksync adds its follower. A follower of a shallow chain finishes block sync
	// in seconds, before a kill can land; the deeper the chain, the wider the window.
	chaosSyncDepthEnv = "CHAOS_SYNC_DEPTH"
)

const (
	// extendedValidators is the validator count of the chains these scenarios
	// provision. The splits and the kill-all assume four (2|2, quorum 3).
	extendedValidators = 4
	// agreeHeights is how many distinct heights, all above the pre-fault floor,
	// every node must report with the same committed state hash.
	agreeHeights = 3
	// recoveryWindow bounds how long every validator has to come back Ready
	// after a fault, as in TestNightlyChaosSuite. It is sized for a chaos-sized
	// chain: a node with a very large store would need longer, so this suite
	// is not meant for those.
	recoveryWindow = 5 * time.Minute
	// The 2|2 split scenario's timing. quorumLossDuration is how long the
	// partition lasts, counted from the moment the CR is created. Injection can
	// take up to injectWindow before the stall check starts, so the budget has to
	// cover injection, the settle, and the stall window with room to spare.
	quorumLossDuration = 5 * time.Minute
	quorumSettle       = 15 * time.Second
	quorumStallWindow  = 60 * time.Second
	// seidContainer is the consensus container the kill faults target.
	seidContainer = "seid"
	// loadInclusionWindow bounds how long the load has to show up in blocks, both
	// before the fault (it must be real) and after recovery (the chain must take it
	// again).
	loadInclusionWindow = 3 * time.Minute
	// defaultScenarioTimeout is a scenario's whole budget unless its registry entry
	// says otherwise.
	defaultScenarioTimeout = 40 * time.Minute
	// defaultSyncDepth is the chain height restart-during-blocksync waits for
	// before adding its follower, unless CHAOS_SYNC_DEPTH says otherwise.
	defaultSyncDepth = 600
)

// extendedScenario is one TestChaosExtended subtest. Each runs on its own fresh
// chain, as the nightly suite's faults do, so one scenario's damage cannot
// leak into the next.
type extendedScenario struct {
	name string
	// snapshots makes the chain's validators emit state-sync snapshots, which a
	// scenario that state-syncs a follower needs.
	snapshots bool
	// timeout is the scenario's whole budget, from before the chain is provisioned.
	// Zero means defaultScenarioTimeout. The two sync scenarios wait for chain depth
	// or a snapshot boundary first, so they need more.
	timeout time.Duration
	run     func(ctx context.Context, t *testing.T, r *chaosRun)
}

var extendedScenarios = []extendedScenario{
	{name: "kill-during-recovery", run: scenarioKillDuringRecovery},
	{name: "restart-during-blocksync", timeout: 75 * time.Minute, run: scenarioRestartDuringBlockSync},
	{name: "restart-during-statesync", snapshots: true, timeout: 60 * time.Minute, run: scenarioRestartDuringStateSync},
	{name: "cold-start", run: scenarioColdStart},
	{name: "quorum-loss-partition", run: scenarioQuorumLossPartition},
	{name: "network-flap", run: scenarioNetworkFlap},
	{name: "clean-stop", run: scenarioCleanStop},
	{name: "time-skew-forward", run: func(ctx context.Context, t *testing.T, r *chaosRun) {
		scenarioTimedFault(ctx, t, r, "time-skew-forward")
	}},
	{name: "time-skew-large", run: func(ctx context.Context, t *testing.T, r *chaosRun) {
		scenarioTimedFault(ctx, t, r, "time-skew-large")
	}},
}

// TestChaosExtended runs faults the nightly suite does not: ones that break
// state mid-write, take away quorum, or probe recovery harder than one kill. It
// is the proving ground for scenarios that are not yet stable enough to turn
// the nightly red. It is deliberately NOT named TestNightly*, so the nightly's
// '^TestNightly' pattern never selects it; run it by name.
//
// What it adds to TestNightlyChaosSuite, and why:
//
//   - kill-during-recovery: SIGKILL validator-0 three times in a row, each time
//     as soon as its container is running again. Replay and recovery code runs
//     against a store that a crash already left half-done.
//   - restart-during-blocksync: add a follower to a chain that is already deep,
//     SIGKILL it while it block syncs, check afterwards that the kill did land
//     mid-sync, and that it resumes and agrees.
//   - restart-during-statesync: the same for a follower bootstrapping from a
//     snapshot, which has to start over because a snapshot restore is not
//     resumable. Detected from seid's log (the RPC is not up during state sync)
//     and verified from the killed container's own log.
//   - cold-start: SIGKILL every validator at once. Each recovers on its own and
//     they can come back at different heights; the chain has to reconcile.
//   - quorum-loss-partition: split the validators 2|2, with the RPC follower on
//     one side (left outside it would relay votes across the split). The chain
//     must STOP (no side has 2/3), then resume without forking. Every f=1 fault
//     tests liveness; this tests the safety side.
//   - network-flap: partition validator-0 on and off five times, 20s each way.
//   - clean-stop: delete a validator pod with a grace period, so seid gets
//     SIGTERM. pod-failure and container-kill both use gracePeriod 0, so the
//     graceful path never runs otherwise.
//   - time-skew-forward / time-skew-large: a clock ahead by 1m, and a clock
//     5m behind (the nightly only skews -30s).
//
// Every scenario ends with the same recovery gate, which is stronger than the
// nightly's: all validators Ready after a real restart (not merely Ready at
// the moment of the check), the follower caught up and advancing, and every
// validator and the follower reporting the same committed state hash at several
// heights above the height the chain had reached before the fault. The hash is
// each node's own (/abci_info), so a node that recovered into different state
// is caught, and so is a fork. A node that recovered into corrupt state and then
// stalled is caught by the Ready and caught-up checks instead.
//
// The suite assumes CometBFT consensus, which is what it provisions: the
// follower's caught-up check reads /status catching_up, which is not meaningful
// under Autobahn.
//
// With CHAOS_LOAD_MINUTES set, every scenario runs under seiload: the load starts
// first, must be landing in blocks before the fault, runs through the fault and
// the recovery gate, and must land again afterwards. Without it the chain is idle,
// and a kill almost always falls between meaningful writes.
//
// Inputs (env): SEI_CHAIN_ID (base) and SEID_IMAGE_CHAOS [both required];
// SEI_NAMESPACE, CHAOS_DURATION, CHAOS_EXTENDED_SCENARIOS, CHAOS_STORAGE_PROFILE,
// CHAOS_REQUIRE_CLEAN_SHUTDOWN, CHAOS_SYNC_DEPTH, and CHAOS_LOAD_MINUTES with
// SEILOAD_IMAGE (required when load is on), SEILOAD_PROFILE, SEILOAD_COMMIT_ID
// [optional]. Run with -test.timeout 0: each scenario budgets 40 minutes, the
// block-sync one 75 and the state-sync one 60.
func TestChaosExtended(t *testing.T) {
	requireCluster(t)
	base := runChainID(mustEnv(t, "SEI_CHAIN_ID"))
	seid := mustEnv(t, seidImageChaosEnv)
	ns := envOr("SEI_NAMESPACE", "")
	duration := envOr("CHAOS_DURATION", "3m")
	faultDur, err := time.ParseDuration(duration)
	if err != nil {
		t.Fatalf("CHAOS_DURATION %q: %v", duration, err)
	}
	if injectWindow >= faultDur {
		t.Fatalf("injectWindow (%s) must be below CHAOS_DURATION (%s)", injectWindow, faultDur)
	}
	storage := extendedStorageConfig(t)
	selected := selectedScenarios(t)
	load, loadOn := loadFromEnv(t)

	for _, sc := range extendedScenarios {
		if !selected(sc.name) {
			continue
		}
		t.Run(sc.name, func(t *testing.T) {
			id := base + "-" + sc.name
			// Selector values are k8s labels, capped at 63 characters. The longest
			// ones derived from the id are the per-fault run id (id + "-flap5"), the
			// follower a block-sync run adds, and the state-sync follower's name.
			if derived := id + "-flap5"; len(derived) > 63 {
				t.Fatalf("chain id %q yields label value %q > 63 chars", id, derived)
			}
			// A SeiNode's name becomes its StatefulSet's name, and the pod is <name>-0:
			// its hostname and its statefulset.kubernetes.io/pod-name label are 63
			// characters at most, so the node name is held to 61.
			for _, node := range []string{rpcNodeName(id, 2), id + "-statesync"} {
				if len(node) > 61 {
					t.Fatalf("chain id %q yields node name %q > 61 chars (its pod name would exceed 63)", id, node)
				}
			}
			timeout := sc.timeout
			if timeout == 0 {
				timeout = defaultScenarioTimeout
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			// A Job eviction or a manual delete sends SIGTERM. Without a signal-aware
			// context the test would not unwind, cleanupChain would never run, and the
			// chain and any live fault CR would be left in the cluster. Every other
			// suite in this package does the same.
			ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
			defer stop()

			c := openClient(ctx, t)
			cs := clientset(t)
			s := spec{
				chainID:       id,
				runID:         id,
				namespace:     ns,
				seidImage:     seid,
				validators:    extendedValidators,
				rpcNodes:      1, // an unfaulted observer of liveness + recovery
				timeout:       timeout,
				storageConfig: storage,
			}
			if sc.snapshots {
				// The validators emit snapshots; the followers do not (a light-client
				// witness serves RPC trust points, chunks travel over p2p from the
				// validators), the way workflow_test.go's state-sync suite provisions it.
				s.storageConfig = mergeConfig(storage, snapshotProductionConfig)
				s.rpcConfig = map[string]string{snapshotIntervalKey: "0"}
			}
			if loadOn {
				// One follower takes the sends and the other serves receipts and is the
				// observer, so the observer's liveness reads do not compete with the load.
				s.rpcNodes = 2
				s.rpcConfig = mergeConfig(s.rpcConfig, loadRPCConfig)
				s.rpcConfigValues = loadRPCConfigValues
				s.seiloadImage = load.image
				s.seiloadProfile = load.profile
				s.seiloadCommit = load.commit
				s.seiloadWorkload = "chaos-" + sc.name
				s.durationMin = load.minutes
			}
			ch, err := provision(ctx, t, c, s)
			cleanupChain(t, ch)
			if err != nil {
				t.Fatalf("provision: %v", err)
			}
			r := &chaosRun{
				id:       id,
				ns:       ch.network.Namespace(),
				ch:       ch,
				c:        c,
				dc:       dynClient(t),
				cs:       cs,
				hc:       &http.Client{Timeout: 10 * time.Second},
				duration: duration,
				faultDur: faultDur,
				image:    seid,
				storage:  storage,
				observer: ch.rpcNodes[len(ch.rpcNodes)-1],
			}
			if loadOn {
				r.runWithLoad(ctx, t, s, sc)
				return
			}
			sc.run(ctx, t, r)
		})
	}
}

// requireCleanShutdown reports whether CHAOS_REQUIRE_CLEAN_SHUTDOWN=true.
func requireCleanShutdown() bool { return envOr(chaosRequireCleanShutdownEnv, "") == "true" }

// extendedStorageConfig maps CHAOS_STORAGE_PROFILE to seid's state-commit
// config. "memiavl" matches TestNightlyChaosSuite. "flatkv" runs the chain on
// FlatKV from genesis, so the same faults hit the storage engine the nightly
// never touches. The nightly chaos image is built for memiavl, so whether it
// boots with flatkv_only has to be confirmed by the first run.
func extendedStorageConfig(t *testing.T) map[string]string {
	t.Helper()
	switch p := envOr(chaosStorageProfileEnv, "memiavl"); p {
	case "memiavl":
		return memiavlStorageConfig
	case "flatkv":
		return flatkvStorageConfig
	default:
		t.Fatalf("%s=%q: want memiavl or flatkv", chaosStorageProfileEnv, p)
		return nil
	}
}

// selectedScenarios returns the CHAOS_EXTENDED_SCENARIOS filter, failing on a
// name that matches no scenario so a typo cannot silently run nothing.
func selectedScenarios(t *testing.T) func(string) bool {
	t.Helper()
	raw := strings.TrimSpace(envOr(chaosScenariosEnv, ""))
	if raw == "" {
		return func(string) bool { return true }
	}
	var want []string
	for n := range strings.SplitSeq(raw, ",") {
		n = strings.TrimSpace(n)
		if !slices.ContainsFunc(extendedScenarios, func(s extendedScenario) bool { return s.name == n }) {
			t.Fatalf("%s: unknown scenario %q", chaosScenariosEnv, n)
		}
		want = append(want, n)
	}
	return func(name string) bool { return slices.Contains(want, name) }
}

// chaosRun is the live state one scenario works against.
type chaosRun struct {
	id       string // chain id; also the sei.io/nodedeployment value
	ns       string // the network's namespace
	ch       *chain
	c        *sei.Client
	dc       dynamic.Interface
	cs       *kubernetes.Clientset
	hc       *http.Client
	duration string        // CHAOS_DURATION, for duration-bearing faults
	faultDur time.Duration // the same, parsed
	image    string        // the seid image, for followers a scenario adds
	storage  map[string]string
	// observer is the follower the scenarios read liveness from. It is fixed when
	// the chain is provisioned: a scenario that adds a follower appends it to
	// ch.rpcNodes (so teardown reaps it), which must not change the observer. With
	// load it is the second follower, the one that takes no sends.
	observer *sei.Node
}

func (r *chaosRun) follower() *sei.Node { return r.observer }

// followerNames returns the sei.io/node value of every RPC follower the chain was
// provisioned with, which network-partition-split puts on one side of the split.
func (r *chaosRun) followerNames() []string {
	names := make([]string, 0, len(r.ch.rpcNodes))
	for _, n := range r.ch.rpcNodes {
		names = append(names, n.Name())
	}
	return names
}

// validatorRPC returns the Tendermint RPC of validator i.
func (r *chaosRun) validatorRPC(ctx context.Context, t *testing.T, i int) string {
	t.Helper()
	name := fmt.Sprintf("%s-%d", r.id, i)
	n, err := r.c.GetNode(ctx, name, r.ns)
	if err != nil {
		t.Fatalf("get validator %s: %v", name, err)
	}
	u := n.TendermintRPC()
	if u == "" {
		t.Fatalf("validator %s has no Tendermint RPC endpoint in its status", name)
	}
	return u
}

// endpoints returns the Tendermint RPC of every validator and every follower, so
// the committed-state check compares all of them.
func (r *chaosRun) endpoints(ctx context.Context, t *testing.T) []string {
	t.Helper()
	urls := make([]string, 0, extendedValidators+len(r.ch.rpcNodes))
	for i := range extendedValidators {
		urls = append(urls, r.validatorRPC(ctx, t, i))
	}
	for _, n := range r.ch.rpcNodes {
		u := n.TendermintRPC()
		if u == "" {
			t.Fatalf("follower %s has no Tendermint RPC endpoint in its status", n.Name())
		}
		urls = append(urls, u)
	}
	return urls
}

// apply renders and applies one fault. suffix keeps the CR name and run label
// unique when a scenario applies the same fault more than once. The fault comes
// from ExtendedCatalog, or the nightly Catalog for network-flap.
func (r *chaosRun) apply(ctx context.Context, t *testing.T, name, suffix, duration string) fault {
	t.Helper()
	return r.applyWith(ctx, t, name, suffix, duration, nil)
}

// applyWith is apply with a hook to set Params fields beyond the chain, run and
// namespace: the node a node-targeted fault hits, or the followers a split puts on
// one side.
func (r *chaosRun) applyWith(
	ctx context.Context, t *testing.T, name, suffix, duration string, mod func(*faults.Params),
) fault {
	t.Helper()
	cf, extErr := faults.LookupExtended(name)
	if extErr != nil {
		var nightlyErr error
		if cf, nightlyErr = faults.Lookup(name); nightlyErr != nil {
			t.Fatalf("lookup fault %q: %v", name, errors.Join(extErr, nightlyErr))
		}
	}
	p := faults.Params{
		ChainID:   r.id,
		RunID:     r.id + suffix,
		Namespace: r.ns,
		Duration:  duration,
	}
	if mod != nil {
		mod(&p)
	}
	f := renderFault(t, cf, p)
	applyFault(ctx, t, r.dc, r.ns, f)
	return f
}

// gateInjectedExactly waits for AllInjected and requires the fault to have hit
// exactly want targets. gateInjected only asserts "exactly one" for one-shot
// kills, which is wrong for a fault whose point is to hit all of them.
func (r *chaosRun) gateInjectedExactly(ctx context.Context, t *testing.T, f fault, want int) {
	t.Helper()
	injectCtx, cancel := context.WithTimeout(ctx, injectWindow)
	defer cancel()
	waitFaultCondition(injectCtx, t, r.dc, r.ns, f, "AllInjected")
	if n := injectedTargets(ctx, t, r.dc, r.ns, f); n != want {
		t.Fatalf("fault %s/%s injected %d targets, want exactly %d", f.resource, f.obj.GetName(), n, want)
	}
}

// validatorPods lists the chain's validator pods.
func (r *chaosRun) validatorPods(ctx context.Context) ([]corev1.Pod, error) {
	l, err := r.cs.CoreV1().Pods(r.ns).List(ctx, metav1.ListOptions{LabelSelector: "sei.io/nodedeployment=" + r.id})
	if err != nil {
		return nil, err
	}
	return l.Items, nil
}

// seidStatus returns the seid container's status in p.
func seidStatus(p *corev1.Pod) (corev1.ContainerStatus, bool) {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name == seidContainer {
			return cs, true
		}
	}
	return corev1.ContainerStatus{}, false
}

// restartCounts snapshots seid's restart count per validator pod.
func (r *chaosRun) restartCounts(ctx context.Context, t *testing.T) map[string]int32 {
	t.Helper()
	pods, err := r.validatorPods(ctx)
	if err != nil {
		t.Fatalf("list validator pods: %v", err)
	}
	counts := make(map[string]int32, len(pods))
	for i := range pods {
		if st, ok := seidStatus(&pods[i]); ok {
			counts[pods[i].Name] = st.RestartCount
		}
	}
	return counts
}

// podUIDs snapshots the UID of each validator pod. A replaced pod keeps its
// StatefulSet name but gets a new UID.
func (r *chaosRun) podUIDs(ctx context.Context, t *testing.T) map[string]types.UID {
	t.Helper()
	pods, err := r.validatorPods(ctx)
	if err != nil {
		t.Fatalf("list validator pods: %v", err)
	}
	uids := make(map[string]types.UID, len(pods))
	for i := range pods {
		uids[pods[i].Name] = pods[i].UID
	}
	return uids
}

// pollUntil calls done every 2s until it returns true or ctx ends, failing the test
// on timeout with what names the wait.
func pollUntil(ctx context.Context, t *testing.T, what string, done func(context.Context) bool) {
	t.Helper()
	pollEvery(ctx, t, what, 2*time.Second, done)
}

// pollEvery is pollUntil with the poll interval chosen by the caller: a scenario
// that has to catch a phase lasting a second or two polls faster than 2s.
func pollEvery(ctx context.Context, t *testing.T, what string, every time.Duration, done func(context.Context) bool) {
	t.Helper()
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if done(ctx) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s: %v", what, ctx.Err())
		case <-tick.C:
		}
	}
}

// waitRestarted blocks until each pod in before has restarted its seid
// container more than before[pod] times and every validator is Ready. A
// restart count that has to rise is what separates "recovered" from "Ready
// only because the kill had not registered yet": a killed container can stay
// Ready until the kubelet notices.
func (r *chaosRun) waitRestarted(ctx context.Context, t *testing.T, before map[string]int32, killed int) {
	t.Helper()
	recCtx, cancel := context.WithTimeout(ctx, recoveryWindow)
	defer cancel()
	pollUntil(recCtx, t, fmt.Sprintf("%d validator(s) restarted and Ready", killed), func(ctx context.Context) bool {
		pods, err := r.validatorPods(ctx)
		if err != nil || len(pods) < len(before) {
			return false
		}
		restarted, ready := 0, 0
		for i := range pods {
			if st, ok := seidStatus(&pods[i]); ok && st.RestartCount > before[pods[i].Name] {
				restarted++
			}
			if podReady(&pods[i]) {
				ready++
			}
		}
		return restarted >= killed && ready >= len(before)
	})
}

// waitReplaced blocks until at least one validator pod has been replaced (new
// UID under an old name) and every validator is Ready. Checking Ready alone
// would pass at once: a pod being deleted with a grace period stays Ready until
// its container stops.
func (r *chaosRun) waitReplaced(ctx context.Context, t *testing.T, before map[string]types.UID) {
	t.Helper()
	recCtx, cancel := context.WithTimeout(ctx, recoveryWindow)
	defer cancel()
	pollUntil(recCtx, t, "a validator pod replaced and every validator Ready", func(ctx context.Context) bool {
		pods, err := r.validatorPods(ctx)
		if err != nil || len(pods) < len(before) {
			return false
		}
		replaced, ready := 0, 0
		for i := range pods {
			if old, ok := before[pods[i].Name]; ok && old != pods[i].UID {
				replaced++
			}
			if podReady(&pods[i]) {
				ready++
			}
		}
		return replaced >= 1 && ready >= len(before)
	})
}

// floor returns the highest committed height any node has right now. Call it
// just before a fault is applied and pass it to assertRecovered.
func (r *chaosRun) floor(ctx context.Context, t *testing.T) int64 {
	t.Helper()
	f, err := chaoscheck.Floor(ctx, r.hc, r.endpoints(ctx, t))
	if err != nil {
		t.Fatalf("read the pre-fault height: %v", err)
	}
	return f
}

// waitAllReady waits, within recoveryWindow, for every validator pod to be
// Ready. waitValidatorsReady only reports a timeout with t.Errorf and returns,
// so an unbounded call would spend the rest of the scenario budget before the
// checks after it ran.
func (r *chaosRun) waitAllReady(ctx context.Context, t *testing.T) {
	t.Helper()
	recCtx, cancel := context.WithTimeout(ctx, recoveryWindow)
	defer cancel()
	waitValidatorsReady(recCtx, t, r.cs, r.ns, r.id, extendedValidators)
}

// gateInjectedEver waits until chaos-mesh has injected the fault into at least
// one target, within injectWindow, and does not care whether the fault is still
// active. gateInjected waits for AllInjected to be True, but a fault shorter than
// the 5s poll can inject and recover between two reads and flip the condition
// back, which would fail a fault that worked. The per-target injectedCount
// stays on the CR after recovery, so this survives that.
func (r *chaosRun) gateInjectedEver(ctx context.Context, t *testing.T, f fault) {
	t.Helper()
	injectCtx, cancel := context.WithTimeout(ctx, injectWindow)
	defer cancel()
	pollUntil(injectCtx, t, fmt.Sprintf("%s/%s injected into a target", f.resource, f.obj.GetName()),
		func(ctx context.Context) bool {
			u, err := r.dc.Resource(chaosGVR(f.resource)).Namespace(r.ns).Get(ctx, f.obj.GetName(), metav1.GetOptions{})
			return err == nil && faultInjectedTargets(u) >= 1
		})
}

// assertRecovered is the shared recovery gate; it records failures with t.Errorf
// so every part runs and a scenario reports all of them. since is when the
// fault was applied, for the logged recovery time.
//
// Beyond what TestNightlyChaosSuite checks (validators Ready, follower caught
// up), it requires the follower to advance, and every validator and the
// follower to report the same committed state hash at several heights that are
// all above floor, the highest height any node had committed just before the
// fault. Requiring heights above the floor is what makes this a recovery
// check: a validator still replaying behind the fault never reports one, so the
// check cannot pass by comparing everyone at a height from before the fault.
//
// The hash is each node's own (see package chaoscheck), so it catches a node
// that recovered into different state, and a fork. It cannot catch a node that
// recovered into corrupt state and then stalled; the Ready and caught-up checks
// catch that one. The follower's caught-up check reads /status catching_up,
// which is not meaningful under Autobahn (it stays true with no consensus
// reactor); the chain this suite provisions never enables Autobahn.
func (r *chaosRun) assertRecovered(ctx context.Context, t *testing.T, since time.Time, floor int64) {
	t.Helper()
	followerRPC := r.follower().TendermintRPC()

	// Each part gets its own bound so a node that never catches up cannot use up
	// the scenario budget before the app-hash check, the gate's distinctive part.
	caughtCtx, cancelCaught := context.WithTimeout(ctx, 2*time.Minute)
	if err := sei.WaitCaughtUp(caughtCtx, r.hc, followerRPC); err != nil {
		t.Errorf("post-fault %s not caught up: %v", r.follower().Name(), err)
	}
	cancelCaught()
	advCtx, cancelAdv := context.WithTimeout(ctx, 2*time.Minute)
	if err := sei.WaitHeightAdvances(advCtx, r.hc, followerRPC, 3); err != nil {
		t.Errorf("post-fault %s height did not advance: %v", r.follower().Name(), err)
	}
	cancelAdv()

	agreeCtx, cancelAgree := context.WithTimeout(ctx, 3*time.Minute)
	defer cancelAgree()
	if err := chaoscheck.WaitAgreeAbove(agreeCtx, r.hc, r.endpoints(ctx, t), floor, agreeHeights); err != nil {
		t.Errorf("post-fault committed-state check: %v", err)
		return
	}
	t.Logf("recovered %s after fault: follower caught up and advancing, %d nodes agree on committed state above height %d",
		time.Since(since).Round(time.Second), extendedValidators+1, floor)
}

// scenarioKillDuringRecovery SIGKILLs validator-0 three times, each time right
// after its container is running again. The second and third kill land while
// the node is replaying from the state the previous kill left behind.
//
// "Mid-recovery" is best effort: the kill goes in as soon as the container is
// running, and the log records whether the pod was already Ready at that
// point. A fast recovery can finish before the next kill lands.
func scenarioKillDuringRecovery(ctx context.Context, t *testing.T, r *chaosRun) {
	const kills = 3
	start := time.Now()
	floor := r.floor(ctx, t)
	restarts := r.restartCounts(ctx, t)
	pod := r.nodePod(ctx, t, r.id+"-0") // validator-0

	for i := 1; i <= kills; i++ {
		before := restarts[pod]
		f := r.apply(ctx, t, "container-kill-validator-0", fmt.Sprintf("-k%d", i), "")
		r.gateInjectedExactly(ctx, t, f, 1)

		// The kill landed once the restart count rises; the next kill needs a
		// running container to hit, and the kubelet's restart back-off grows with
		// each crash, so wait for it rather than assume it.
		var readyAtRunning bool
		var seen int32
		// Bounded: a validator that falls into a real crash loop must fail within
		// recoveryWindow, not after the rest of the 40-minute scenario budget.
		killCtx, cancelKill := context.WithTimeout(ctx, recoveryWindow)
		pollUntil(killCtx, t, fmt.Sprintf("kill %d: %s seid restarted and running", i, pod), func(ctx context.Context) bool {
			n, running, ready := r.seidRestarts(ctx, pod)
			if n > before && running {
				readyAtRunning = ready
				seen = n
				return true
			}
			return false
		})
		cancelKill()
		// The observed count, not before+1: a crash loop or two restarts between
		// polls would otherwise let the next kill's wait pass on this restart.
		restarts[pod] = seen
		t.Logf("kill %d/%d landed on %s; container running again (pod Ready at that point: %v)",
			i, kills, pod, readyAtRunning)
	}

	// Each wait above saw the restart count rise, so Ready from here on is a real
	// recovery from the last kill, not a leftover from before it.
	r.waitAllReady(ctx, t)
	r.assertRecovered(ctx, t, start, floor)
}

// scenarioColdStart SIGKILLs every validator at once. No quorum survives, so
// the chain halts until enough validators recover from their own logs. Each
// recovers independently and can come back at a different height, which is the
// case that checks recovery agrees across nodes and not only on one.
func scenarioColdStart(ctx context.Context, t *testing.T, r *chaosRun) {
	start := time.Now()
	floor := r.floor(ctx, t)
	before := r.restartCounts(ctx, t)
	if len(before) != extendedValidators {
		t.Fatalf("want %d validator pods before the kill, got %d", extendedValidators, len(before))
	}
	f := r.apply(ctx, t, "container-kill-all", "", "")
	r.gateInjectedExactly(ctx, t, f, extendedValidators)
	t.Logf("container-kill-all injected: every validator killed at once")

	r.waitRestarted(ctx, t, before, extendedValidators)
	r.assertRecovered(ctx, t, start, floor)
}

// scenarioQuorumLossPartition splits the validators 2|2, with the RPC follower
// on one side. Neither side holds 2/3, so the chain must stop committing; once
// the partition lifts it must resume on a single history, and the committed
// state hashes agreeing afterwards is what would catch a fork.
//
// The follower is in the partition on purpose: it peers with every validator
// and relays consensus votes, so left outside it would bridge the split.
//
// Timing: the partition lasts quorumLossDuration from when the CR is created,
// and injection can take up to injectWindow before the stall check starts. The
// budget below covers injection, the settle, and the stall window, so the
// partition cannot lift while the check is still running and fail it on a
// healthy chain.
func scenarioQuorumLossPartition(ctx context.Context, t *testing.T, r *chaosRun) {
	if injectWindow+quorumSettle+quorumStallWindow >= quorumLossDuration {
		t.Fatalf("injectWindow (%s) + settle (%s) + stall window (%s) must be below the partition duration (%s)",
			injectWindow, quorumSettle, quorumStallWindow, quorumLossDuration)
	}
	// network_partition_split.yaml.tmpl names validators -0 .. -3 literally; a
	// different count would leave a validator outside both sides, relaying votes.
	if extendedValidators != 4 {
		t.Fatalf("network-partition-split names four validators; extendedValidators is %d", extendedValidators)
	}
	start := time.Now()
	floor := r.floor(ctx, t)
	f := r.applyWith(ctx, t, "network-partition-split", "", quorumLossDuration.String(),
		func(p *faults.Params) { p.Followers = r.followerNames() })
	gateInjected(ctx, t, r.dc, r.ns, f, false)
	t.Logf("network-partition-split injected: validators split 2|2, follower on one side, no side has quorum")

	// A block already in flight when the split lands may still commit, hence the
	// settle time and the one-block tolerance. Observed on validator-0, which is on
	// the first side: the follower is on that side too, but if it was lagging when
	// the split landed it could legitimately catch up more than a block from
	// validators -0 and -1 and fail a chain that did halt.
	if err := chaoscheck.HoldsHeight(
		ctx, r.hc, r.validatorRPC(ctx, t, 0), quorumSettle, quorumStallWindow, 1,
	); err != nil {
		t.Errorf("chain kept committing under a 2|2 split: %v", err)
	}

	gateRecovered(ctx, t, r.dc, r.ns, f)
	t.Logf("network-partition-split recovered")
	r.assertRecovered(ctx, t, start, floor)
}

// scenarioNetworkFlap partitions validator-0 from the rest, lifts it, and
// repeats. Each partition is the nightly's network-partition fault with a short
// duration. Peers must drop and re-establish connections five times without the
// chain losing liveness (f=1) or the node ending up in a bad state.
//
// The partition duration is below injectWindow on purpose, and a fault can inject
// and recover between two 5s polls, so each cycle gates on gateInjectedEver (the
// per-target injectedCount, which survives recovery) and not on AllInjected,
// which would flip back and fail a fault that worked.
func scenarioNetworkFlap(ctx context.Context, t *testing.T, r *chaosRun) {
	const (
		cycles = 5
		phase  = 20 * time.Second
	)
	start := time.Now()
	floor := r.floor(ctx, t)
	for i := 1; i <= cycles; i++ {
		f := r.apply(ctx, t, "network-partition", fmt.Sprintf("-flap%d", i), phase.String())
		r.gateInjectedEver(ctx, t, f)
		gateRecovered(ctx, t, r.dc, r.ns, f)

		select {
		case <-ctx.Done():
			t.Fatalf("network-flap cycle %d: %v", i, ctx.Err())
		case <-time.After(phase):
		}
		advCtx, cancel := context.WithTimeout(ctx, time.Minute)
		if err := sei.WaitHeightAdvances(advCtx, r.hc, r.follower().TendermintRPC(), 1); err != nil {
			t.Errorf("flap cycle %d: chain not advancing: %v", i, err)
		}
		cancel()
		t.Logf("flap cycle %d/%d done", i, cycles)
	}
	r.assertRecovered(ctx, t, start, floor)
}

// scenarioCleanStop deletes one validator pod with a grace period, so seid gets
// SIGTERM and time to shut down before any SIGKILL. It records seid's exit code
// from the terminating pod: 0 or 143 means it handled SIGTERM, 137 means the
// grace period ran out and the kubelet killed it. The code is a failure only
// when CHAOS_REQUIRE_CLEAN_SHUTDOWN=true.
func scenarioCleanStop(ctx context.Context, t *testing.T, r *chaosRun) {
	exits := r.watchSeidExits(ctx, t)
	before := r.podUIDs(ctx, t)
	if len(before) != extendedValidators {
		t.Fatalf("want %d validator pods before the stop, got %d", extendedValidators, len(before))
	}

	start := time.Now()
	floor := r.floor(ctx, t)
	f := r.apply(ctx, t, "pod-clean-stop", "", "")
	gateInjected(ctx, t, r.dc, r.ns, f, true)
	t.Logf("pod-clean-stop injected: one validator pod deleted with a grace period")

	r.waitReplaced(ctx, t, before)
	t.Logf("replacement pod Ready %s after the stop", time.Since(start).Round(time.Second))

	// The exit code is the clean-shutdown signal. Evaluate the replaced pod's.
	for uid, name := range r.replacedPods(ctx, t, before) {
		code, ok := exits.get(uid)
		switch {
		case !ok:
			msg := fmt.Sprintf("%s: seid exit code not observed (the pod left before a watch event carried it, "+
				"or the watch ended)", name)
			if requireCleanShutdown() {
				// An unobserved code must not read as a clean one.
				t.Errorf("%s", msg)
			} else {
				t.Logf("%s", msg)
			}
		case code == 137:
			msg := fmt.Sprintf("%s: seid exited 137 (SIGKILL): the grace period ran out before it shut down", name)
			if requireCleanShutdown() {
				t.Errorf("%s", msg)
			} else {
				t.Logf("%s (not a failure unless %s=true)", msg, chaosRequireCleanShutdownEnv)
			}
		default:
			t.Logf("%s: seid exited %d on SIGTERM", name, code)
		}
	}
	r.assertRecovered(ctx, t, start, floor)
}

// replacedPods maps the UID of each pod that was replaced to its name.
func (r *chaosRun) replacedPods(ctx context.Context, t *testing.T, before map[string]types.UID) map[types.UID]string {
	t.Helper()
	pods, err := r.validatorPods(ctx)
	if err != nil {
		t.Fatalf("list validator pods: %v", err)
	}
	out := map[types.UID]string{}
	for i := range pods {
		if old, ok := before[pods[i].Name]; ok && old != pods[i].UID {
			out[old] = pods[i].Name
		}
	}
	return out
}

// exitRecorder holds the last seid exit code seen for each pod UID.
type exitRecorder struct {
	mu    sync.Mutex
	codes map[types.UID]int32
}

func (e *exitRecorder) get(uid types.UID) (int32, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.codes[uid]
	return c, ok
}

func (e *exitRecorder) set(uid types.UID, code int32) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.codes[uid] = code
}

// watchSeidExits streams validator pod updates and records seid's exit code.
// A terminating pod's terminated state is visible only briefly before the pod
// object is removed, and polling would miss it, so this watches. It stops with
// ctx; a watch that fails to start leaves the recorder empty, and the caller
// logs that the code was not observed. The goroutine touches only the
// recorder, never t.
func (r *chaosRun) watchSeidExits(ctx context.Context, t *testing.T) *exitRecorder {
	t.Helper()
	rec := &exitRecorder{codes: map[types.UID]int32{}}
	w, err := r.cs.CoreV1().Pods(r.ns).Watch(ctx, metav1.ListOptions{LabelSelector: "sei.io/nodedeployment=" + r.id})
	if err != nil {
		t.Logf("pod watch unavailable, seid exit codes will not be recorded: %v", err)
		return rec
	}
	go func() {
		defer w.Stop()
		for ev := range w.ResultChan() {
			if ev.Type != watch.Modified && ev.Type != watch.Deleted {
				continue
			}
			p, ok := ev.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			if st, ok := seidStatus(p); ok && st.State.Terminated != nil {
				rec.set(p.UID, st.State.Terminated.ExitCode)
			}
		}
	}()
	return rec
}

// scenarioTimedFault runs a duration-bearing, f=1 fault from ExtendedCatalog the
// way the nightly suite runs its own: the chain must stay live while the fault
// is active, the fault must recover, and the shared recovery gate must pass.
func scenarioTimedFault(ctx context.Context, t *testing.T, r *chaosRun, name string) {
	if cf, err := faults.LookupExtended(name); err == nil && cf.QuorumLoss {
		t.Fatalf("fault %q removes quorum, so the under-fault liveness check would fail by design; "+
			"give it its own scenario", name)
	}
	start := time.Now()
	floor := r.floor(ctx, t)
	f := r.apply(ctx, t, name, "", r.duration)
	gateInjected(ctx, t, r.dc, r.ns, f, false)
	t.Logf("%s: fault injected", name)

	underFault, cancel := context.WithTimeout(ctx, r.faultDur*2/3)
	err := sei.WaitHeightAdvances(underFault, r.hc, r.follower().TendermintRPC(), 3)
	cancel()
	if err != nil {
		t.Errorf("under-fault %s height did not advance: %v", r.follower().Name(), err)
	}

	gateRecovered(ctx, t, r.dc, r.ns, f)
	t.Logf("%s: fault recovered", name)
	r.waitAllReady(ctx, t)
	r.assertRecovered(ctx, t, start, floor)
}
