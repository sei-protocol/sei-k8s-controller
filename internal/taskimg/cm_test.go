package taskimg

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

func testIdentity() WorkflowIdentity {
	return WorkflowIdentity{Name: "wf-test", UID: "uid-test", Namespace: "nightly"}
}

func TestEnsureWorkflowVarsCM_CreatesWithOwnerRef(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	w := testIdentity()

	if err := EnsureWorkflowVarsCM(context.Background(), c, w, map[VarKey]string{KeyRunID: "wf-test"}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	got := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "nightly", Name: "workflow-vars-wf-test"}, got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Data[string(KeyRunID)] != "wf-test" {
		t.Fatalf("seed not written: %v", got.Data)
	}
	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].Kind != "Workflow" {
		t.Fatalf("ownerRef not stamped: %v", got.OwnerReferences)
	}
}

func TestEnsureWorkflowVarsCM_AlreadyExistsIsNoError(t *testing.T) {
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "workflow-vars-wf-test", Namespace: "nightly"},
		Data:       map[string]string{string(KeyRunID): "wf-test"},
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(existing).Build()

	if err := EnsureWorkflowVarsCM(context.Background(), c, testIdentity(), nil); err != nil {
		t.Fatalf("Ensure on existing: %v", err)
	}
}

func TestSetVars_MergesIntoExisting(t *testing.T) {
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "workflow-vars-wf-test", Namespace: "nightly"},
		Data:       map[string]string{string(KeyRunID): "wf-test"},
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(existing).Build()
	w := testIdentity()

	if err := SetVars(context.Background(), c, w, map[VarKey]string{
		KeyAdminAddress:    "sei1abc",
		KeyAdminSecretName: "admin-wf-test",
	}); err != nil {
		t.Fatalf("SetVars: %v", err)
	}

	got := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "nightly", Name: "workflow-vars-wf-test"}, got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Data[string(KeyRunID)] != "wf-test" {
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
