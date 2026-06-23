//go:build integration

// Package integration holds the Sei nightly integration suites as plain `go test`
// targets (TestBenchmark, TestChaosSuite, TestChainUpgrade, TestRelease), selected
// with -run. Orchestration is statement order in one process; cross-step state is
// local Go values, not external config.
//
// Everything lives in *_test.go behind //go:build integration, so it never links
// into a production binary and is excluded from the default `go test ./...`. The
// nightly compiles it once (go test -c -tags integration) and runs each target as
// an in-cluster CronJob (-test.run TestX).
//
// Depends only on sdk/sei (+ the k8s provider blank import); never internal/seitask
// or internal/taskruntime.
package integration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"

	// Register the k8s provisioning provider for the SDK (database/sql-style
	// blank import; sei.Open(ctx, "k8s") needs it present).
	_ "github.com/sei-protocol/sei-k8s-controller/sdk/sei/provider/k8s"
)

// runLabelKey marks a run's resources for the nightly label-GC sweep — the only
// reaper on abnormal exit (shared namespace), since t.Cleanup is skipped on
// SIGKILL or a -test.timeout breach. provision stamps it on the network + every
// node; a suite's directly-applied seiload Job and fault CRs must stamp it too.
const runLabelKey = "sei.io/harness-run"

// spec is the typed input shared by the suites — the local-Go-state replacement
// for the per-run workflow-vars contract.
type spec struct {
	chainID    string        // SeiNetwork name == genesis chain id; also the peer-selector value and per-run discriminator
	runID      string        // unique per run; the sei.io/harness-run label value
	namespace  string        // shared nightly namespace (D2); "" => SDK client default (SA namespace)
	seidImage  string        // seid container image under test
	validators int           // genesis validator count (>= 1)
	rpcNodes   int           // standalone RPC followers; named <chain>-rpc-0..N-1
	timeout    time.Duration // overall scenario deadline (drives ctx, kept < CronJob activeDeadlineSeconds)

	// seiload inputs (load suite)
	seiloadImage   string // sei-load benchmark image
	seiloadProfile string // profile name in the seiload-profiles ConfigMap
	seiloadCommit  string // sei-chain commit label for the run's metrics
	durationMin    int    // seiload run length, minutes
}

// chain is the live provisioned topology a suite runs load against and asserts
// on, held in local Go state.
type chain struct {
	network  *sei.Network
	rpcNodes []*sei.Node
}

// evmEndpoints returns the per-follower EVM JSON-RPC URLs that seiload's profile
// fans its workload across.
func (ch *chain) evmEndpoints() []string {
	urls := make([]string, 0, len(ch.rpcNodes))
	for _, n := range ch.rpcNodes {
		urls = append(urls, n.EVMRPC())
	}
	return urls
}

// rpcNodeName is the load-bearing selector contract: chaos fault CRs target
// followers by sei.io/node=<chain>-rpc-<ordinal>. The SDK NodeSpec has no
// Replicas field (the caller loops CreateNode), so the suite reproduces the
// <base>-<ordinal> naming the old `provision-node --replicas` produced.
func rpcNodeName(chainID string, ordinal int) string {
	return fmt.Sprintf("%s-rpc-%d", chainID, ordinal)
}

// provision stands up the genesis SeiNetwork + N standalone RPC SeiNodes via the
// SDK in-process (not a seictl subprocess), waiting each follower
// to Running + caught-up + EVM-serving before returning. The returned chain is
// non-nil even on error so the caller can still tear down whatever was created
// (pair every provision with a t.Cleanup(teardown)).
func provision(ctx context.Context, t *testing.T, c *sei.Client, s spec) (*chain, error) {
	t.Helper()
	ch := &chain{}

	// Stamped on the network + every node so the label-GC sweep can reap this
	// run's resources on an abnormal exit t.Cleanup can't cover.
	runLabels := map[string]string{runLabelKey: s.runID}

	net, err := c.CreateNetwork(ctx, sei.NetworkSpec{
		Name:       s.chainID,
		Namespace:  s.namespace,
		Image:      s.seidImage,
		Validators: s.validators,
		Labels:     runLabels,
		// Ephemeral chain: cascade-delete the controller-created validators (+
		// their PVCs) on teardown. The CRD default Retain would orphan them —
		// they never carry sei.io/harness-run, so neither t.Cleanup nor the
		// label-GC sweep would reap them.
		DeletionPolicy: sei.DeletionDelete,
	})
	if err != nil {
		return ch, fmt.Errorf("create network %q: %w", s.chainID, err)
	}
	ch.network = net
	if err := net.WaitReady(ctx); err != nil {
		return ch, fmt.Errorf("network %q ready: %w", s.chainID, err)
	}
	t.Logf("network %s: ready", s.chainID)

	hc := &http.Client{Timeout: 10 * time.Second}
	for i := range s.rpcNodes {
		name := rpcNodeName(s.chainID, i)
		node, err := c.CreateNode(ctx, sei.NodeSpec{
			Name:      name,
			Network:   s.chainID,
			Namespace: s.namespace,
			Image:     s.seidImage,
			Labels:    runLabels,
		})
		if err != nil {
			return ch, fmt.Errorf("create rpc node %q: %w", name, err)
		}
		ch.rpcNodes = append(ch.rpcNodes, node)

		// Per-gate progress so a stall is localizable in real time from the pod
		// log (which node, which gate) rather than a single terminal error.
		if err := node.WaitReady(ctx); err != nil {
			return ch, fmt.Errorf("rpc node %q running: %w", name, err)
		}
		t.Logf("rpc node %s: running", name)
		if err := sei.WaitCaughtUp(ctx, hc, node.TendermintRPC()); err != nil {
			return ch, fmt.Errorf("rpc node %q caught up: %w", name, err)
		}
		t.Logf("rpc node %s: caught up", name)
		if err := sei.WaitEVMServing(ctx, hc, node.EVMRPC()); err != nil {
			return ch, fmt.Errorf("rpc node %q EVM serving: %w", name, err)
		}
		t.Logf("rpc node %s: EVM serving", name)
	}
	return ch, nil
}

// teardown deletes the provisioned resources — the normal-exit fast path, wired
// via t.Cleanup. Best-effort and idempotent (the SDK treats not-found as
// success): every delete is attempted and errors joined, so one failure never
// strands the rest. The harness-run label sweep is the backstop when this never
// runs (SIGKILL).
func (ch *chain) teardown(ctx context.Context) error {
	var errs []error
	for _, n := range ch.rpcNodes {
		if n == nil {
			continue
		}
		if err := n.Delete(ctx); err != nil {
			errs = append(errs, fmt.Errorf("delete node %q: %w", n.Name(), err))
		}
	}
	if ch.network != nil {
		if err := ch.network.Delete(ctx); err != nil {
			errs = append(errs, fmt.Errorf("delete network %q: %w", ch.network.Name(), err))
		}
	}
	return errors.Join(errs...)
}

// cleanupChain registers best-effort teardown on a fresh context, so it still
// runs when the scenario ctx is already expired.
func cleanupChain(t *testing.T, ch *chain) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := ch.teardown(ctx); err != nil {
			t.Errorf("teardown: %v", err)
		}
	})
}

// requireCluster skips a suite unless it is pointed at a real cluster. The SDK
// selects its k8s provider on SEI_NODE_CLUSTER presence; without it there is
// nothing to provision against.
func requireCluster(t *testing.T) {
	t.Helper()
	if os.Getenv("SEI_NODE_CLUSTER") == "" {
		t.Skip("integration suite: set SEI_NODE_CLUSTER to run against a cluster")
	}
}

// openClient opens the SDK in k8s mode (config resolved from the ambient
// kubeconfig / in-cluster SA chain).
func openClient(ctx context.Context, t *testing.T) *sei.Client {
	t.Helper()
	c, err := sei.Open(ctx, "k8s")
	if err != nil {
		t.Fatalf("open sei SDK (k8s): %v", err)
	}
	return c
}

// envOr returns the env var or a fallback (for local runs).
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// mustEnv fails the suite if a required input is missing.
func mustEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Fatalf("integration suite: required env %s is unset", key)
	}
	return v
}

// envInt reads an integer env var or a fallback; a non-integer value fails fast.
func envInt(t *testing.T, key string, fallback int) int {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("integration suite: env %s=%q is not an integer: %v", key, v, err)
	}
	return n
}
