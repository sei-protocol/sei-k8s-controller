package task

import (
	"context"
	"encoding/json"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
)

const TaskTypeApplySidecarCert = "apply-sidecar-cert"

type ApplySidecarCertParams struct {
	NodeName  string `json:"nodeName"`
	Namespace string `json:"namespace"`
}

type applySidecarCertExecution struct {
	taskBase
	params ApplySidecarCertParams
	cfg    ExecutionConfig
}

func deserializeApplySidecarCert(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p ApplySidecarCertParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing apply-sidecar-cert params: %w", err)
		}
	}
	return &applySidecarCertExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

func (e *applySidecarCertExecution) Execute(ctx context.Context) error {
	node, err := ResourceAs[*seiv1alpha1.SeiNode](e.cfg)
	if err != nil {
		return err
	}

	desired := noderesource.GenerateSidecarCertificate(node)
	if desired == nil {
		e.complete()
		return nil
	}
	if err := ctrl.SetControllerReference(node, desired, e.cfg.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on sidecar Certificate: %w", err)
	}

	// ForceOwnership: our reconcile wins on field conflicts. cert-manager
	// owns .status (separate field manager); we set only .spec.
	//nolint:staticcheck // unstructured Patch + Apply is the documented path for non-typed CRDs
	if err := e.cfg.KubeClient.Patch(ctx, desired, client.Apply, fieldOwner, client.ForceOwnership); err != nil {
		return fmt.Errorf("applying sidecar Certificate: %w", err)
	}

	e.complete()
	return nil
}

func (e *applySidecarCertExecution) Status(_ context.Context) ExecutionStatus {
	return e.DefaultStatus()
}
