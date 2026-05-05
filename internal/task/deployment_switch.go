package task

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// switchTrafficExecution updates the group's deployment status to reflect
// the entrant revision as active.
type switchTrafficExecution struct {
	taskBase
	params SwitchTrafficParams
	cfg    ExecutionConfig
}

func deserializeSwitchTraffic(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p SwitchTrafficParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing switch-traffic params: %w", err)
		}
	}
	return &switchTrafficExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

func (e *switchTrafficExecution) Execute(ctx context.Context) error {
	group, err := ResourceAs[*seiv1alpha1.SeiNodeDeployment](e.cfg)
	if err != nil {
		return Terminal(err)
	}

	if group.Status.Rollout == nil {
		return Terminal(fmt.Errorf("no rollout status on group %s", e.params.GroupName))
	}

	patch := client.MergeFromWithOptions(group.DeepCopy(), client.MergeFromWithOptimisticLock{})
	group.Status.Rollout.IncumbentRevision = e.params.EntrantRevision
	if err := e.cfg.KubeClient.Status().Patch(ctx, group, patch); err != nil {
		return fmt.Errorf("patching rollout revision: %w", err) // transient
	}

	log.FromContext(ctx).Info("traffic switched to entrant revision",
		"group", e.params.GroupName, "revision", e.params.EntrantRevision)

	e.complete()
	return nil
}

func (e *switchTrafficExecution) Status(_ context.Context) ExecutionStatus {
	return e.status
}

// teardownNodesExecution deletes incumbent SeiNode resources.
type teardownNodesExecution struct {
	taskBase
	params TeardownNodesParams
	cfg    ExecutionConfig
}

func deserializeTeardownNodes(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p TeardownNodesParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing teardown-nodes params: %w", err)
		}
	}
	return &teardownNodesExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

func (e *teardownNodesExecution) Execute(ctx context.Context) error {
	logger := log.FromContext(ctx)

	for _, name := range e.params.NodeNames {
		if err := e.deleteNode(ctx, name); err != nil {
			return err // transient — kube API errors are retryable
		}
		logger.Info("deleted incumbent node", "node", name)
	}

	e.complete()
	return nil
}

func (e *teardownNodesExecution) deleteNode(ctx context.Context, name string) error {
	node := &seiv1alpha1.SeiNode{}
	err := e.cfg.KubeClient.Get(ctx, types.NamespacedName{Name: name, Namespace: e.params.Namespace}, node)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting incumbent node %s: %w", name, err)
	}
	if err := e.cfg.KubeClient.Delete(ctx, node); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting incumbent node %s: %w", name, err)
	}
	return nil
}

func (e *teardownNodesExecution) Status(_ context.Context) ExecutionStatus {
	return e.status
}
