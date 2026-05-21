package taskruntime

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

const (
	testNamespace      = "nightly"
	testWorkflowName   = "wf-test"
	testWorkflowVarsCM = "workflow-vars-wf-test"
)

func testIdentity() WorkflowIdentity {
	return WorkflowIdentity{Name: testWorkflowName, UID: "uid-test", Namespace: testNamespace}
}

func TestEnsureWorkflowVarsCM_CreatesWithOwnerRef(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	w := testIdentity()

	if err := EnsureWorkflowVarsCM(context.Background(), c, w, map[VarKey]string{KeyRunID: testWorkflowName}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	got := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testWorkflowVarsCM}, got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Data[string(KeyRunID)] != testWorkflowName {
		t.Fatalf("seed not written: %v", got.Data)
	}
	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].Kind != workflowKind {
		t.Fatalf("ownerRef not stamped: %v", got.OwnerReferences)
	}
}

func TestEnsureWorkflowVarsCM_AlreadyExistsIsNoError(t *testing.T) {
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testWorkflowVarsCM, Namespace: testNamespace},
		Data:       map[string]string{string(KeyRunID): testWorkflowName},
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(existing).Build()

	if err := EnsureWorkflowVarsCM(context.Background(), c, testIdentity(), nil); err != nil {
		t.Fatalf("Ensure on existing: %v", err)
	}
}

func TestSetVars_MergesIntoExisting(t *testing.T) {
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testWorkflowVarsCM, Namespace: testNamespace},
		Data:       map[string]string{string(KeyRunID): testWorkflowName},
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(existing).Build()
	w := testIdentity()

	if err := SetVars(context.Background(), c, w, map[VarKey]string{
		KeyAdminAddress:    "sei1abc",
		KeyAdminSecretName: "admin-" + testWorkflowName,
	}); err != nil {
		t.Fatalf("SetVars: %v", err)
	}

	got := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testWorkflowVarsCM}, got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Data[string(KeyRunID)] != testWorkflowName {
		t.Fatalf("existing key clobbered: %v", got.Data)
	}
	if got.Data[string(KeyAdminAddress)] != "sei1abc" || got.Data[string(KeyAdminSecretName)] != "admin-wf-test" {
		t.Fatalf("new keys not merged: %v", got.Data)
	}
}

func TestSetVar_ConfigMapMissingIsInfraError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	err := SetVar(context.Background(), c, testIdentity(), KeyChainID, "bench-1")
	if err == nil {
		t.Fatalf("expected error when CM missing")
	}
	var infra *InfraError
	if !errors.As(err, &infra) {
		t.Fatalf("expected InfraError, got %T", err)
	}
}

func TestSetVars_Empty(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	if err := SetVars(context.Background(), c, testIdentity(), nil); err != nil {
		t.Fatalf("SetVars(nil): %v", err)
	}
}
