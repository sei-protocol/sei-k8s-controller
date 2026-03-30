package task

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// awaitNodesAtHeightExecution submits await-condition(height=H) to each
// node's sidecar and polls until all complete. Used by HardFork strategy.
type awaitNodesAtHeightExecution struct {
	id        string
	params    AwaitNodesAtHeightParams
	cfg       ExecutionConfig
	status    ExecutionStatus
	err       error
	submitted map[string]uuid.UUID
}

func deserializeAwaitNodesAtHeight(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p AwaitNodesAtHeightParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing await-nodes-at-height params: %w", err)
		}
	}
	return &awaitNodesAtHeightExecution{
		id: id, params: p, cfg: cfg,
		status: ExecutionRunning, submitted: make(map[string]uuid.UUID),
	}, nil
}

func (e *awaitNodesAtHeightExecution) Execute(ctx context.Context) error {
	logger := log.FromContext(ctx)
	for _, name := range e.params.NodeNames {
		sc, err := e.sidecarForNode(ctx, name)
		if err != nil {
			return err
		}
		taskID := deterministicDeploymentTaskID(name, "await-height", e.id)
		req := sidecar.TaskRequest{
			Id:   &taskID,
			Type: sidecar.TaskTypeAwaitCondition,
			Params: &map[string]any{
				"condition":    sidecar.ConditionHeight,
				"targetHeight": e.params.TargetHeight,
			},
		}
		if _, err := sc.SubmitTask(ctx, req); err != nil {
			logger.Info("failed to submit await-height, will retry", "node", name, "error", err)
			return err
		}
		e.submitted[name] = taskID
	}
	return nil
}

func (e *awaitNodesAtHeightExecution) Status(ctx context.Context) ExecutionStatus {
	if e.status == ExecutionComplete || e.status == ExecutionFailed {
		return e.status
	}
	if len(e.submitted) == 0 {
		return ExecutionRunning
	}
	for _, name := range e.params.NodeNames {
		taskID, ok := e.submitted[name]
		if !ok {
			return ExecutionRunning
		}
		status, err := e.pollSidecarTask(ctx, name, taskID)
		if err != nil {
			e.err = err
			e.status = ExecutionFailed
			return ExecutionFailed
		}
		if status != ExecutionComplete {
			return status
		}
	}
	e.status = ExecutionComplete
	return ExecutionComplete
}

func (e *awaitNodesAtHeightExecution) pollSidecarTask(ctx context.Context, name string, taskID uuid.UUID) (ExecutionStatus, error) {
	sc, err := e.sidecarForNode(ctx, name)
	if err != nil {
		return ExecutionRunning, nil
	}
	result, err := sc.GetTask(ctx, taskID)
	if err != nil {
		return ExecutionRunning, nil
	}
	switch result.Status {
	case sidecar.Completed:
		return ExecutionComplete, nil
	case sidecar.Failed:
		errMsg := "unknown error"
		if result.Error != nil {
			errMsg = *result.Error
		}
		return ExecutionFailed, fmt.Errorf("await-height failed on %s: %s", name, errMsg)
	default:
		return ExecutionRunning, nil
	}
}

func (e *awaitNodesAtHeightExecution) sidecarForNode(ctx context.Context, name string) (*sidecar.SidecarClient, error) {
	node := &seiv1alpha1.SeiNode{}
	if err := e.cfg.KubeClient.Get(ctx, types.NamespacedName{Name: name, Namespace: e.params.Namespace}, node); err != nil {
		return nil, fmt.Errorf("getting node %s: %w", name, err)
	}
	return sidecarClientForNode(node)
}

func (e *awaitNodesAtHeightExecution) Err() error { return e.err }

// awaitNodesCaughtUpExecution polls node sidecars until all report
// Ready status (sidecar transitions to Ready after mark-ready succeeds,
// which requires seid to be synced and serving).
type awaitNodesCaughtUpExecution struct {
	id     string
	params AwaitNodesCaughtUpParams
	cfg    ExecutionConfig
	status ExecutionStatus
	err    error
}

func deserializeAwaitNodesCaughtUp(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p AwaitNodesCaughtUpParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing await-nodes-caught-up params: %w", err)
		}
	}
	return &awaitNodesCaughtUpExecution{id: id, params: p, cfg: cfg, status: ExecutionRunning}, nil
}

func (e *awaitNodesCaughtUpExecution) Execute(_ context.Context) error { return nil }

func (e *awaitNodesCaughtUpExecution) Status(ctx context.Context) ExecutionStatus {
	if e.status == ExecutionComplete || e.status == ExecutionFailed {
		return e.status
	}
	for _, name := range e.params.NodeNames {
		if !e.isNodeReady(ctx, name) {
			return ExecutionRunning
		}
	}
	e.status = ExecutionComplete
	return ExecutionComplete
}

func (e *awaitNodesCaughtUpExecution) isNodeReady(ctx context.Context, name string) bool {
	node := &seiv1alpha1.SeiNode{}
	if err := e.cfg.KubeClient.Get(ctx, types.NamespacedName{Name: name, Namespace: e.params.Namespace}, node); err != nil {
		return false
	}
	sc, err := sidecarClientForNode(node)
	if err != nil {
		return false
	}
	resp, err := sc.Status(ctx)
	if err != nil {
		return false
	}
	return resp.Status == sidecar.Ready
}

func (e *awaitNodesCaughtUpExecution) Err() error { return e.err }

// sidecarClientForNode constructs a SidecarClient from a SeiNode's
// pod DNS name and sidecar port.
func sidecarClientForNode(node *seiv1alpha1.SeiNode) (*sidecar.SidecarClient, error) {
	port := int32(7777)
	if node.Spec.Sidecar != nil && node.Spec.Sidecar.Port != 0 {
		port = node.Spec.Sidecar.Port
	}
	return sidecar.NewSidecarClientFromPodDNS(node.Name, node.Namespace, port)
}

// deterministicDeploymentTaskID generates a UUID v5 scoped to deployment operations.
func deterministicDeploymentTaskID(nodeName, taskType, parentID string) uuid.UUID {
	return uuid.NewSHA1(deploymentTaskNamespace,
		fmt.Appendf(nil, "%s/%s/%s", nodeName, taskType, parentID))
}
