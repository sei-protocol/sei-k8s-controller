package planner

import (
	"slices"
	"testing"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func TestConfigureStateSyncTask_PassesResolvedWitnesses(t *testing.T) {
	witnesses := []string{
		"syncer-0-0-0.syncer-0-0.arctic-1.svc.cluster.local:26657",
		"syncer-0-1-0.syncer-0-1.arctic-1.svc.cluster.local:26657",
	}
	node := &seiv1alpha1.SeiNode{
		Status: seiv1alpha1.SeiNodeStatus{ResolvedRPCWitnesses: witnesses},
	}
	snap := &seiv1alpha1.SnapshotSource{TrustPeriod: "168h0m0s", BackfillBlocks: 6000}

	task := configureStateSyncTask(node, snap)

	if !slices.Equal(task.RpcServers, witnesses) {
		t.Errorf("RpcServers = %v, want %v", task.RpcServers, witnesses)
	}
	if task.TrustPeriod != "168h0m0s" {
		t.Errorf("TrustPeriod = %q, want 168h0m0s", task.TrustPeriod)
	}
	if task.BackfillBlocks != 6000 {
		t.Errorf("BackfillBlocks = %d, want 6000", task.BackfillBlocks)
	}
}

// No resolved witnesses (e.g. EC2/static peers) leaves RpcServers empty so the
// sidecar falls back to deriving witnesses from persistent_peers.
func TestConfigureStateSyncTask_NoWitnessesLeavesEmpty(t *testing.T) {
	node := &seiv1alpha1.SeiNode{}
	task := configureStateSyncTask(node, nil)
	if len(task.RpcServers) != 0 {
		t.Errorf("RpcServers = %v, want empty", task.RpcServers)
	}
}
