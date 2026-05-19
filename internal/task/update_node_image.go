package task

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const TaskTypeUpdateNodeImage = "update-node-image"

// updateNodeImageFieldOwner isolates SeiNodeTask's image patches from the
// SeiNode controller's own SSA writes. Co-ownership of spec.image with the
// seinode-controller field manager is intentional: SeiNodeTask asserts the
// new value once, then the SeiNode controller drives the rollout from there.
var updateNodeImageFieldOwner = client.FieldOwner("seinode-task-controller")

// +kubebuilder:rbac:groups=sei.io,resources=seinodes,verbs=patch

// UpdateNodeImageParams is the on-disk task payload. NodeName + Namespace
// identify the target SeiNode; Image is the desired container image.
type UpdateNodeImageParams struct {
	NodeName  string `json:"nodeName"`
	Namespace string `json:"namespace"`
	Image     string `json:"image"`
}

type updateNodeImageExecution struct {
	taskBase
	params UpdateNodeImageParams
	cfg    ExecutionConfig
}

func deserializeUpdateNodeImage(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p UpdateNodeImageParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing update-node-image params: %w", err)
		}
	}
	if p.NodeName == "" || p.Namespace == "" || p.Image == "" {
		return nil, Terminal(fmt.Errorf("update-node-image: nodeName, namespace, and image are required"))
	}
	return &updateNodeImageExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

// Execute idempotently asserts target.spec.image == params.Image via SSA
// with fieldOwner=seinode-task-controller. Safe to call across reconciles:
// SSA short-circuits when the field already matches under our manager.
func (e *updateNodeImageExecution) Execute(ctx context.Context) error {
	target := &seiv1alpha1.SeiNode{}
	key := types.NamespacedName{Name: e.params.NodeName, Namespace: e.params.Namespace}
	// APIReader bypasses the cache: target.status.currentImage may have just
	// been stamped by the SeiNode controller in the same reconcile loop.
	if err := e.cfg.APIReader.Get(ctx, key, target); err != nil {
		if apierrors.IsNotFound(err) {
			return Terminal(fmt.Errorf("target SeiNode %q not found", key.Name))
		}
		return fmt.Errorf("getting target SeiNode: %w", err)
	}

	if target.Spec.Image == e.params.Image {
		return nil
	}

	// SSA apply: only set spec.image. We omit other spec fields so the
	// seinode-controller field manager retains ownership of everything else.
	patch := &seiv1alpha1.SeiNode{}
	patch.APIVersion = seiv1alpha1.GroupVersion.String()
	patch.Kind = "SeiNode"
	patch.SetName(target.Name)
	patch.SetNamespace(target.Namespace)
	patch.Spec.Image = e.params.Image

	//nolint:staticcheck // migrating to typed ApplyConfiguration is a separate effort
	if err := e.cfg.KubeClient.Patch(ctx, patch, client.Apply, updateNodeImageFieldOwner, client.ForceOwnership); err != nil {
		return fmt.Errorf("patching target spec.image: %w", err)
	}
	return nil
}

// Status polls target.status.currentImage. Complete iff currentImage matches
// params.Image. No readiness check by design — major-upgrade scenarios
// deliberately expect CrashLoop after early upgrade. Readiness is a
// separate AwaitCondition step.
func (e *updateNodeImageExecution) Status(ctx context.Context) ExecutionStatus {
	if s, done := e.isTerminal(); done {
		return s
	}
	target := &seiv1alpha1.SeiNode{}
	key := types.NamespacedName{Name: e.params.NodeName, Namespace: e.params.Namespace}
	if err := e.cfg.APIReader.Get(ctx, key, target); err != nil {
		// Transient read miss right after the apply; let the next reconcile retry.
		return ExecutionRunning
	}
	if target.Status.CurrentImage == e.params.Image {
		e.complete()
		return ExecutionComplete
	}
	return ExecutionRunning
}
