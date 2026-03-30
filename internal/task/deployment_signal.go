package task

import (
	"context"
	"encoding/json"
	"fmt"

	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// submitHaltSignalExecution is a fire-and-forget task that submits
// await-condition(height=H, action=SIGTERM) to each incumbent node's
// sidecar. Completes immediately after submission (best-effort).
type submitHaltSignalExecution struct {
	id     string
	params SubmitHaltSignalParams
	cfg    ExecutionConfig
	status ExecutionStatus
	err    error
}

func deserializeSubmitHaltSignal(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p SubmitHaltSignalParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing submit-halt-signal params: %w", err)
		}
	}
	return &submitHaltSignalExecution{id: id, params: p, cfg: cfg, status: ExecutionRunning}, nil
}

func (e *submitHaltSignalExecution) Execute(ctx context.Context) error {
	logger := log.FromContext(ctx)

	for _, name := range e.params.NodeNames {
		e.submitToNode(ctx, logger, name)
	}

	// Best-effort: always complete. For a real hard fork, consensus
	// halts at the height regardless of whether SIGTERM was delivered.
	e.status = ExecutionComplete
	return nil
}

func (e *submitHaltSignalExecution) submitToNode(ctx context.Context, logger interface{ Info(string, ...any) }, name string) {
	node := &seiv1alpha1.SeiNode{}
	if err := e.cfg.KubeClient.Get(ctx, types.NamespacedName{Name: name, Namespace: e.params.Namespace}, node); err != nil {
		logger.Info("cannot get incumbent node for halt signal, skipping", "node", name, "error", err)
		return
	}

	sc, err := sidecarClientForNode(node)
	if err != nil {
		logger.Info("cannot build sidecar client for halt signal, skipping", "node", name, "error", err)
		return
	}

	taskID := deterministicDeploymentTaskID(name, "halt-signal", e.id)
	req := sidecar.TaskRequest{
		Id:   &taskID,
		Type: sidecar.TaskTypeAwaitCondition,
		Params: &map[string]any{
			"condition":    sidecar.ConditionHeight,
			"targetHeight": e.params.HaltHeight,
			"action":       sidecar.ActionSIGTERM,
		},
	}

	if _, err := sc.SubmitTask(ctx, req); err != nil {
		logger.Info("failed to submit halt signal to incumbent node (best-effort)",
			"node", name, "error", err)
	} else {
		logger.Info("halt signal submitted to incumbent node",
			"node", name, "haltHeight", e.params.HaltHeight)
	}
}

func (e *submitHaltSignalExecution) Status(_ context.Context) ExecutionStatus {
	return e.status
}

func (e *submitHaltSignalExecution) Err() error { return e.err }
