package sei

import "testing"

// Shared fixture URLs for the sei package endpoint tests.
const (
	rpc0URL = "http://rpc-0:8545"
	rpc2URL = "http://rpc-2:8545"
)

// TestFleetEndpoints_EVMRPCListPreservesFleetOrder pins the D7 contract:
// EVMRPCList yields per-node URLs in fleet (ordinal) order, with empty-EVM nodes
// skipped but the relative order of the survivors unchanged. A harness indexes
// this list by ordinal, so a reorder is a silent correctness break.
func TestFleetEndpoints_EVMRPCListPreservesFleetOrder(t *testing.T) {
	fe := FleetEndpoints{Nodes: []FleetNodeEndpoint{
		{Name: "rpc-0", EvmJsonRPC: rpc0URL},
		{Name: "rpc-1"}, // lost endpoint: skipped, must not reorder the rest
		{Name: "rpc-2", EvmJsonRPC: rpc2URL},
		{Name: "rpc-3", EvmJsonRPC: "http://rpc-3:8545"},
	}}
	got := fe.EVMRPCList()
	want := []string{rpc0URL, rpc2URL, "http://rpc-3:8545"}
	if len(got) != len(want) {
		t.Fatalf("EVMRPCList len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("EVMRPCList[%d] = %q, want %q (fleet order broke)", i, got[i], want[i])
		}
	}
}
