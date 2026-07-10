package k8s

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

const (
	wfName       = "resync-0"
	renderWFName = "resync"
	witnessA     = "a:26657"
	witnessB     = "b:26657"
)

func workflowSpec() sei.WorkflowSpec {
	return sei.WorkflowSpec{
		Name: wfName, Namespace: testNS, Node: rpc0Name, Kind: sei.WorkflowStateSync,
		StateSync: &sei.StateSyncWorkflow{RpcServers: []string{witnessA, witnessB}},
	}
}

func workflowInPhase(phase seiv1alpha1.SeiNodeTaskWorkflowPhase, conds ...metav1.Condition) *seiv1alpha1.SeiNodeTaskWorkflow {
	return &seiv1alpha1.SeiNodeTaskWorkflow{
		ObjectMeta: metav1.ObjectMeta{Name: wfName, Namespace: testNS},
		Spec: seiv1alpha1.SeiNodeTaskWorkflowSpec{
			Kind:      seiv1alpha1.SeiNodeTaskWorkflowKindStateSync,
			Target:    seiv1alpha1.SeiNodeTaskTarget{NodeRef: seiv1alpha1.SeiNodeTaskNodeRef{Name: rpc0Name}},
			StateSync: &seiv1alpha1.StateSyncWorkflow{RpcServers: []string{witnessA, witnessB}},
		},
		Status: seiv1alpha1.SeiNodeTaskWorkflowStatus{Phase: phase, Conditions: conds},
	}
}

func TestCreateWorkflow_AppliesAndReturnsHandle(t *testing.T) {
	p := providerWith(t, http.DefaultClient)
	h, err := p.CreateWorkflow(context.Background(), workflowSpec())
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}
	if h.Name() != wfName || h.Namespace() != testNS {
		t.Fatalf("handle identity = %s/%s", h.Namespace(), h.Name())
	}
	got := &seiv1alpha1.SeiNodeTaskWorkflow{}
	if err := p.c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: wfName}, got); err != nil {
		t.Fatalf("applied workflow not found: %v", err)
	}
	if got.Spec.StateSync == nil || len(got.Spec.StateSync.RpcServers) != 2 {
		t.Errorf("stateSync payload not rendered: %+v", got.Spec.StateSync)
	}
	if _, ok := h.Object().(*seiv1alpha1.SeiNodeTaskWorkflow); !ok {
		t.Errorf("Object() did not return *SeiNodeTaskWorkflow, got %T", h.Object())
	}
}

func TestGetWorkflow_ReadsBackPhase(t *testing.T) {
	p := providerWith(t, http.DefaultClient, workflowInPhase(seiv1alpha1.SeiNodeTaskWorkflowPhaseRunning))
	h, err := p.GetWorkflow(context.Background(), wfName, testNS)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if h.Phase() != sei.WorkflowPhaseRunning {
		t.Errorf("Phase() = %q, want Running", h.Phase())
	}
}

func TestWorkflowWaitTerminal_Complete(t *testing.T) {
	p := providerWith(t, http.DefaultClient, workflowInPhase(seiv1alpha1.SeiNodeTaskWorkflowPhaseComplete))
	h := &workflowHandle{p: p, namespace: testNS, name: wfName}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.WaitTerminal(ctx); err != nil {
		t.Fatalf("WaitTerminal: %v", err)
	}
	if h.Phase() != sei.WorkflowPhaseComplete {
		t.Errorf("Phase() = %q after complete", h.Phase())
	}
}

func TestWorkflowWaitTerminal_FailedCarriesReason(t *testing.T) {
	failed := workflowInPhase(seiv1alpha1.SeiNodeTaskWorkflowPhaseFailed, metav1.Condition{
		Type:    seiv1alpha1.ConditionSeiNodeTaskWorkflowFailed,
		Status:  metav1.ConditionTrue,
		Reason:  "WorkflowFailed",
		Message: "task reset-data: injected failure",
	})
	p := providerWith(t, http.DefaultClient, failed)
	h := &workflowHandle{p: p, namespace: testNS, name: wfName}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := h.WaitTerminal(ctx)
	if err == nil {
		t.Fatal("Failed workflow should error")
	}
	if sei.IsTimeout(err) {
		t.Fatalf("Failed must not be a timeout, got %v", err)
	}
	if !strings.Contains(err.Error(), "injected failure") {
		t.Errorf("error should carry the failure reason, got %v", err)
	}
}

func TestWorkflowWaitTerminal_DeadlineIsTimeout(t *testing.T) {
	p := providerWith(t, http.DefaultClient, workflowInPhase(seiv1alpha1.SeiNodeTaskWorkflowPhaseRunning))
	h := &workflowHandle{p: p, namespace: testNS, name: wfName}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := h.WaitTerminal(ctx)
	if err == nil || !sei.IsTimeout(err) {
		t.Fatalf("non-terminal workflow under an elapsed ctx should be IsTimeout, got %v", err)
	}
}

func TestWorkflowDelete_Idempotent(t *testing.T) {
	p := providerWith(t, http.DefaultClient)
	h := &workflowHandle{p: p, namespace: testNS, name: "absent"}
	if err := h.Delete(context.Background()); err != nil {
		t.Fatalf("deleting an absent workflow should be nil, got %v", err)
	}
}
