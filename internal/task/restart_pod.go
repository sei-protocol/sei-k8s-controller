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
// RestartedPodUID is the content-addressed restart signal, read directly from
// the immutable spec.restartPod.podUID each reconcile (not snapshotted to
// status.task), so it is stable across reconciles. The task deletes this pod and
// completes once a replacement owned pod (a different UID) is Ready. Keying on
// UID rather than a creation-time epoch survives the same-second-truncation
// race: an OnDelete replacement always has a fresh UID. CEL requires a non-empty,
// immutable podUID for kind=RestartPod. The caller owns UID correctness: the task
// acts only on a UID that matches a live pod, so a stale UID leaves the pod in
// place and completes as a no-op (the contract is documented on the PodUID API
// field).
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
// Idempotent and stateless across reconciles: Execute deletes the pod whose UID
// matches RestartedPodUID. Any other observed state — the replacement pod (a
// different UID), an empty UID, or a missing pod — leaves the cluster untouched.
// A missing StatefulSet/pod is a transient wait (apply-statefulset or scheduling
// may lag) and reports Running for the next reconcile.
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

	// Defense-in-depth: CEL and synthesis guarantee a non-empty UID, so this is a
	// backstop. An empty UID short-circuits to a Running Status, leaving the
	// execution timeout to fail the task — a restart that deleted nothing stays
	// out of the Complete state.
	if e.params.RestartedPodUID == "" {
		return nil
	}

	pod, err := e.ownedPod(ctx, node, sts)
	if err != nil {
		return err
	}
	// Delete only the supplied pod. A matching live pod is the one to restart; a
	// different UID is already the OnDelete replacement (idempotent across
	// reconciles) or a stale caller-supplied UID — both leave the cluster as-is.
	if pod == nil || pod.UID != e.params.RestartedPodUID || pod.DeletionTimestamp != nil {
		return nil
	}
	return e.deletePod(ctx, pod)
}

// Status completes when the replacement pod — a Ready owned pod whose UID
// differs from RestartedPodUID — exists. An empty RestartedPodUID holds the task
// at Running until the execution timeout fails it, so a restart that deleted
// nothing stays out of the Complete state. The original pod (matching UID), a
// terminating pod, or a missing pod also hold at Running so the executor re-polls.
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
