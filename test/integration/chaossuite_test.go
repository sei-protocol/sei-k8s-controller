//go:build integration

package integration

import (
	"context"
	"net/http"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

// chaosScenario is one fault ported from the platform chaos suite: a name, the
// fault CR's GVR resource, and its template.
type chaosScenario struct {
	name     string
	resource string
	tmpl     string
}

// chaosScenarios is the ported fault set. Growing toward the platform suite's
// 14; each is added once it passes in-cluster.
var chaosScenarios = []chaosScenario{
	{name: "network-partition", resource: "networkchaos", tmpl: networkPartitionTmpl},
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

	for _, sc := range chaosScenarios {
		t.Run(sc.name, func(t *testing.T) {
			id := base + "-" + sc.name
			s := spec{
				chainID:       id,
				runID:         id,
				namespace:     ns,
				seidImage:     seid,
				validators:    4,
				rpcNodes:      1, // an unfaulted observer of liveness + recovery
				timeout:       40 * time.Minute,
				storageConfig: memiavlStorageConfig,
			}

			ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
			defer cancel()
			ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
			defer stop()

			c := openClient(ctx, t)
			dc := dynClient(t)

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
			applyFault(ctx, t, dc, faultNS, f)
			gateInjected(ctx, t, dc, faultNS, f)
			t.Logf("%s: fault injected", sc.name)

			hc := &http.Client{Timeout: 10 * time.Second}
			follower := ch.rpcNodes[0]
			// Live under fault: with f=1 the chain keeps 2/3 quorum, so the
			// unfaulted follower must stay caught up while the fault is active.
			if err := sei.WaitCaughtUp(ctx, hc, follower.TendermintRPC()); err != nil {
				t.Errorf("under-fault %s not caught up: %v", follower.Name(), err)
			}

			// The fault self-expires after its duration; gate recovery (catches
			// stuck finalizers) then confirm the chain reconverged.
			gateRecovered(ctx, t, dc, faultNS, f)
			t.Logf("%s: fault recovered", sc.name)
			if err := sei.WaitCaughtUp(ctx, hc, follower.TendermintRPC()); err != nil {
				t.Errorf("post-fault %s not caught up: %v", follower.Name(), err)
			}
		})
	}
}
