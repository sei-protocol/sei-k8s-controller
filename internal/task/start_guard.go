package task

import (
	"context"
	"encoding/json"
	"fmt"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	sidecar "github.com/sei-protocol/sei-k8s-controller/sidecarapi/client"
)

// DataResetPending reports whether node has a data reset it has not finished:
// spec.dataResetGeneration above status.dataResetGeneration. Only a node with
// spec.nodeConfig can carry the counter (CEL), so the nodeConfig check is a
// guard against a raw controller-internal caller, not a policy.
func DataResetPending(node *seiv1alpha1.SeiNode) bool {
	return node.Spec.NodeConfig != nil &&
		node.Spec.DataResetGeneration > node.Status.DataResetGeneration
}

// StartBlocked returns why the sidecar start gate must stay closed on node, or
// "" when mark-ready may reach the sidecar. It is the start guard: the one rule
// every path that can start seid obeys, so a plan built before the spec changed,
// or a MarkReady SeiNodeTask, cannot release seid onto data a reset has not
// cleared yet.
func StartBlocked(node *seiv1alpha1.SeiNode) string {
	if DataResetPending(node) {
		return fmt.Sprintf("data reset pending: spec.dataResetGeneration=%d, status.dataResetGeneration=%d",
			node.Spec.DataResetGeneration, node.Status.DataResetGeneration)
	}
	return ""
}

// deserializeMarkReady wraps the mark-ready sidecar task in the start guard.
// Both callers resolve the same SeiNode into cfg.Resource: the plan executor
// (the reconciled node) and the SeiNodeTask controller (the target node). A
// resource that is not a SeiNode (a SeiNetwork group plan) has no gate of its
// own, so it passes through unguarded.
func deserializeMarkReady(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	inner, err := deserializeSidecar[sidecar.MarkReadyTask](id, params, cfg.BuildSidecarClient, true)
	if err != nil {
		return nil, err
	}
	node, ok := cfg.Resource.(*seiv1alpha1.SeiNode)
	if !ok {
		return inner, nil
	}
	return &startGuardedExecution{TaskExecution: inner, node: node}, nil
}

// StartGuardRefusal prefixes the error of a mark-ready the start guard refused.
// The planner reads it from a failed plan to tell a deferred start from a
// failed task, so it is stable.
const StartGuardRefusal = "mark-ready refused by the start guard"

// startGuardedExecution checks the start guard at submission, the moment the
// gate would open. A refusal is terminal: the plan fails and the planner builds
// the next plan from the current spec, which puts the reset first. Waiting
// instead would hold the node's only plan slot and block that reset.
type startGuardedExecution struct {
	TaskExecution
	node *seiv1alpha1.SeiNode
	err  error
}

func (e *startGuardedExecution) Execute(ctx context.Context) error {
	if reason := StartBlocked(e.node); reason != "" {
		e.err = fmt.Errorf("%s: %s", StartGuardRefusal, reason)
		return Terminal(e.err)
	}
	return e.TaskExecution.Execute(ctx)
}

func (e *startGuardedExecution) Status(ctx context.Context) ExecutionStatus {
	if e.err != nil {
		return ExecutionFailed
	}
	return e.TaskExecution.Status(ctx)
}

func (e *startGuardedExecution) Err() error {
	if e.err != nil {
		return e.err
	}
	return e.TaskExecution.Err()
}
