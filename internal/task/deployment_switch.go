package task

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// ---------------------------------------------------------------------------
// switch-traffic: updates the SeiNodeGroup's deployment status to reflect
// the incoming revision as active. The group controller's networking
// reconciliation uses the active revision in the Service selector, so
// updating the status is sufficient to switch traffic.
// ---------------------------------------------------------------------------

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
	return &switchTrafficExecution{
		id:     id,
		params: p,
		cfg:    cfg,
		status: ExecutionRunning,
	}, nil
}

func (e *switchTrafficExecution) Execute(ctx context.Context) error {
	group, err := ResourceAs[*seiv1alpha1.SeiNodeGroup](e.cfg)
	if err != nil {
		e.status = ExecutionFailed
		e.err = err
		return err
	}

	if group.Status.Deployment == nil {
		e.status = ExecutionFailed
		e.err = fmt.Errorf("no deployment status on group %s", e.params.GroupName)
		return e.err
	}

	// The plan executor patches status after each task, and the group
	// controller uses activeRevision in groupSelector(). Updating
	// activeRevision here causes the next Service reconciliation to
	// point traffic at the green nodes.
	group.Status.Deployment.ActiveRevision = e.params.IncomingRevision

	log.FromContext(ctx).Info("traffic switched to incoming revision",
		"group", e.params.GroupName, "revision", e.params.IncomingRevision)

	e.status = ExecutionComplete
	return nil
}

func (e *switchTrafficExecution) Status(_ context.Context) ExecutionStatus {
	return e.status
}

func (e *switchTrafficExecution) Err() error { return e.err }

// ---------------------------------------------------------------------------
// teardown-blue: deletes old (blue) SeiNode resources.
// ---------------------------------------------------------------------------

type teardownBlueExecution struct {
	id     string
	params TeardownBlueParams
	cfg    ExecutionConfig
	status ExecutionStatus
	err    error
}

func deserializeTeardownBlue(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p TeardownBlueParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing teardown-blue params: %w", err)
		}
	}
	return &teardownBlueExecution{
		id:     id,
		params: p,
		cfg:    cfg,
		status: ExecutionRunning,
	}, nil
}

func (e *teardownBlueExecution) Execute(ctx context.Context) error {
	logger := log.FromContext(ctx)

	for _, name := range e.params.NodeNames {
		node := &seiv1alpha1.SeiNode{}
		err := e.cfg.KubeClient.Get(ctx, types.NamespacedName{Name: name, Namespace: e.params.Namespace}, node)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			e.status = ExecutionFailed
			e.err = fmt.Errorf("getting blue node %s: %w", name, err)
			return e.err
		}

		if err := e.cfg.KubeClient.Delete(ctx, node); err != nil && !apierrors.IsNotFound(err) {
			e.status = ExecutionFailed
			e.err = fmt.Errorf("deleting blue node %s: %w", name, err)
			return e.err
		}
		logger.Info("deleted blue node", "node", name)
	}

	e.status = ExecutionComplete
	return nil
}

func (e *teardownBlueExecution) Status(_ context.Context) ExecutionStatus {
	return e.status
}

func (e *teardownBlueExecution) Err() error { return e.err }
