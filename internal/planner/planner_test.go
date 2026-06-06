package planner

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

// The init plan for a label-only node with empty status.resolvedPeers must
// still carry a discover-peers step, but as the init-aware task type with NO
// frozen params — it re-derives sources from the live node each reconcile and
// defers (never submits a zero-source write) until reconcilePeers populates
// resolvedPeers. Freezing empty sources at plan-build time would defeat that.
func TestBuildBasePlan_LabelPeersEmptyResolved_InitDiscoverPeersNilParams(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "label-node", Namespace: "label-ns"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  "label-chain",
			Image:    "sei:v1",
			FullNode: &seiv1alpha1.FullNodeSpec{},
			Peers: []seiv1alpha1.PeerSource{
				{Label: &seiv1alpha1.LabelPeerSource{Selector: map[string]string{"sei.io/chain": "label-chain"}}},
			},
		},
		// No status.resolvedPeers — label not yet resolved.
	}

	plan, err := (&fullNodePlanner{}).BuildPlan(node)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	var found *seiv1alpha1.PlannedTask
	for i := range plan.Tasks {
		if plan.Tasks[i].Type == task.TaskTypeDiscoverPeersInit {
			found = &plan.Tasks[i]
		}
	}
	if found == nil {
		t.Fatalf("init plan must contain %q; got %v", task.TaskTypeDiscoverPeersInit, planTaskTypes(plan))
	}
	// No frozen sources: the init task re-derives them from the live node, so
	// the persisted params are empty (marshal of nil → "null"), never a
	// DiscoverPeersTask with stale/empty sources.
	if found.Params != nil && string(found.Params.Raw) != "null" {
		t.Errorf("init discover-peers must not freeze sources at plan-build time, got %s", string(found.Params.Raw))
	}
	if found.MaxRetries != discoverPeersMaxRetries {
		t.Errorf("init discover-peers MaxRetries = %d, want %d (bounded deferral budget)", found.MaxRetries, discoverPeersMaxRetries)
	}
}

func TestTaskMaxRetries(t *testing.T) {
	cases := map[string]int{
		TaskConfigureGenesis: genesisConfigureMaxRetries,
		TaskAssembleGenesis:  groupAssemblyMaxRetries,
		TaskDiscoverPeers:    discoverPeersMaxRetries,
		"unknown-task-type":  0,
		"":                   0,
	}
	for taskType, want := range cases {
		if got := taskMaxRetries(taskType); got != want {
			t.Errorf("taskMaxRetries(%q) = %d, want %d", taskType, got, want)
		}
	}
}
