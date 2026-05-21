package envtest

import (
	"context"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// StartStatefulSetStatusFaker spawns a goroutine that periodically reconciles
// the .Status of every StatefulSet in the cluster so envtest can drive
// rollouts to completion. envtest's apiserver has no StatefulSet
// controller — Pods never get created, .Status stays empty, and the
// SeiNode rollout plan (ReplacePod + ObserveImage) gates on
// status.observedGeneration / status.updatedReplicas which never advance.
//
// The faker patches every observed StatefulSet to look "fully rolled
// out at the current spec generation":
//
//   - status.observedGeneration = .Generation
//   - status.currentRevision  = "stub-rev"
//   - status.updateRevision   = "stub-rev"   (matches currentRevision so
//     ReplacePod's "rollout complete" branch fires immediately)
//   - status.updatedReplicas  = *spec.replicas (ObserveImage's gate)
//   - status.readyReplicas    = *spec.replicas
//   - status.replicas         = *spec.replicas
//
// This is intentionally indistinguishable from "rollout already done".
// The InPlace test does not assert on transient rollout state — it
// asserts on terminal state — so collapsing the rollout to
// instantaneous is fine.
//
// The returned cancel func stops the goroutine. Callers should defer it.
// Poll interval is short (50ms) because the InPlace test's poll loop
// runs at 200ms; the faker needs to win the race for every reconcile
// trigger.
func StartStatefulSetStatusFaker(ctx context.Context, kc client.Client) context.CancelFunc {
	innerCtx, cancel := context.WithCancel(ctx)
	go runFaker(innerCtx, kc)
	return cancel
}

func runFaker(ctx context.Context, kc client.Client) {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := fakeStatuses(ctx, kc); err != nil && !errors.Is(err, context.Canceled) {
				// Best-effort; the next tick will retry. Swallow rather
				// than spam logs — envtest teardown races are noisy
				// enough already.
				_ = err
			}
		}
	}
}

func fakeStatuses(ctx context.Context, kc client.Client) error {
	list := &appsv1.StatefulSetList{}
	if err := kc.List(ctx, list); err != nil {
		return fmt.Errorf("listing statefulsets: %w", err)
	}
	for i := range list.Items {
		sts := &list.Items[i]
		var replicas int32 = 1
		if sts.Spec.Replicas != nil {
			replicas = *sts.Spec.Replicas
		}

		desired := appsv1.StatefulSetStatus{
			ObservedGeneration: sts.Generation,
			Replicas:           replicas,
			ReadyReplicas:      replicas,
			UpdatedReplicas:    replicas,
			AvailableReplicas:  replicas,
			CurrentRevision:    "stub-rev",
			UpdateRevision:     "stub-rev",
			CurrentReplicas:    replicas,
		}
		if statusEqual(sts.Status, desired) {
			continue
		}
		patch := client.MergeFrom(sts.DeepCopy())
		sts.Status = desired
		if err := kc.Status().Patch(ctx, sts, patch); err != nil {
			if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
				continue
			}
			return fmt.Errorf("patching statefulset %s/%s status: %w", sts.Namespace, sts.Name, err)
		}
	}
	return nil
}

func statusEqual(a, b appsv1.StatefulSetStatus) bool {
	return a.ObservedGeneration == b.ObservedGeneration &&
		a.Replicas == b.Replicas &&
		a.ReadyReplicas == b.ReadyReplicas &&
		a.UpdatedReplicas == b.UpdatedReplicas &&
		a.AvailableReplicas == b.AvailableReplicas &&
		a.CurrentRevision == b.CurrentRevision &&
		a.UpdateRevision == b.UpdateRevision &&
		a.CurrentReplicas == b.CurrentReplicas
}
