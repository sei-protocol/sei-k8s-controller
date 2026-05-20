package taskruntime

import (
	"fmt"
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Downward-API env vars projecting the parent Workflow's identity onto each
// Task pod. The Workflow CR's metadata.name → label
// chaos-mesh.org/workflow; metadata.uid → annotation
// chaos-mesh.org/workflow-uid (UID is not directly projectable from
// fieldRef, so Chaos Mesh writes it into an annotation).
const (
	EnvWorkflowName = "SEI_WORKFLOW_NAME"
	EnvWorkflowUID  = "SEI_WORKFLOW_UID"
	EnvNamespace    = "SEI_NAMESPACE"

	workflowAPIVersion = "chaos-mesh.org/v1alpha1"
	workflowKind       = "Workflow"
)

// WorkflowIdentity is the parent Workflow CR's identity, read once at
// subcommand startup from the downward-API env vars.
type WorkflowIdentity struct {
	Name      string
	UID       types.UID
	Namespace string
}

// LoadWorkflowIdentity returns the parent Workflow's identity from env.
// Missing env vars → InfraError; the subcommand cannot stamp ownerRefs.
func LoadWorkflowIdentity() (WorkflowIdentity, error) {
	name := os.Getenv(EnvWorkflowName)
	uid := os.Getenv(EnvWorkflowUID)
	ns := os.Getenv(EnvNamespace)
	missing := []string{}
	if name == "" {
		missing = append(missing, EnvWorkflowName)
	}
	if uid == "" {
		missing = append(missing, EnvWorkflowUID)
	}
	if ns == "" {
		missing = append(missing, EnvNamespace)
	}
	if len(missing) > 0 {
		return WorkflowIdentity{}, Infra(fmt.Errorf("downward-API env not projected: %v", missing))
	}
	return WorkflowIdentity{Name: name, UID: types.UID(uid), Namespace: ns}, nil
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
