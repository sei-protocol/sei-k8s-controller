//go:build chaos

package chaos

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"

	_ "github.com/sei-protocol/sei-k8s-controller/sdk/sei/provider/k8s" // k8s mode
)

// TestNightlyLoad is the nightly load suite: stand up a genesis SeiNetwork + N
// RPC SeiNodes via the SDK, wait until the followers are caught up, run the
// seiload payload against their EVM endpoints, and tear everything down. It is
// the Go-native replacement for platform's k8s_nightly.yml. Run it with:
//
//	SEI_CHAOS_CLUSTER=1 SEID_IMAGE=... go test -tags chaos -run TestNightlyLoad -timeout 90m -count=1 ./test/chaos/
func TestNightlyLoad(t *testing.T) {
	cfg := loadConfig(t)
	ctx := context.Background()

	c, err := sei.Open(ctx, "k8s")
	if err != nil {
		t.Fatalf("open SDK (k8s mode): %v", err)
	}

	// Genesis validator pool.
	net, err := c.CreateNetwork(ctx, sei.NetworkSpec{
		Name:       cfg.ChainID,
		Namespace:  cfg.Namespace,
		Image:      cfg.SeidImage,
		Validators: cfg.ValidatorCount,
	})
	if err != nil {
		t.Fatalf("create network %q: %v", cfg.ChainID, err)
	}
	t.Cleanup(func() { teardown(t, "network "+net.Name(), net.Delete) })

	readyCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	if err := net.WaitReady(readyCtx); err != nil {
		t.Fatalf("network %q not Ready: %v", net.Name(), err)
	}

	// RPC followers: N standalone SeiNodes peered to the genesis pool. The SDK
	// stamps the network label + synthesizes the peer selector from NodeSpec.Network.
	nodes := make([]*sei.Node, 0, cfg.RPCCount)
	for i := 0; i < cfg.RPCCount; i++ {
		name := fmt.Sprintf("%s-rpc-%d", cfg.ChainID, i)
		node, err := c.CreateNode(ctx, sei.NodeSpec{
			Name:      name,
			Network:   cfg.ChainID,
			Namespace: cfg.Namespace,
			Image:     cfg.SeidImage,
		})
		if err != nil {
			t.Fatalf("create node %q: %v", name, err)
		}
		t.Cleanup(func() { teardown(t, "node "+node.Name(), node.Delete) })
		nodes = append(nodes, node)
	}

	// Wait Running, then gate on the strict caught-up contract per follower.
	for _, node := range nodes {
		runCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
		if err := node.WaitReady(runCtx); err != nil {
			cancel()
			t.Fatalf("node %q not Running: %v", node.Name(), err)
		}
		if err := sei.WaitCaughtUp(runCtx, nil, node.TendermintRPC()); err != nil {
			cancel()
			t.Fatalf("node %q: %v", node.Name(), err)
		}
		cancel()
	}

	// Collect EVM endpoints verbatim off .status (never reconstructed) for the load payload.
	endpoints := make([]string, 0, len(nodes))
	for _, node := range nodes {
		ep := node.EVMRPC()
		if ep == "" {
			t.Fatalf("node %q Running but .status.endpoint.evmJsonRpc empty", node.Name())
		}
		endpoints = append(endpoints, ep)
	}

	if err := runSeiloadSuite(ctx, t, cfg, endpoints); err != nil {
		t.Fatalf("seiload suite: %v", err)
	}
}

// teardown deletes a resource on test cleanup, logging rather than failing — the
// nightly-gc CronJob is the defense-in-depth sweep, and a Cleanup that t.Fatal'd
// would mask the real failure. Replaces the workflow's `if: always()` teardown.
func teardown(t *testing.T, what string, del func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := del(ctx); err != nil {
		t.Logf("teardown %s: %v (gc CronJob will sweep)", what, err)
	}
}
