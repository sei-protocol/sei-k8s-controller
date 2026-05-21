package taskruntime

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestLoadWorkflowIdentity(t *testing.T) {
	t.Run("env-short-circuit (UID set)", func(t *testing.T) {
		t.Setenv(EnvWorkflowName, "release-test-abc")
		t.Setenv(EnvWorkflowUID, "uid-xyz")
		t.Setenv(EnvNamespace, testNamespace)

		// Fake client w/ no Workflow CR — env UID should short-circuit
		// the apiserver lookup, so this should still succeed.
		c := fake.NewClientBuilder().Build()
		w, err := LoadWorkflowIdentity(context.Background(), c)
		if err != nil {
			t.Fatalf("LoadWorkflowIdentity: %v", err)
		}
		if w.Name != "release-test-abc" || string(w.UID) != "uid-xyz" || w.Namespace != testNamespace {
			t.Fatalf("got %+v", w)
		}
	})

	t.Run("apiserver-lookup (UID env empty)", func(t *testing.T) {
		t.Setenv(EnvWorkflowName, "release-test-abc")
		t.Setenv(EnvWorkflowUID, "")
		t.Setenv(EnvNamespace, testNamespace)

		wf := &unstructured.Unstructured{}
		wf.SetGroupVersionKind(workflowGVK)
		wf.SetName("release-test-abc")
		wf.SetNamespace(testNamespace)
		wf.SetUID("uid-from-apiserver")

		scheme := runtime.NewScheme()
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(wf).Build()
		w, err := LoadWorkflowIdentity(context.Background(), c)
		if err != nil {
			t.Fatalf("LoadWorkflowIdentity: %v", err)
		}
		if string(w.UID) != "uid-from-apiserver" {
			t.Fatalf("expected UID from apiserver lookup, got %q", w.UID)
		}
	})

	t.Run("missing name", func(t *testing.T) {
		t.Setenv(EnvWorkflowName, "")
		t.Setenv(EnvNamespace, testNamespace)
		c := fake.NewClientBuilder().Build()
		_, err := LoadWorkflowIdentity(context.Background(), c)
		var infra *InfraError
		if !errors.As(err, &infra) {
			t.Fatalf("expected InfraError, got %T: %v", err, err)
		}
	})

	t.Run("workflow CR not found", func(t *testing.T) {
		t.Setenv(EnvWorkflowName, "missing-workflow")
		t.Setenv(EnvWorkflowUID, "")
		t.Setenv(EnvNamespace, testNamespace)

		scheme := runtime.NewScheme()
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		_, err := LoadWorkflowIdentity(context.Background(), c)
		var infra *InfraError
		if !errors.As(err, &infra) {
			t.Fatalf("expected InfraError, got %T: %v", err, err)
		}
	})
}

func TestOwnerRef(t *testing.T) {
	w := WorkflowIdentity{Name: "wf", UID: "uid", Namespace: "ns"}
	ref := w.OwnerRef()
	if ref.APIVersion != "chaos-mesh.org/v1alpha1" || ref.Kind != workflowKind {
		t.Fatalf("wrong target: %+v", ref)
	}
	if ref.Name != "wf" || string(ref.UID) != "uid" {
		t.Fatalf("wrong identity: %+v", ref)
	}
	if ref.Controller == nil || *ref.Controller {
		t.Fatalf("Controller should be explicit false; got %+v", ref.Controller)
	}
	if ref.BlockOwnerDeletion == nil || *ref.BlockOwnerDeletion {
		t.Fatalf("BlockOwnerDeletion should be explicit false; got %+v", ref.BlockOwnerDeletion)
	}
}

func TestWorkflowVarsName(t *testing.T) {
	got := WorkflowVarsName("major-upgrade-20260520-184443")
	want := "workflow-vars-major-upgrade-20260520-184443"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
