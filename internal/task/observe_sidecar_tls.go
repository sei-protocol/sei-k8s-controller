package task

import (
	"context"
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
)

const TaskTypeObserveSidecarTLS = "observe-sidecar-tls"

// ObserveSidecarTLSParams identifies the node whose StatefulSet rollout
// to observe.
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

// Execute polls StatefulSet rollout convergence and stamps
// status.currentSidecarTLSSecretName from spec on success.
func (e *observeSidecarTLSExecution) Execute(ctx context.Context) error {
	node, err := ResourceAs[*seiv1alpha1.SeiNode](e.cfg)
	if err != nil {
		return Terminal(err)
	}
	if !noderesource.SidecarTLSEnabled(node) {
		return Terminal(fmt.Errorf("observe-sidecar-tls scheduled but spec.sidecar.tls is nil"))
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
	if sts.Spec.Replicas == nil {
		return nil
	}
	// Require ReadyReplicas (proxy passed its readiness probe), not
	// just UpdatedReplicas — stamping flips transport to HTTPS.
	if sts.Status.UpdatedReplicas < *sts.Spec.Replicas || sts.Status.ReadyReplicas < *sts.Spec.Replicas {
		return nil
	}

	node.Status.CurrentSidecarTLSSecretName = node.Spec.Sidecar.TLS.SecretName
	e.complete()
	return nil
}

func (e *observeSidecarTLSExecution) Status(_ context.Context) ExecutionStatus {
	return e.DefaultStatus()
}
