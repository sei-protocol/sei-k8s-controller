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
// the entrant revision as active. The group controller's networking
// reconciliation uses the active revision in the Service selector, so
// updating the status is sufficient to switch traffic.
type switchTrafficExecution struct {
	id     string
	params SwitchTrafficParams
	cfg    ExecutionConfig
	status ExecutionStatus
	err    error
}

func deserializeSwitchTraffic(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p SwitchTrafficParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing switch-traffic params: %w", err)
		}
	}
	return &switchTrafficExecution{id: id, params: p, cfg: cfg, status: ExecutionRunning}, nil
}

func (e *switchTrafficExecution) Execute(ctx context.Context) error {
	group, err := ResourceAs[*seiv1alpha1.SeiNodeGroup](e.cfg)
	if err != nil {
		return e.fail(err)
	}

	if group.Status.Deployment == nil {
		return e.fail(fmt.Errorf("no deployment status on group %s", e.params.GroupName))
	}

	patch := client.MergeFrom(group.DeepCopy())
	group.Status.Deployment.IncumbentRevision = e.params.EntrantRevision
	if err := e.cfg.KubeClient.Status().Patch(ctx, group, patch); err != nil {
		return e.fail(fmt.Errorf("patching deployment revision: %w", err))
	}

	log.FromContext(ctx).Info("traffic switched to entrant revision",
		"group", e.params.GroupName, "revision", e.params.EntrantRevision)

	e.status = ExecutionComplete
	return nil
}

func (e *switchTrafficExecution) fail(err error) error {
	e.status = ExecutionFailed
	e.err = err
	return err
}

func (e *switchTrafficExecution) Status(_ context.Context) ExecutionStatus { return e.status }
func (e *switchTrafficExecution) Err() error                               { return e.err }

// teardownNodesExecution deletes incumbent SeiNode resources after
// traffic has been switched to the entrant set.
type teardownNodesExecution struct {
	id     string
	params TeardownNodesParams
	cfg    ExecutionConfig
	status ExecutionStatus
	err    error
}

func deserializeTeardownNodes(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p TeardownNodesParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing teardown-nodes params: %w", err)
		}
	}
	return &teardownNodesExecution{id: id, params: p, cfg: cfg, status: ExecutionRunning}, nil
}

func (e *teardownNodesExecution) Execute(ctx context.Context) error {
	logger := log.FromContext(ctx)

	for _, name := range e.params.NodeNames {
		if err := e.deleteNode(ctx, name); err != nil {
			return e.fail(err)
		}
		logger.Info("deleted incumbent node", "node", name)
	}

	e.status = ExecutionComplete
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

func (e *teardownNodesExecution) fail(err error) error {
	e.status = ExecutionFailed
	e.err = err
	return err
}

func (e *teardownNodesExecution) Status(_ context.Context) ExecutionStatus { return e.status }
func (e *teardownNodesExecution) Err() error                               { return e.err }
