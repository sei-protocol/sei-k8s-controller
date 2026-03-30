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

type createEntrantNodesExecution struct {
	id     string
	params CreateEntrantNodesParams
	cfg    ExecutionConfig
	status ExecutionStatus
	err    error
}

func deserializeCreateEntrantNodes(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p CreateEntrantNodesParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing create-entrant-nodes params: %w", err)
		}
	}
	return &createEntrantNodesExecution{id: id, params: p, cfg: cfg, status: ExecutionRunning}, nil
}

func (e *createEntrantNodesExecution) Execute(ctx context.Context) error {
	group, err := ResourceAs[*seiv1alpha1.SeiNodeGroup](e.cfg)
	if err != nil {
		return e.fail(err)
	}

	for i, name := range e.params.NodeNames {
		if err := e.ensureEntrantNode(ctx, group, name, i); err != nil {
			return e.fail(err)
		}
	}

	e.status = ExecutionComplete
	return nil
}

func (e *createEntrantNodesExecution) ensureEntrantNode(
	ctx context.Context,
	group *seiv1alpha1.SeiNodeGroup,
	name string,
	ordinal int,
) error {
	existing := &seiv1alpha1.SeiNode{}
	if err := e.cfg.KubeClient.Get(ctx, types.NamespacedName{Name: name, Namespace: e.params.Namespace}, existing); err == nil {
		return nil // already exists
	}

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
				"sei.io/nodegroup-ordinal": fmt.Sprintf("%d", ordinal),
				"sei.io/revision":          e.params.EntrantRevision,
			},
		},
		Spec: *spec,
	}

	if err := ctrl.SetControllerReference(group, node, e.cfg.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on %s: %w", name, err)
	}

	if err := e.cfg.KubeClient.Create(ctx, node); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating entrant node %s: %w", name, err)
	}
	return nil
}

func (e *createEntrantNodesExecution) fail(err error) error {
	e.status = ExecutionFailed
	e.err = err
	return err
}

func (e *createEntrantNodesExecution) Status(_ context.Context) ExecutionStatus {
	return e.status
}

func (e *createEntrantNodesExecution) Err() error { return e.err }
