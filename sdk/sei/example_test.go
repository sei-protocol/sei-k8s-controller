package sei_test

import (
	"context"
	"fmt"
	"time"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
	_ "github.com/sei-protocol/sei-k8s-controller/sdk/sei/provider/k8s" // flavor opt-in; SEI_NODE_CLUSTER selects it
)

// Example_singleHarness is the WS-E §7 payoff, compiling: the k8s_nightly
// pipeline (provision network -> provision fleet -> read endpoints -> assert ->
// teardown) as one Go call graph sharing one context and one error model, with
// no jq seams or URL reconstruction. It is a build-only example (no Output:
// directive) — running it requires a real cluster.
func Example_singleHarness() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	c, err := sei.Open(ctx, "") // "" => env: SEI_NODE_CLUSTER present -> k8s (no dsn)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = c.Close() }()

	net, err := c.ProvisionNetwork(ctx, sei.NetworkSpec{
		Name: "chaos-net", Preset: "genesis-chain",
		ChainID: "sei-chaos-1", Image: "ghcr.io/sei/sei-chain:abc123", Replicas: 4,
	})
	if err != nil {
		fmt.Println(err) // typed Timeout/Failed, branchable via sei.IsTimeout/IsFailed
		return
	}
	defer func() { _ = net.Teardown(ctx) }()

	fleet, err := c.ProvisionFleet(ctx, net, sei.FleetSpec{
		NamePrefix: "rpc", Preset: "rpc", Replicas: 3, Image: "ghcr.io/sei/sei-chain:abc123",
	})
	if err != nil {
		fmt.Println(err) // all Running AND probe-passed (consensus, not just liveness)
		return
	}
	defer func() { _ = fleet.Teardown(ctx) }()

	// Typed endpoints straight off .status — no jq, no <name>.<ns>.svc rebuild.
	rpcs := fleet.Endpoints().EVMRPCList()
	if len(rpcs) == 0 {
		fmt.Println("no rpc endpoints")
		return
	}

	// A harness's own load/assert helpers live in the same package, sharing ctx
	// and the SDK's structured errors — the in-Go-test path the SDK enables.
	if err := assertLiveness(ctx, rpcs[0]); err != nil {
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
