package taskruntime

import (
	"context"
	"fmt"
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Identity env contract for Task pods (downward API, projected by the
// scenario YAML's container env block):
//
//	SEI_WORKFLOW_NAME ← fieldRef metadata.labels['chaos-mesh.org/workflow']
//	SEI_NAMESPACE     ← fieldRef metadata.namespace
//
// The Workflow CR's UID is NOT projectable via downward API — Chaos Mesh
// does not stamp it on Task pods — so we fetch it from the apiserver at
// subcommand startup using NAME + NAMESPACE. SEI_WORKFLOW_UID env, when
// set, short-circuits the lookup; tests use it.
const (
	EnvWorkflowName = "SEI_WORKFLOW_NAME"
	EnvWorkflowUID  = "SEI_WORKFLOW_UID"
	EnvNamespace    = "SEI_NAMESPACE"

	workflowAPIVersion = "chaos-mesh.org/v1alpha1"
	workflowKind       = "Workflow"
)

var workflowGVK = schema.GroupVersionKind{Group: "chaos-mesh.org", Version: "v1alpha1", Kind: workflowKind}

// WorkflowIdentity is the parent Workflow CR's identity, read once at
// subcommand startup.
type WorkflowIdentity struct {
	Name      string
	UID       types.UID
	Namespace string
}

// LoadWorkflowIdentity reads NAME + NAMESPACE from env (downward-API
// projected on each Task pod), then fetches the Workflow CR's UID from
// the apiserver. SEI_WORKFLOW_UID env short-circuits the round-trip.
func LoadWorkflowIdentity(ctx context.Context, c client.Client) (WorkflowIdentity, error) {
	name := os.Getenv(EnvWorkflowName)
	ns := os.Getenv(EnvNamespace)
	missing := []string{}
	if name == "" {
		missing = append(missing, EnvWorkflowName)
	}
	if ns == "" {
		missing = append(missing, EnvNamespace)
	}
	if len(missing) > 0 {
		return WorkflowIdentity{}, Infra(fmt.Errorf("downward-API env not projected: %v", missing))
	}
	if uid := os.Getenv(EnvWorkflowUID); uid != "" {
		return WorkflowIdentity{Name: name, UID: types.UID(uid), Namespace: ns}, nil
	}
	wf := &unstructured.Unstructured{}
	wf.SetGroupVersionKind(workflowGVK)
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, wf); err != nil {
		return WorkflowIdentity{}, Infra(fmt.Errorf("fetching Workflow %s/%s for UID: %w", ns, name, err))
	}
	uid := wf.GetUID()
	if uid == "" {
		return WorkflowIdentity{}, Infra(fmt.Errorf("workflow %s/%s exists but has no UID", ns, name))
	}
	return WorkflowIdentity{Name: name, UID: uid, Namespace: ns}, nil
}

// OwnerRef returns an ownerReference to the parent Workflow CR. Controller
// is explicit-false (Chaos Mesh manages Workflow children only via
// WorkflowNodes); BlockOwnerDeletion is explicit-false so cleanup doesn't
// stall on slow Task children.
func (w WorkflowIdentity) OwnerRef() metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         workflowAPIVersion,
		Kind:               workflowKind,
		Name:               w.Name,
		UID:                w.UID,
		Controller:         new(bool),
		BlockOwnerDeletion: new(bool),
	}
}

// WorkflowVarsName returns the per-run workflow-vars ConfigMap name.
// Single-sourced so producers and consumers don't drift.
func WorkflowVarsName(workflowName string) string {
	return "workflow-vars-" + workflowName
}
