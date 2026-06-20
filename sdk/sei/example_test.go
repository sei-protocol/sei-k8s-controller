package sei_test

import (
	"context"
	"fmt"
	"time"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
	_ "github.com/sei-protocol/sei-k8s-controller/sdk/sei/provider/k8s" // mode opt-in; SEI_NODE_CLUSTER selects it
)

// Example_lifecycle is the SDK's payoff, compiling: create a network -> wait
// ready -> create N RPC nodes as peers -> wait ready -> use the endpoints ->
// delete. One Go call graph, one context, one error model — no jq seams, no URL
// reconstruction. It is a build-only example (no Output: directive); running it
// requires a real cluster. The caller owns every Delete — the SDK never
// auto-cleans.
func Example_lifecycle() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	c, err := sei.Open(ctx, "") // "" => env: SEI_NODE_CLUSTER present -> k8s
	if err != nil {
		fmt.Println(err)
		return
	}

	net, err := c.CreateNetwork(ctx, sei.NetworkSpec{
		Name: "chaos-net", Image: "ghcr.io/sei/sei-chain:abc123", Validators: 4,
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = net.Delete(ctx) }() // caller owns lifecycle

	if err := net.WaitReady(ctx); err != nil {
		fmt.Println(err) // sei.IsTimeout(err) distinguishes a deadline from a Failed phase
		return
	}

	// Create three RPC nodes peered to the network — the caller loops; the SDK
	// does not fan out.
	const rpcCount = 3
	nodes := make([]*sei.Node, 0, rpcCount)
	for i := range rpcCount {
		node, err := c.CreateNode(ctx, sei.NodeSpec{
			Name: fmt.Sprintf("rpc-%d", i), Network: net.Name(),
			Image: "ghcr.io/sei/sei-chain:abc123",
		})
		if err != nil {
			fmt.Println(err)
			return
		}
		defer func() { _ = node.Delete(ctx) }()
		nodes = append(nodes, node)
	}
	for _, node := range nodes {
		if err := node.WaitReady(ctx); err != nil {
			fmt.Println(err)
			return
		}
	}

	// Typed endpoints straight off .status — no jq, no <name>.<ns>.svc rebuild.
	if err := assertLiveness(ctx, nodes[0].EVMRPC()); err != nil {
		fmt.Println(err)
	}
}

// assertLiveness stands in for the harness's own block-height assertion.
func assertLiveness(_ context.Context, rpc string) error {
	if rpc == "" {
		return fmt.Errorf("empty rpc")
	}
	return nil
}
