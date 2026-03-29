package task

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

type createGreenNodesExecution struct {
	id     string
	params CreateGreenNodesParams
	cfg    ExecutionConfig
	status ExecutionStatus
	err    error
}

func deserializeCreateGreenNodes(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p CreateGreenNodesParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing create-green-nodes params: %w", err)
		}
	}
	return &createGreenNodesExecution{
		id:     id,
		params: p,
		cfg:    cfg,
		status: ExecutionRunning,
	}, nil
}

func (e *createGreenNodesExecution) Execute(ctx context.Context) error {
	group, err := ResourceAs[*seiv1alpha1.SeiNodeGroup](e.cfg)
	if err != nil {
		e.status = ExecutionFailed
		e.err = err
		return err
	}

	for i, name := range e.params.NodeNames {
		// Check if already exists (idempotent).
		existing := &seiv1alpha1.SeiNode{}
		if err := e.cfg.KubeClient.Get(ctx, types.NamespacedName{Name: name, Namespace: e.params.Namespace}, existing); err == nil {
			continue
		}

		// Build green node from the group's current template.
		spec := group.Spec.Template.Spec.DeepCopy()
		if spec.PodLabels == nil {
			spec.PodLabels = make(map[string]string)
		}
		spec.PodLabels["sei.io/nodegroup"] = e.params.GroupName

		node := &seiv1alpha1.SeiNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: e.params.Namespace,
				Labels: map[string]string{
					"sei.io/nodegroup":         e.params.GroupName,
					"sei.io/nodegroup-ordinal": fmt.Sprintf("%d", i),
					"sei.io/revision":          e.params.IncomingRevision,
				},
			},
			Spec: *spec,
		}

		if err := ctrl.SetControllerReference(group, node, e.cfg.Scheme); err != nil {
			e.status = ExecutionFailed
			e.err = fmt.Errorf("setting owner reference on %s: %w", name, err)
			return e.err
		}

		if err := e.cfg.KubeClient.Create(ctx, node); err != nil {
			if apierrors.IsAlreadyExists(err) {
				continue
			}
			e.status = ExecutionFailed
			e.err = fmt.Errorf("creating green node %s: %w", name, err)
			return e.err
		}
	}

	e.status = ExecutionComplete
	return nil
}

func (e *createGreenNodesExecution) Status(_ context.Context) ExecutionStatus {
	return e.status
}

func (e *createGreenNodesExecution) Err() error { return e.err }
