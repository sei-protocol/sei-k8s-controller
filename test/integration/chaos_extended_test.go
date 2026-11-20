//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
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
)

const (
	// extendedValidators is the validator count of the chains these scenarios
	// provision. The splits and the kill-all assume four (2|2, quorum 3).
	extendedValidators = 4
	// agreeMargin pulls the app-hash comparison height back from the slowest
	// node's tip, so a node a block or two behind still has it.
	agreeMargin = 3
	// recoveryWindow bounds how long every validator has to come back Ready
	// after a fault, as in TestNightlyChaosSuite.
	recoveryWindow = 5 * time.Minute
	// seidContainer is the consensus container the kill faults target.
	seidContainer = "seid"
)

// extendedScenario is one TestChaosExtended subtest. Each runs on its own fresh
// chain, as the nightly suite's faults do, so one scenario's damage cannot
// leak into the next.
type extendedScenario struct {
	name string
	run  func(ctx context.Context, t *testing.T, r *chaosRun)
}

var extendedScenarios = []extendedScenario{
	{"kill-during-recovery", scenarioKillDuringRecovery},
	{"cold-start", scenarioColdStart},
	{"quorum-loss-partition", scenarioQuorumLossPartition},
	{"network-flap", scenarioNetworkFlap},
	{"clean-stop", scenarioCleanStop},
	{"time-skew-forward", func(ctx context.Context, t *testing.T, r *chaosRun) {
		scenarioTimedFault(ctx, t, r, "time-skew-forward")
	}},
	{"time-skew-large", func(ctx context.Context, t *testing.T, r *chaosRun) {
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
//   - cold-start: SIGKILL every validator at once. Each recovers on its own and
//     they can come back at different heights; the chain has to reconcile.
//   - quorum-loss-partition: split the validators 2|2. The chain must STOP (no
//     side has 2/3), then resume without forking. Every f=1 fault tests
//     liveness; this tests the safety side.
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
// validator and the follower agreeing on the app hash at a common height. A
// node that came back with a corrupted store reports Ready and keeps up, but
// its hash differs.
//
// Inputs (env): SEI_CHAIN_ID (base), SEID_IMAGE_CHAOS [required]; SEI_NAMESPACE,
// CHAOS_DURATION, CHAOS_EXTENDED_SCENARIOS, CHAOS_STORAGE_PROFILE,
// CHAOS_REQUIRE_CLEAN_SHUTDOWN [optional]. Run with -test.timeout 0.
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

	for _, sc := range extendedScenarios {
		if !selected(sc.name) {
			continue
		}
		t.Run(sc.name, func(t *testing.T) {
			id := base + "-" + sc.name
			// Selector values are k8s labels, capped at 63 characters; the longest
			// one derived from the id is the per-fault run id (id + "-flap5").
			if longest := id + "-flap5"; len(longest) > 63 {
				t.Fatalf("chain id %q yields label value %q > 63 chars", id, longest)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
			defer cancel()

			c := openClient(ctx, t)
			cs := clientset(t)
			ch, err := provision(ctx, t, c, spec{
				chainID:       id,
				runID:         id,
				namespace:     ns,
				seidImage:     seid,
				validators:    extendedValidators,
				rpcNodes:      1, // an unfaulted observer of liveness + recovery
				timeout:       40 * time.Minute,
				storageConfig: storage,
			})
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
			}
			sc.run(ctx, t, r)
		})
	}
}

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
}

func (r *chaosRun) follower() *sei.Node { return r.ch.rpcNodes[0] }

// endpoints returns the Tendermint RPC of every validator and the follower, so
// the app-hash check compares all of them.
func (r *chaosRun) endpoints(ctx context.Context, t *testing.T) []string {
	t.Helper()
	var urls []string
	for i := range extendedValidators {
		name := fmt.Sprintf("%s-%d", r.id, i)
		n, err := r.c.GetNode(ctx, name, r.ns)
		if err != nil {
			t.Fatalf("get validator %s: %v", name, err)
		}
		u := n.TendermintRPC()
		if u == "" {
			t.Fatalf("validator %s has no Tendermint RPC endpoint in its status", name)
		}
		urls = append(urls, u)
	}
	return append(urls, r.follower().TendermintRPC())
}

// apply renders and applies one fault. suffix keeps the CR name and run label
// unique when a scenario applies the same fault more than once. The fault comes
// from ExtendedCatalog, or the nightly Catalog for network-flap.
func (r *chaosRun) apply(ctx context.Context, t *testing.T, name, suffix, duration string) fault {
	t.Helper()
	cf, extErr := faults.LookupExtended(name)
	if extErr != nil {
		var nightlyErr error
		if cf, nightlyErr = faults.Lookup(name); nightlyErr != nil {
			t.Fatalf("lookup fault %q: %v; %v", name, extErr, nightlyErr)
		}
	}
	f := renderFault(t, cf, faults.Params{
		ChainID:   r.id,
		RunID:     r.id + suffix,
		Namespace: r.ns,
		Duration:  duration,
	})
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

// poll calls done every 2s until it returns true or ctx ends, failing the test
// on timeout with what names the wait.
func poll(ctx context.Context, t *testing.T, what string, done func(context.Context) bool) {
	t.Helper()
	tick := time.NewTicker(2 * time.Second)
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
	poll(recCtx, t, fmt.Sprintf("%d validator(s) restarted and Ready", killed), func(ctx context.Context) bool {
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
	poll(recCtx, t, "a validator pod replaced and every validator Ready", func(ctx context.Context) bool {
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

// assertRecovered is the shared recovery gate; it records failures with t.Errorf
// so every part runs and a scenario reports all of them. since is when the
// fault was applied, for the logged recovery time.
//
// Beyond what TestNightlyChaosSuite checks (validators Ready, follower caught
// up), it requires the follower to advance and every node to agree on the app
// hash. Agreement is what a recovery bug that corrupts one node's store
// breaks first: that node stays Ready and keeps up, but computes a different
// state.
func (r *chaosRun) assertRecovered(ctx context.Context, t *testing.T, since time.Time) {
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
	if err := chaoscheck.WaitAgree(agreeCtx, r.hc, r.endpoints(ctx, t), agreeMargin); err != nil {
		t.Errorf("post-fault app hash check: %v", err)
		return
	}
	t.Logf("recovered %s after fault: validators Ready, follower caught up and advancing, app hash agrees across %d nodes",
		time.Since(since).Round(time.Second), extendedValidators+1)
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
	restarts := r.restartCounts(ctx, t)
	pod := r.validator0Pod(ctx, t)

	for i := 1; i <= kills; i++ {
		before := restarts[pod]
		f := r.apply(ctx, t, "container-kill-validator-0", fmt.Sprintf("-k%d", i), "")
		r.gateInjectedExactly(ctx, t, f, 1)

		// The kill landed once the restart count rises; the next kill needs a
		// running container to hit, and the kubelet's restart back-off grows with
		// each crash, so wait for it rather than assume it.
		var readyAtRunning bool
		var seen int32
		poll(ctx, t, fmt.Sprintf("kill %d: %s seid restarted and running", i, pod), func(ctx context.Context) bool {
			p, err := r.cs.CoreV1().Pods(r.ns).Get(ctx, pod, metav1.GetOptions{})
			if err != nil {
				return false
			}
			st, ok := seidStatus(p)
			if ok && st.RestartCount > before && st.State.Running != nil {
				readyAtRunning = podReady(p)
				seen = st.RestartCount
				return true
			}
			return false
		})
		// The observed count, not before+1: a crash loop or two restarts between
		// polls would otherwise let the next kill's wait pass on this restart.
		restarts[pod] = seen
		t.Logf("kill %d/%d landed on %s; container running again (pod Ready at that point: %v)",
			i, kills, pod, readyAtRunning)
	}

	// Each wait above saw the restart count rise, so Ready from here on is a real
	// recovery from the last kill, not a leftover from before it.
	waitValidatorsReady(ctx, t, r.cs, r.ns, r.id, extendedValidators)
	r.assertRecovered(ctx, t, start)
}

// validator0Pod names validator-0's pod.
func (r *chaosRun) validator0Pod(ctx context.Context, t *testing.T) string {
	t.Helper()
	l, err := r.cs.CoreV1().Pods(r.ns).List(ctx, metav1.ListOptions{LabelSelector: "sei.io/node=" + r.id + "-0"})
	if err != nil || len(l.Items) != 1 {
		t.Fatalf("want exactly one pod for validator-0, got %d (err: %v)", len(l.Items), err)
	}
	return l.Items[0].Name
}

// scenarioColdStart SIGKILLs every validator at once. No quorum survives, so
// the chain halts until enough validators recover from their own logs. Each
// recovers independently and can come back at a different height, which is the
// case that checks recovery agrees across nodes and not only on one.
func scenarioColdStart(ctx context.Context, t *testing.T, r *chaosRun) {
	start := time.Now()
	before := r.restartCounts(ctx, t)
	if len(before) != extendedValidators {
		t.Fatalf("want %d validator pods before the kill, got %d", extendedValidators, len(before))
	}
	f := r.apply(ctx, t, "container-kill-all", "", "")
	r.gateInjectedExactly(ctx, t, f, extendedValidators)
	t.Logf("container-kill-all injected: every validator killed at once")

	r.waitRestarted(ctx, t, before, extendedValidators)
	r.assertRecovered(ctx, t, start)
}

// scenarioQuorumLossPartition splits the validators 2|2. Neither side holds
// 2/3, so the chain must stop committing; once the partition lifts it must
// resume on a single history. The app-hash check at the end is what proves it
// did not fork.
func scenarioQuorumLossPartition(ctx context.Context, t *testing.T, r *chaosRun) {
	const settle = 15 * time.Second
	window := r.faultDur / 2
	if settle+window >= r.faultDur {
		t.Fatalf("settle (%s) + stall window (%s) must be below CHAOS_DURATION (%s)", settle, window, r.faultDur)
	}
	start := time.Now()
	f := r.apply(ctx, t, "network-partition-split", "", r.duration)
	gateInjected(ctx, t, r.dc, r.ns, f, false)
	t.Logf("network-partition-split injected: validators split 2|2, no side has quorum")

	// A block already in flight when the split lands may still commit, hence the
	// settle time and the one-block tolerance.
	if err := chaoscheck.HoldsHeight(ctx, r.hc, r.follower().TendermintRPC(), settle, window, 1); err != nil {
		t.Errorf("chain kept committing under a 2|2 split: %v", err)
	}

	gateRecovered(ctx, t, r.dc, r.ns, f)
	t.Logf("network-partition-split recovered")
	r.assertRecovered(ctx, t, start)
}

// scenarioNetworkFlap partitions validator-0 from the rest, lifts it, and
// repeats. Each partition is the nightly's network-partition fault with a short
// duration. Peers must drop and re-establish connections five times without the
// chain losing liveness (f=1) or the node ending up in a bad state.
//
// The partition duration is below injectWindow on purpose, so a fault that is
// injected late can expire before gateInjected returns; the end-state gate, not
// the per-cycle liveness check, is what this scenario relies on.
func scenarioNetworkFlap(ctx context.Context, t *testing.T, r *chaosRun) {
	const (
		cycles = 5
		phase  = 20 * time.Second
	)
	start := time.Now()
	for i := 1; i <= cycles; i++ {
		f := r.apply(ctx, t, "network-partition", fmt.Sprintf("-flap%d", i), phase.String())
		gateInjected(ctx, t, r.dc, r.ns, f, false)
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
	r.assertRecovered(ctx, t, start)
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
			t.Logf("%s: seid exit code not observed (the pod left before a watch event carried it)", name)
		case code == 137:
			msg := fmt.Sprintf("%s: seid exited 137 (SIGKILL): the grace period ran out before it shut down", name)
			if envOr(chaosRequireCleanShutdownEnv, "") == "true" {
				t.Errorf("%s", msg)
			} else {
				t.Logf("%s (not a failure unless %s=true)", msg, chaosRequireCleanShutdownEnv)
			}
		default:
			t.Logf("%s: seid exited %d on SIGTERM", name, code)
		}
	}
	r.assertRecovered(ctx, t, start)
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
	waitValidatorsReady(ctx, t, r.cs, r.ns, r.id, extendedValidators)
	r.assertRecovered(ctx, t, start)
}
