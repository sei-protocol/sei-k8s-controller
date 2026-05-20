package taskimg

import (
	"errors"
	"testing"
)

func TestLoadWorkflowIdentity(t *testing.T) {
	t.Run("all set", func(t *testing.T) {
		t.Setenv(EnvWorkflowName, "release-test-abc")
		t.Setenv(EnvWorkflowUID, "uid-xyz")
		t.Setenv(EnvNamespace, testNamespace)

		w, err := LoadWorkflowIdentity()
		if err != nil {
			t.Fatalf("LoadWorkflowIdentity: %v", err)
		}
		if w.Name != "release-test-abc" || string(w.UID) != "uid-xyz" || w.Namespace != testNamespace {
			t.Fatalf("got %+v", w)
		}
	})

	t.Run("missing all", func(t *testing.T) {
		t.Setenv(EnvWorkflowName, "")
		t.Setenv(EnvWorkflowUID, "")
		t.Setenv(EnvNamespace, "")

		_, err := LoadWorkflowIdentity()
		if err == nil {
			t.Fatalf("expected error, got nil")
		}
		var infra *InfraError
		if !errors.As(err, &infra) {
			t.Fatalf("expected InfraError, got %T: %v", err, err)
		}
	})
}

func TestOwnerRef(t *testing.T) {
	w := WorkflowIdentity{Name: "wf", UID: "uid", Namespace: "ns"}
	ref := w.OwnerRef()
	if ref.APIVersion != "chaos-mesh.org/v1alpha1" || ref.Kind != "Workflow" {
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
