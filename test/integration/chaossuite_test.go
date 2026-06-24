//go:build integration

package integration

import (
	"context"
	"net/http"
	"os/signal"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

// oneShotObserveWindow bounds the under-fault liveness check for kill faults,
// which carry no spec.duration to derive a window from. Sized for +3 blocks on
// the surviving validators, not for the killed pod's restart.
const oneShotObserveWindow = 90 * time.Second

// Chaos-Mesh fault GVR resource names (the dynamic client's plural form).
const (
	rNetworkChaos = "networkchaos"
	rStressChaos  = "stresschaos"
	rTimeChaos    = "timechaos"
	rIOChaos      = "iochaos"
	rPodChaos     = "podchaos"
)

// chaosScenario is one fault ported from the platform chaos suite: a name, the
// fault CR's GVR resource, and its template. oneShot marks a kill fault
// (pod-kill / container-kill) — no spec.duration and no AllRecovered, so it
// skips the self-expiry tripwire and the recovery gate.
type chaosScenario struct {
	name     string
	resource string
	tmpl     string
	oneShot  bool
}

// chaosScenarios is the ported fault set. Growing toward the platform suite's
// 14; each is added once it passes in-cluster.
var chaosScenarios = []chaosScenario{
	{name: "network-partition", resource: rNetworkChaos, tmpl: networkPartitionTmpl},
	{name: "packet-loss", resource: rNetworkChaos, tmpl: packetLossTmpl},
	{name: "cpu-stress", resource: rStressChaos, tmpl: cpuStressTmpl},
	{name: "time-skew", resource: rTimeChaos, tmpl: timeSkewTmpl},
	{name: "network-latency", resource: rNetworkChaos, tmpl: networkLatencyTmpl},
	{name: "bandwidth-limit", resource: rNetworkChaos, tmpl: bandwidthLimitTmpl},
	{name: "memory-stress", resource: rStressChaos, tmpl: memoryStressTmpl},
	{name: "disk-io-latency", resource: rIOChaos, tmpl: diskIOLatencyTmpl},
	{name: "byzantine", resource: rNetworkChaos, tmpl: byzantineTmpl},
	{name: "pod-failure", resource: rPodChaos, tmpl: podFailureTmpl, oneShot: true},
	{name: "container-kill", resource: rPodChaos, tmpl: containerKillTmpl, oneShot: true},
	// dns-chaos deferred: it's a rediscovery fault (live MConnections don't
	// re-resolve), so the under-fault progress assert can't perturb it — needs a
	// recovery-focused assert + peer-FQDN-matching patterns.
}

// TestChaosSuite runs each fault against its own fresh chain: provision → inject
// the Chaos-Mesh fault → gate it injected → assert the chain stays live under it
// (faults are bounded to f=1, so 2/3 quorum holds) → gate recovery → assert the
// chain reconverged. Each fault is a subtest so one failure doesn't abort the
// rest (matching the platform suite's continue-on-failure).
//
// Inputs (env): SEI_CHAIN_ID (base), SEID_IMAGE [required]; SEI_NAMESPACE,
// CHAOS_DURATION [optional]. Run with -test.timeout 0 (see TestBenchmark).
func TestChaosSuite(t *testing.T) {
	requireCluster(t)
	base := mustEnv(t, "SEI_CHAIN_ID")
	seid := mustEnv(t, "SEID_IMAGE")
	ns := envOr("SEI_NAMESPACE", "")
	duration := envOr("CHAOS_DURATION", "3m")
	faultDur, err := time.ParseDuration(duration)
	if err != nil {
		t.Fatalf("CHAOS_DURATION %q: %v", duration, err)
	}

	for _, sc := range chaosScenarios {
		t.Run(sc.name, func(t *testing.T) {
			id := base + "-" + sc.name
			// The validator-0 selector value sei.io/node=<id>-0 is a k8s label
			// value (capped at 63 chars); fail loud rather than on an opaque
			// admission rejection at fault-apply time.
			if v := id + "-0"; len(v) > 63 {
				t.Fatalf("chain id %q yields label value %q > 63 chars", id, v)
			}
			s := spec{
				chainID:       id,
				runID:         id,
				namespace:     ns,
				seidImage:     seid,
				validators:    4,
				rpcNodes:      1, // an unfaulted observer of liveness + recovery
				timeout:       40 * time.Minute,
				storageConfig: flatkvStorageConfig,
			}

			ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
			defer cancel()
			ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
			defer stop()

			c := openClient(ctx, t)
			dc := dynClient(t)
			cs := clientset(t)

			ch, err := provision(ctx, t, c, s)
			cleanupChain(t, ch)
			if err != nil {
				t.Fatalf("provision: %v", err)
			}
			faultNS := ch.network.Namespace()

			f := renderFault(t, sc.resource, sc.tmpl, faultParams{
				ChainID:   s.chainID,
				RunID:     s.runID,
				Namespace: faultNS,
				Duration:  duration,
			})
			// Duration-bearing faults must self-expire (else gateRecovered hangs
			// to the deadline). One-shot kills carry no duration by design.
			if !sc.oneShot && !f.hasDuration() {
				t.Fatalf("fault %s has no spec.duration — gateRecovered would hang until the deadline", sc.name)
			}
			applyFault(ctx, t, dc, faultNS, f)
			gateInjected(ctx, t, dc, faultNS, f, sc.oneShot)
			t.Logf("%s: fault injected", sc.name)

			hc := &http.Client{Timeout: 10 * time.Second}
			follower := ch.rpcNodes[0]
			// Live under fault: the chain must keep producing blocks while the
			// fault is active (f=1 holds 2/3 quorum). Assert the follower's height
			// advances within a window — observed while the fault is in effect, not
			// after it lifts. catching_up==false alone is insufficient: a stalled
			// node reports it at a frozen height.
			window := faultDur * 2 / 3
			if sc.oneShot {
				window = oneShotObserveWindow
			}
			underFault, cancelUF := context.WithTimeout(ctx, window)
			err = sei.WaitHeightAdvances(underFault, hc, follower.TendermintRPC(), 3)
			cancelUF()
			if err != nil {
				t.Errorf("under-fault %s height did not advance: %v", follower.Name(), err)
			}

			// Duration-bearing faults self-expire; gate recovery (catches stuck
			// finalizers). One-shot kills have no AllRecovered.
			if !sc.oneShot {
				gateRecovered(ctx, t, dc, faultNS, f)
				t.Logf("%s: fault recovered", sc.name)
			}
			// Recovery: every validator — including the faulted/killed one — must
			// return to Ready, not just the unfaulted survivors. The follower
			// caught-up check alone would green a chain whose killed validator
			// CrashLoops forever (f=1 keeps the survivors live). Mirrors the
			// platform chaos verify-cleanup.
			recCtx, cancelRec := context.WithTimeout(ctx, 5*time.Minute)
			waitValidatorsReady(recCtx, t, cs, faultNS, s.chainID, s.validators)
			cancelRec()
			if err := sei.WaitCaughtUp(ctx, hc, follower.TendermintRPC()); err != nil {
				t.Errorf("post-fault %s not caught up: %v", follower.Name(), err)
			}
		})
	}
}

// waitValidatorsReady blocks until at least want validator pods
// (sei.io/nodedeployment=chainID) report Ready — the recovery proof that the
// faulted/killed validator rejoined. A validator stuck CrashLooping after the
// fault fails here even though the survivors keep the chain live.
func waitValidatorsReady(ctx context.Context, t *testing.T, cs *kubernetes.Clientset, ns, chainID string, want int) {
	t.Helper()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		ready := 0
		pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "sei.io/nodedeployment=" + chainID})
		if err == nil {
			for i := range pods.Items {
				if podReady(&pods.Items[i]) {
					ready++
				}
			}
		}
		if ready >= want {
			return
		}
		select {
		case <-ctx.Done():
			t.Errorf("post-fault validators ready %d/%d before deadline: %v", ready, want, ctx.Err())
			return
		case <-tick.C:
		}
	}
}

// podReady reports whether the pod's Ready condition is True.
func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
