package task

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
)

const TaskTypeWaitForSidecarTLSSecret = "wait-for-sidecar-tls-secret"

type WaitForSidecarTLSSecretParams struct {
	NodeName  string `json:"nodeName"`
	Namespace string `json:"namespace"`
}

type waitForSidecarTLSSecretExecution struct {
	taskBase
	params WaitForSidecarTLSSecretParams
	cfg    ExecutionConfig
}

func deserializeWaitForSidecarTLSSecret(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p WaitForSidecarTLSSecretParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing wait-for-sidecar-tls-secret params: %w", err)
		}
	}
	return &waitForSidecarTLSSecretExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

// Execute polls the cert-manager-managed Secret for non-empty tls.crt.
// Returns nil while the Secret is absent or unpopulated — the task
// stays Running and the executor re-invokes on the next reconcile.
// Bypasses the cache because the Secret was just requested by the
// preceding apply-sidecar-cert task.
func (e *waitForSidecarTLSSecretExecution) Execute(ctx context.Context) error {
	node, err := ResourceAs[*seiv1alpha1.SeiNode](e.cfg)
	if err != nil {
		return Terminal(err)
	}

	sec := &corev1.Secret{}
	key := types.NamespacedName{Name: noderesource.SidecarTLSSecretName(node), Namespace: node.Namespace}
	if err := e.cfg.APIReader.Get(ctx, key, sec); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting sidecar TLS Secret: %w", err)
	}
	if len(sec.Data[corev1.TLSCertKey]) == 0 {
		return nil
	}

	e.complete()
	return nil
}

func (e *waitForSidecarTLSSecretExecution) Status(_ context.Context) ExecutionStatus {
	return e.DefaultStatus()
}
