package task

import (
	"context"
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
)

const TaskTypeReplacePod = "replace-pod"

type ReplacePodParams struct {
	NodeName  string `json:"nodeName"`
	Namespace string `json:"namespace"`
}

type replacePodExecution struct {
	taskBase
	podCycle
	params ReplacePodParams
}

func deserializeReplacePod(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p ReplacePodParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing replace-pod params: %w", err)
		}
	}
	return &replacePodExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		podCycle: podCycle{cfg: cfg},
		params:   p,
	}, nil
}

// Execute deletes pods at the StatefulSet's old revision. Pairs with
// the StatefulSet's OnDelete update strategy: pod lifecycle is the
// SeiNode controller's responsibility, not the StatefulSet controller's.
//
// Revision-gated and readiness-blind: it completes as soon as old-revision
// pods are deleted, without waiting for the replacement to become Ready. The
// image-rollout path depends on this — seid may be intentionally unready (e.g.
// halted at a chain-upgrade height).
func (e *replacePodExecution) Execute(ctx context.Context) error {
	node, sts, err := e.fetchStatefulSet(ctx)
	if err != nil {
		return err
	}
	if sts == nil {
		return nil
	}

	// Revision gate runs before the selector/replica guards: a not-yet-observed
	// or not-yet-populated revision is a transient wait, and an already-rolled
	// StatefulSet is a complete no-op — neither should reach pod deletion.
	if sts.Status.ObservedGeneration < sts.Generation {
		return nil
	}
	if sts.Status.UpdateRevision == "" {
		return nil
	}
	if sts.Status.CurrentRevision == sts.Status.UpdateRevision {
		e.complete()
		return nil
	}

	if err := guardSelectorAndReplicas(node, sts, TaskTypeReplacePod); err != nil {
		return err
	}

	pods, err := e.ownedPods(ctx, node, sts)
	if err != nil {
		return err
	}

	updateRev := sts.Status.UpdateRevision
	for i := range pods {
		pod := &pods[i]
		hash, hasHash := pod.Labels[appsv1.ControllerRevisionHashLabelKey]
		if !hasHash {
			continue
		}
		if hash == updateRev {
			continue
		}
		if pod.DeletionTimestamp != nil {
			continue
		}
		if err := e.deletePod(ctx, pod); err != nil {
			return err
		}
	}

	e.complete()
	return nil
}

// Status returns the cached execution status. replace-pod completes
// synchronously within Execute (no readiness wait).
func (e *replacePodExecution) Status(_ context.Context) ExecutionStatus {
	return e.DefaultStatus()
}
