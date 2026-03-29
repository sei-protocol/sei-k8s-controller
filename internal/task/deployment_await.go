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
// await-green-running: polls until all green SeiNodes reach Running phase.
// ---------------------------------------------------------------------------

type awaitGreenRunningExecution struct {
	id     string
	params AwaitGreenRunningParams
	cfg    ExecutionConfig
	status ExecutionStatus
	err    error
}

func deserializeAwaitGreenRunning(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p AwaitGreenRunningParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing await-green-running params: %w", err)
		}
	}
	return &awaitGreenRunningExecution{
		id:     id,
		params: p,
		cfg:    cfg,
		status: ExecutionRunning,
	}, nil
}

func (e *awaitGreenRunningExecution) Execute(_ context.Context) error { return nil }

func (e *awaitGreenRunningExecution) Status(ctx context.Context) ExecutionStatus {
	if e.status == ExecutionComplete || e.status == ExecutionFailed {
		return e.status
	}

	for _, name := range e.params.NodeNames {
		node := &seiv1alpha1.SeiNode{}
		if err := e.cfg.KubeClient.Get(ctx, types.NamespacedName{Name: name, Namespace: e.params.Namespace}, node); err != nil {
			return ExecutionRunning
		}
		switch node.Status.Phase {
		case seiv1alpha1.PhaseFailed:
			e.err = fmt.Errorf("green node %s entered Failed phase", name)
			e.status = ExecutionFailed
			return ExecutionFailed
		case seiv1alpha1.PhaseRunning:
			continue
		default:
			return ExecutionRunning
		}
	}

	e.status = ExecutionComplete
	return ExecutionComplete
}

func (e *awaitGreenRunningExecution) Err() error { return e.err }

// ---------------------------------------------------------------------------
// await-green-at-height: submits await-condition(height=H) to each green
// node's sidecar and polls until all complete. Used by HardFork strategy.
// ---------------------------------------------------------------------------

type awaitGreenAtHeightExecution struct {
	id        string
	params    AwaitGreenAtHeightParams
	cfg       ExecutionConfig
	status    ExecutionStatus
	err       error
	submitted map[string]uuid.UUID // node name → sidecar task ID
}

func deserializeAwaitGreenAtHeight(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p AwaitGreenAtHeightParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing await-green-at-height params: %w", err)
		}
	}
	return &awaitGreenAtHeightExecution{
		id:        id,
		params:    p,
		cfg:       cfg,
		status:    ExecutionRunning,
		submitted: make(map[string]uuid.UUID),
	}, nil
}

func (e *awaitGreenAtHeightExecution) Execute(ctx context.Context) error {
	logger := log.FromContext(ctx)
	for _, name := range e.params.NodeNames {
		node := &seiv1alpha1.SeiNode{}
		if err := e.cfg.KubeClient.Get(ctx, types.NamespacedName{Name: name, Namespace: e.params.Namespace}, node); err != nil {
			return fmt.Errorf("getting green node %s: %w", name, err)
		}
		sc, err := buildSidecarClientForNode(e.cfg, node)
		if err != nil {
			return fmt.Errorf("building sidecar client for %s: %w", name, err)
		}
		taskID := uuid.NewSHA1(uuid.MustParse("b7e89c3a-4f12-4d8b-9a6e-1c2d3e4f5a6b"),
			fmt.Appendf(nil, "%s/await-height/%s", name, e.id))
		req := sidecar.TaskRequest{
			Id:   &taskID,
			Type: sidecar.TaskTypeAwaitCondition,
			Params: &map[string]any{
				"condition":    sidecar.ConditionHeight,
				"targetHeight": e.params.HaltHeight,
			},
		}
		if _, err := sc.SubmitTask(ctx, req); err != nil {
			logger.Info("failed to submit await-height to green node, will retry", "node", name, "error", err)
			return err
		}
		e.submitted[name] = taskID
	}
	return nil
}

func (e *awaitGreenAtHeightExecution) Status(ctx context.Context) ExecutionStatus {
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
		node := &seiv1alpha1.SeiNode{}
		if err := e.cfg.KubeClient.Get(ctx, types.NamespacedName{Name: name, Namespace: e.params.Namespace}, node); err != nil {
			return ExecutionRunning
		}
		sc, err := buildSidecarClientForNode(e.cfg, node)
		if err != nil {
			return ExecutionRunning
		}
		result, err := sc.GetTask(ctx, taskID)
		if err != nil {
			return ExecutionRunning
		}
		switch result.Status {
		case sidecar.Completed:
			continue
		case sidecar.Failed:
			errMsg := "unknown error"
			if result.Error != nil {
				errMsg = *result.Error
			}
			e.err = fmt.Errorf("await-height failed on %s: %s", name, errMsg)
			e.status = ExecutionFailed
			return ExecutionFailed
		default:
			return ExecutionRunning
		}
	}

	e.status = ExecutionComplete
	return ExecutionComplete
}

func (e *awaitGreenAtHeightExecution) Err() error { return e.err }

// ---------------------------------------------------------------------------
// await-green-caught-up: polls green node sidecars until they report
// catching_up == false. Used by BlueGreen strategy.
//
// NOTE: This requires a ConditionCaughtUp addition to seictl's
// ConditionWaiter. Until then, this polls the seid RPC /status endpoint
// directly via the sidecar's proxy.
// For the initial implementation, we check if the green nodes are Running
// and have been for a sufficient time, using the sidecar status endpoint.
// ---------------------------------------------------------------------------

type awaitGreenCaughtUpExecution struct {
	id     string
	params AwaitGreenCaughtUpParams
	cfg    ExecutionConfig
	status ExecutionStatus
	err    error
}

func deserializeAwaitGreenCaughtUp(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p AwaitGreenCaughtUpParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing await-green-caught-up params: %w", err)
		}
	}
	return &awaitGreenCaughtUpExecution{
		id:     id,
		params: p,
		cfg:    cfg,
		status: ExecutionRunning,
	}, nil
}

func (e *awaitGreenCaughtUpExecution) Execute(_ context.Context) error { return nil }

func (e *awaitGreenCaughtUpExecution) Status(ctx context.Context) ExecutionStatus {
	if e.status == ExecutionComplete || e.status == ExecutionFailed {
		return e.status
	}

	// Check that all green nodes have sidecar reporting Ready status.
	// The sidecar transitions to Ready after mark-ready, which only
	// succeeds once seid is synced and serving.
	for _, name := range e.params.NodeNames {
		node := &seiv1alpha1.SeiNode{}
		if err := e.cfg.KubeClient.Get(ctx, types.NamespacedName{Name: name, Namespace: e.params.Namespace}, node); err != nil {
			return ExecutionRunning
		}

		sc, err := buildSidecarClientForNode(e.cfg, node)
		if err != nil {
			return ExecutionRunning
		}

		resp, err := sc.Status(ctx)
		if err != nil {
			return ExecutionRunning
		}
		if resp.Status != sidecar.Ready {
			return ExecutionRunning
		}
	}

	e.status = ExecutionComplete
	return ExecutionComplete
}

func (e *awaitGreenCaughtUpExecution) Err() error { return e.err }

// buildSidecarClientForNode constructs a SidecarClient from a SeiNode's
// pod DNS name and sidecar port.
func buildSidecarClientForNode(_ ExecutionConfig, node *seiv1alpha1.SeiNode) (*sidecar.SidecarClient, error) {
	port := int32(7777)
	if node.Spec.Sidecar != nil && node.Spec.Sidecar.Port != 0 {
		port = node.Spec.Sidecar.Port
	}
	return sidecar.NewSidecarClientFromPodDNS(node.Name, node.Namespace, port)
}
