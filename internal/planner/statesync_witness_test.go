package planner

import (
	"slices"
	"testing"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func TestConfigureStateSyncTask_PassesCanonicalSyncers(t *testing.T) {
	syncers := []string{
		"syncer-0.arctic-1.example.com:26657",
		"syncer-1.arctic-1.example.com:26657",
	}
	node := &seiv1alpha1.SeiNode{
		Spec: seiv1alpha1.SeiNodeSpec{
			FullNode: &seiv1alpha1.FullNodeSpec{
				Snapshot: &seiv1alpha1.SnapshotSource{TrustPeriod: "168h0m0s", BackfillBlocks: 6000},
			},
		},
		Status: seiv1alpha1.SeiNodeStatus{ResolvedStateSyncers: syncers},
	}

	task := configureStateSyncTask(node)

	if !slices.Equal(task.RpcServers, syncers) {
		t.Errorf("RpcServers = %v, want %v", task.RpcServers, syncers)
	}
	if task.TrustPeriod != "168h0m0s" {
		t.Errorf("TrustPeriod = %q, want 168h0m0s", task.TrustPeriod)
	}
	if task.BackfillBlocks != 6000 {
		t.Errorf("BackfillBlocks = %d, want 6000", task.BackfillBlocks)
	}
}

// No resolved syncers leaves RpcServers empty. In production the StateSyncReady
// gate fails closed before this task is reached when <2 syncers are configured;
// this guards the builder's nil-safety regardless.
func TestConfigureStateSyncTask_NoSyncersLeavesEmpty(t *testing.T) {
	node := &seiv1alpha1.SeiNode{}
	task := configureStateSyncTask(node)
	if len(task.RpcServers) != 0 {
		t.Errorf("RpcServers = %v, want empty", task.RpcServers)
	}
}
