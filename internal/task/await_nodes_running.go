package task

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const TaskTypeAwaitNodesRunning = "await-nodes-running"

// AwaitNodesRunningParams holds the serialized parameters for the
// await-nodes-running task. The task polls child SeiNodes until all
// reach PhaseRunning. When NodeNames is set, only those specific nodes
// are checked; otherwise all nodes matching the group label are checked.
type AwaitNodesRunningParams struct {
	GroupName string   `json:"groupName"`
	Namespace string   `json:"namespace"`
	Expected  int      `json:"expected"`
	NodeNames []string `json:"nodeNames,omitempty"`
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

	nodes, err := e.listTargetNodes(ctx)
	if err != nil {
		return ExecutionRunning
	}

	running := 0
	for i := range nodes {
		switch nodes[i].Status.Phase {
		case seiv1alpha1.PhaseRunning:
			running++
		case seiv1alpha1.PhaseFailed:
			e.err = fmt.Errorf("node %s is in Failed phase", nodes[i].Name)
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

func (e *awaitNodesRunningExecution) listTargetNodes(ctx context.Context) ([]seiv1alpha1.SeiNode, error) {
	if len(e.params.NodeNames) > 0 {
		nodes := make([]seiv1alpha1.SeiNode, 0, len(e.params.NodeNames))
		for _, name := range e.params.NodeNames {
			node := &seiv1alpha1.SeiNode{}
			if err := e.cfg.KubeClient.Get(ctx, types.NamespacedName{Name: name, Namespace: e.params.Namespace}, node); err != nil {
				return nil, err
			}
			nodes = append(nodes, *node)
		}
		return nodes, nil
	}
	nodeList := &seiv1alpha1.SeiNodeList{}
	if err := e.cfg.KubeClient.List(ctx, nodeList,
		client.InNamespace(e.params.Namespace),
		client.MatchingLabels{"sei.io/nodegroup": e.params.GroupName},
	); err != nil {
		return nil, err
	}
	return nodeList.Items, nil
}

func (e *awaitNodesRunningExecution) Err() error { return e.err }
