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
// cleared yet, or out from under a maintenance hold.
//
// The hold half acts only on a Running node. An init plan fails the node
// terminally (FailedPhase=Failed), so refusing its mark-ready because a hold
// arrived mid-init would destroy the node; instead seid may start once, and the
// hold plan stops it when the node reaches Running. A hold set before the init
// plan is built parks the node without starting it (parkInsteadOfRelease).
func StartBlocked(node *seiv1alpha1.SeiNode) string {
	if reason := resetBlocks(node); reason != "" {
		return reason
	}
	if hold := node.Spec.HoldRequested(); hold != "" && node.Status.Phase == seiv1alpha1.PhaseRunning {
		return fmt.Sprintf("maintenance hold %s is set", hold)
	}
	return ""
}

// resetBlocks is the reset half of the start guard. The hold's own start-once
// step obeys only this half: it is how an AfterExit hold starts a parked seid.
func resetBlocks(node *seiv1alpha1.SeiNode) string {
	if DataResetPending(node) {
		return fmt.Sprintf("data reset pending: spec.dataResetGeneration=%d, status.dataResetGeneration=%d",
			node.Spec.DataResetGeneration, node.Status.DataResetGeneration)
	}
	return ""
}

// TaskTypeStartSeidOnce is the maintenance hold's start-once step. It submits
// the sidecar's mark-ready, like the mark-ready plan task, but the hold does not
// block it: an AfterExit hold on a parked node starts seid once, waits for
// await-seid-start, and closes the gate again with mark-not-ready. A pending
// reset still blocks it.
const TaskTypeStartSeidOnce = "start-seid-once"

// deserializeMarkReady wraps the mark-ready sidecar task in the start guard.
// Both callers resolve the same SeiNode into cfg.Resource: the plan executor
// (the reconciled node) and the SeiNodeTask controller (the target node). A
// resource that is not a SeiNode (a SeiNetwork group plan) has no gate of its
// own, so it passes through unguarded.
func deserializeMarkReady(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	return deserializeGuardedMarkReady(id, params, cfg, StartBlocked)
}

func deserializeStartSeidOnce(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	return deserializeGuardedMarkReady(id, params, cfg, resetBlocks)
}

func deserializeGuardedMarkReady(
	id string, params json.RawMessage, cfg ExecutionConfig, blocked func(*seiv1alpha1.SeiNode) string,
) (TaskExecution, error) {
	inner, err := deserializeSidecar[sidecar.MarkReadyTask](id, params, cfg.BuildSidecarClient, true)
	if err != nil {
		return nil, err
	}
	node, ok := cfg.Resource.(*seiv1alpha1.SeiNode)
	if !ok {
		return inner, nil
	}
	return &startGuardedExecution{TaskExecution: inner, node: node, blocked: blocked}, nil
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
	node    *seiv1alpha1.SeiNode
	blocked func(*seiv1alpha1.SeiNode) string
	err     error
}

func (e *startGuardedExecution) Execute(ctx context.Context) error {
	if reason := e.blocked(e.node); reason != "" {
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
