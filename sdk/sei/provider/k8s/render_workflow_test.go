package k8s

import (
	"testing"
	"time"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

func TestRenderWorkflow_StateSync(t *testing.T) {
	spec := sei.WorkflowSpec{
		Name:                renderWFName,
		Namespace:           testNS,
		Node:                testTaskNode,
		Kind:                sei.WorkflowStateSync,
		Labels:              map[string]string{testRunLabel: testRunID},
		RequirePhase:        "Running",
		RequirePhaseTimeout: 10 * time.Minute,
		StateSync: &sei.StateSyncWorkflow{
			ConfigPatch: map[string]map[string]any{
				"app.toml": {"state-store.evm-ss-split": true},
			},
			RpcServers: []string{witnessA, witnessB},
		},
	}
	wf, err := renderWorkflow(spec, testNS)
	if err != nil {
		t.Fatalf("renderWorkflow: %v", err)
	}

	if wf.Name != renderWFName || wf.Namespace != testNS {
		t.Fatalf("metadata = %s/%s", wf.Namespace, wf.Name)
	}
	if got := wf.Labels[testRunLabel]; got != testRunID {
		t.Errorf("label %s = %q, want %q", testRunLabel, got, testRunID)
	}
	if string(wf.Spec.Kind) != sei.WorkflowStateSync {
		t.Errorf("kind = %q", wf.Spec.Kind)
	}
	if wf.Spec.Target.NodeRef.Name != testTaskNode {
		t.Errorf("target.nodeRef.name = %q", wf.Spec.Target.NodeRef.Name)
	}
	if wf.Spec.Target.RequirePhase != seiv1alpha1.PhaseRunning {
		t.Errorf("requirePhase = %q", wf.Spec.Target.RequirePhase)
	}
	if wf.Spec.Target.RequirePhaseTimeout == nil || wf.Spec.Target.RequirePhaseTimeout.Duration != 10*time.Minute {
		t.Errorf("requirePhaseTimeout = %v", wf.Spec.Target.RequirePhaseTimeout)
	}
	if wf.Spec.StateSync == nil {
		t.Fatal("stateSync payload is nil")
	}
	// A bool config value must render as a JSON bool, not the string "true".
	got := string(wf.Spec.StateSync.ConfigPatch["app.toml"]["state-store.evm-ss-split"].Raw)
	if got != "true" {
		t.Errorf("configPatch value = %q, want JSON bool true", got)
	}
	if len(wf.Spec.StateSync.RpcServers) != 2 {
		t.Errorf("rpcServers = %v", wf.Spec.StateSync.RpcServers)
	}
}

func TestRenderWorkflow_EmptyConfigPatchIsNil(t *testing.T) {
	wf, err := renderWorkflow(sei.WorkflowSpec{
		Name: renderWFName, Namespace: testNS, Node: testTaskNode, Kind: sei.WorkflowStateSync,
		StateSync: &sei.StateSyncWorkflow{RpcServers: []string{witnessA, witnessB}},
	}, testNS)
	if err != nil {
		t.Fatalf("renderWorkflow: %v", err)
	}
	if wf.Spec.StateSync.ConfigPatch != nil {
		t.Errorf("empty ConfigPatch should render nil, got %v", wf.Spec.StateSync.ConfigPatch)
	}
}
