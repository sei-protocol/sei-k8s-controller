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
			Migration: &sei.ConfigMigration{
				Kind:      sei.ConfigMigrationGigaStore,
				GigaStore: &sei.GigaStoreMigration{Backend: "rocksdb"},
			},
			RpcServers: []string{witnessA, witnessB},
		},
	}
	wf := renderWorkflow(spec, testNS)

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
	// The typed migration renders onto spec.stateSync.migration; the controller
	// materializes its fixed flags into the config-patch step.
	m := wf.Spec.StateSync.Migration
	if m == nil {
		t.Fatal("migration is nil")
	}
	if m.Kind != seiv1alpha1.ConfigMigrationGigaStore {
		t.Errorf("migration.kind = %q, want GigaStore", m.Kind)
	}
	if m.GigaStore == nil || m.GigaStore.Backend != "rocksdb" {
		t.Errorf("migration.gigaStore = %+v, want backend rocksdb", m.GigaStore)
	}
	if len(wf.Spec.StateSync.RpcServers) != 2 {
		t.Errorf("rpcServers = %v", wf.Spec.StateSync.RpcServers)
	}
}

func TestRenderWorkflow_NilMigrationRendersNil(t *testing.T) {
	wf := renderWorkflow(sei.WorkflowSpec{
		Name: renderWFName, Namespace: testNS, Node: testTaskNode, Kind: sei.WorkflowStateSync,
		StateSync: &sei.StateSyncWorkflow{RpcServers: []string{witnessA, witnessB}},
	}, testNS)
	if wf.Spec.StateSync.Migration != nil {
		t.Errorf("nil migration should render nil, got %v", wf.Spec.StateSync.Migration)
	}
}
