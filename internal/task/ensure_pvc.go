package task

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
)

const TaskTypeEnsureDataPVC = "ensure-data-pvc"

// EnsureDataPVCParams identifies the node whose PVC should be ensured.
type EnsureDataPVCParams struct {
	NodeName  string `json:"nodeName"`
	Namespace string `json:"namespace"`
}

type ensureDataPVCExecution struct {
	taskBase
	params EnsureDataPVCParams
	cfg    ExecutionConfig
}

func deserializeEnsureDataPVC(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p EnsureDataPVCParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing ensure-data-pvc params: %w", err)
		}
	}
	return &ensureDataPVCExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

func (e *ensureDataPVCExecution) Execute(ctx context.Context) error {
	node, err := ResourceAs[*seiv1alpha1.SeiNode](e.cfg)
	if err != nil {
		return err
	}

	desired := noderesource.GenerateDataPVC(node, e.cfg.Platform)
	if err := ctrl.SetControllerReference(node, desired, e.cfg.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on data PVC: %w", err)
	}

	if err := e.cfg.KubeClient.Create(ctx, desired); err != nil {
		if apierrors.IsAlreadyExists(err) {
			e.complete()
			return nil
		}
		return fmt.Errorf("creating data PVC: %w", err)
	}

	e.complete()
	return nil
}

func (e *ensureDataPVCExecution) Status(_ context.Context) ExecutionStatus {
	if s, done := e.isTerminal(); done {
		return s
	}
	return e.status
}
