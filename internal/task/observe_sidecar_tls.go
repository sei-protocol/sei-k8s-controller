package task

import (
	"context"
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const TaskTypeObserveSidecarTLS = "observe-sidecar-tls"

type ObserveSidecarTLSParams struct {
	NodeName  string `json:"nodeName"`
	Namespace string `json:"namespace"`
}

type observeSidecarTLSExecution struct {
	taskBase
	params ObserveSidecarTLSParams
	cfg    ExecutionConfig
}

func deserializeObserveSidecarTLS(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p ObserveSidecarTLSParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing observe-sidecar-tls params: %w", err)
		}
	}
	return &observeSidecarTLSExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

// Execute polls the StatefulSet rollout. Once converged, stamps
// status.currentSidecarTLS to mirror spec.sidecar.tls so the planner's
// drift detector observes the toggle as applied. Mirrors ObserveImage:
// the StatefulSet template is the source of truth for pod composition,
// so rollout convergence implies the live pod matches.
func (e *observeSidecarTLSExecution) Execute(ctx context.Context) error {
	node, err := ResourceAs[*seiv1alpha1.SeiNode](e.cfg)
	if err != nil {
		return Terminal(err)
	}

	sts := &appsv1.StatefulSet{}
	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}
	if err := e.cfg.APIReader.Get(ctx, key, sts); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting statefulset: %w", err)
	}

	if sts.Status.ObservedGeneration < sts.Generation {
		return nil
	}
	if sts.Spec.Replicas == nil || sts.Status.UpdatedReplicas < *sts.Spec.Replicas {
		return nil
	}

	node.Status.CurrentSidecarTLS = sidecarTLSStatusFromSpec(node)
	e.complete()
	return nil
}

func (e *observeSidecarTLSExecution) Status(_ context.Context) ExecutionStatus {
	return e.DefaultStatus()
}

// sidecarTLSStatusFromSpec returns the SidecarTLSStatus mirroring the
// node's spec.sidecar.tls, or nil if TLS is not enabled. Centralizes
// the spec → status projection so the observer and any future caller
// agree on the shape.
func sidecarTLSStatusFromSpec(node *seiv1alpha1.SeiNode) *seiv1alpha1.SidecarTLSStatus {
	if node.Spec.Sidecar == nil || node.Spec.Sidecar.TLS == nil {
		return nil
	}
	return &seiv1alpha1.SidecarTLSStatus{
		IssuerName: node.Spec.Sidecar.TLS.IssuerName,
		IssuerKind: node.Spec.Sidecar.TLS.IssuerKind,
	}
}
