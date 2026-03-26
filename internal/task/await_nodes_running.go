package task

import (
	"context"
	"encoding/json"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const TaskTypeAwaitNodesRunning = "await-nodes-running"

// AwaitNodesRunningParams holds the serialized parameters for the
// await-nodes-running task. The task polls child SeiNodes until all
// reach PhaseRunning.
type AwaitNodesRunningParams struct {
	GroupName string `json:"groupName"`
	Namespace string `json:"namespace"`
	Expected  int    `json:"expected"`
}

type awaitNodesRunningExecution struct {
	id     string
	params AwaitNodesRunningParams
	cfg    ExecutionConfig
	status ExecutionStatus
	err    error
}

func deserializeAwaitNodesRunning(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p AwaitNodesRunningParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing await-nodes-running params: %w", err)
		}
	}
	return &awaitNodesRunningExecution{
		id:     id,
		params: p,
		cfg:    cfg,
		status: ExecutionRunning,
	}, nil
}

func (e *awaitNodesRunningExecution) Execute(_ context.Context) error { return nil }

func (e *awaitNodesRunningExecution) Status(ctx context.Context) ExecutionStatus {
	if e.status == ExecutionComplete || e.status == ExecutionFailed {
		return e.status
	}

	nodeList := &seiv1alpha1.SeiNodeList{}
	if err := e.cfg.KubeClient.List(ctx, nodeList,
		client.InNamespace(e.params.Namespace),
		client.MatchingLabels{"sei.io/nodegroup": e.params.GroupName},
	); err != nil {
		return ExecutionRunning
	}

	running := 0
	for i := range nodeList.Items {
		switch nodeList.Items[i].Status.Phase {
		case seiv1alpha1.PhaseRunning:
			running++
		case seiv1alpha1.PhaseFailed:
			e.err = fmt.Errorf("node %s is in Failed phase", nodeList.Items[i].Name)
			e.status = ExecutionFailed
			return ExecutionFailed
		}
	}

	if running >= e.params.Expected {
		e.status = ExecutionComplete
		return ExecutionComplete
	}
	return ExecutionRunning
}

func (e *awaitNodesRunningExecution) Err() error { return e.err }
