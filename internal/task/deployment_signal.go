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

// ---------------------------------------------------------------------------
// submit-halt-signal-blue: fire-and-forget submission of
// await-condition(height=H, action=SIGTERM) to each blue node's sidecar.
// Completes immediately after submission (best-effort).
// ---------------------------------------------------------------------------

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
	return &submitHaltSignalExecution{
		id:     id,
		params: p,
		cfg:    cfg,
		status: ExecutionRunning,
	}, nil
}

func (e *submitHaltSignalExecution) Execute(ctx context.Context) error {
	logger := log.FromContext(ctx)

	for _, name := range e.params.NodeNames {
		node := &seiv1alpha1.SeiNode{}
		if err := e.cfg.KubeClient.Get(ctx, types.NamespacedName{Name: name, Namespace: e.params.Namespace}, node); err != nil {
			logger.Info("cannot get blue node for halt signal, skipping", "node", name, "error", err)
			continue
		}

		sc, err := buildSidecarClientForNode(e.cfg, node)
		if err != nil {
			logger.Info("cannot build sidecar client for halt signal, skipping", "node", name, "error", err)
			continue
		}

		taskID := uuid.NewSHA1(uuid.MustParse("b7e89c3a-4f12-4d8b-9a6e-1c2d3e4f5a6b"),
			fmt.Appendf(nil, "%s/halt-signal/%s", name, e.id))
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
			// Best-effort: log warning but don't fail the plan. For a real
			// hard fork, consensus halts at the height regardless.
			logger.Info("failed to submit halt signal to blue node (best-effort)",
				"node", name, "error", err)
		} else {
			logger.Info("halt signal submitted to blue node",
				"node", name, "haltHeight", e.params.HaltHeight)
		}
	}

	e.status = ExecutionComplete
	return nil
}

func (e *submitHaltSignalExecution) Status(_ context.Context) ExecutionStatus {
	return e.status
}

func (e *submitHaltSignalExecution) Err() error { return e.err }
