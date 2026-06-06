package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const TaskTypeRestartPod = "restart-pod"

// RestartPodParams identifies the target SeiNode whose pod is restarted so
// seid re-reads config.toml on the next start.
//
// RestartedPodUID is the content-addressed restart signal, captured once at
// synthesis and threaded through params so it is stable across reconciles. The
// task deletes only this pod and completes once an owned Ready pod with a
// different UID exists. Keying on UID rather than a creation-time epoch avoids
// the same-second-truncation race: an OnDelete replacement always has a fresh
// UID. The synthesis site never populates this empty for kind=RestartPod (it
// defers synthesis until a pod is observed); an empty UID reaching the task is
// treated as a wait, never as success, so a no-op restart can't masquerade as
// complete.
type RestartPodParams struct {
	NodeName        string    `json:"nodeName"`
	Namespace       string    `json:"namespace"`
	RestartedPodUID types.UID `json:"restartedPodUID,omitempty"`
}

type restartPodExecution struct {
	taskBase
	podCycle
	params RestartPodParams
}

func deserializeRestartPod(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p RestartPodParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing restart-pod params: %w", err)
		}
	}
	return &restartPodExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		podCycle: podCycle{cfg: cfg},
		params:   p,
	}, nil
}

// Execute deletes the target pod identified by RestartedPodUID. Pairs with the
// StatefulSet's OnDelete update strategy: pod lifecycle is the SeiNode
// controller's responsibility, and OnDelete recreates the pod at the same
// (unchanged) revision so seid re-reads config.toml on start.
//
// Idempotent and stateless across reconciles: the captured pod is deleted; a
// replacement pod (different UID), an empty UID (none captured), or a missing
// pod is a no-op. A missing StatefulSet/pod is a transient wait (apply-statefulset
// or scheduling may lag), not an error.
func (e *restartPodExecution) Execute(ctx context.Context) error {
	node, sts, err := e.fetchStatefulSet(ctx)
	if err != nil {
		return err
	}
	if sts == nil {
		return nil
	}
	if err := guardSelectorAndReplicas(node, sts, TaskTypeRestartPod); err != nil {
		return err
	}

	// Defense-in-depth: the synthesis site never dispatches an empty UID for
	// RestartPod, so this is unreachable in practice. If one ever slips through,
	// delete nothing — Status reports Running so the controller's execution
	// timeout fails the task rather than completing without a restart.
	if e.params.RestartedPodUID == "" {
		return nil
	}

	pod, err := e.ownedPod(ctx, node, sts)
	if err != nil {
		return err
	}
	// Only delete the captured pod — a different UID is already the
	// replacement (idempotency across reconciles).
	if pod == nil || pod.UID != e.params.RestartedPodUID || pod.DeletionTimestamp != nil {
		return nil
	}
	return e.deletePod(ctx, pod)
}

// Status completes when an owned pod that is NOT the restarted pod is Ready.
// An empty RestartedPodUID never completes (it reports Running until the
// controller's execution timeout fails the task): completing on the first owned
// Ready pod would let a restart that deleted nothing masquerade as success. The
// original pod (matching UID), a terminating pod, or a missing pod also reports
// Running so the executor re-polls.
func (e *restartPodExecution) Status(ctx context.Context) ExecutionStatus {
	if s, done := e.isTerminal(); done {
		return s
	}
	if e.params.RestartedPodUID == "" {
		return ExecutionRunning
	}

	node, sts, err := e.fetchStatefulSet(ctx)
	if err != nil {
		// Terminal type-assertion failure surfaces as a failed task; a missing
		// StatefulSet is a transient wait.
		var termErr *TerminalError
		if errors.As(err, &termErr) {
			e.setFailed(err)
			return ExecutionFailed
		}
		return ExecutionRunning
	}
	if sts == nil {
		return ExecutionRunning
	}

	pod, err := e.ownedPod(ctx, node, sts)
	if err != nil || pod == nil {
		return ExecutionRunning
	}
	if pod.DeletionTimestamp != nil || pod.UID == e.params.RestartedPodUID {
		return ExecutionRunning
	}
	if !podReady(pod) {
		return ExecutionRunning
	}

	e.complete()
	return ExecutionComplete
}

// ownedPod returns the single pod owned by the StatefulSet (matching its
// selector and ownerReference), or nil if none is found.
func (e *restartPodExecution) ownedPod(ctx context.Context, node *seiv1alpha1.SeiNode, sts *appsv1.StatefulSet) (*corev1.Pod, error) {
	pods, err := e.ownedPods(ctx, node, sts)
	if err != nil {
		return nil, err
	}
	if len(pods) == 0 {
		return nil, nil
	}
	return &pods[0], nil
}
