package envtest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// StatusFaker drives StatefulSet .Status toward a "fully rolled out at
// current generation" state on a 50ms tick, since envtest's apiserver
// runs no StatefulSet controller of its own. Tests that need to observe
// transient (in-flight) rollout state can Pause() the faker, drive the
// scenario, then Resume() — see TestInPlaceRollout_Supersession.
type StatusFaker struct {
	ctx    context.Context
	cancel context.CancelFunc
	kc     client.Client

	mu     sync.Mutex
	paused bool
}

// StartStatefulSetStatusFaker spawns the faker goroutine and returns a
// handle. The fake patches every observed StatefulSet to look "fully
// rolled out at the current spec generation":
//
//   - status.observedGeneration = .Generation
//   - status.currentRevision  = "stub-rev"
//   - status.updateRevision   = "stub-rev"   (matches currentRevision so
//     ReplacePod's "rollout complete" branch fires immediately)
//   - status.updatedReplicas  = *spec.replicas (ObserveImage's gate)
//   - status.readyReplicas    = *spec.replicas
//   - status.replicas         = *spec.replicas
//
// This is intentionally indistinguishable from "rollout already done."
// envtest tests assert on terminal state, so collapsing the rollout to
// instantaneous is fine for the common case.
//
// Poll interval is 50ms because test poll loops run at 200ms; the faker
// needs to win the race for every reconcile trigger.
func StartStatefulSetStatusFaker(ctx context.Context, kc client.Client) *StatusFaker {
	innerCtx, cancel := context.WithCancel(ctx)
	f := &StatusFaker{ctx: innerCtx, cancel: cancel, kc: kc}
	go f.run()
	return f
}

// Stop cancels the faker goroutine.
func (f *StatusFaker) Stop() { f.cancel() }

// Pause halts status writes. StatefulSets observed while paused stay in
// whatever .Status state the apiserver last persisted, which lets a test
// stall a rollout mid-flight without modifying production controller code.
func (f *StatusFaker) Pause() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = true
}

// Resume re-enables status writes. The next tick will reconcile any
// StatefulSets that diverged from the desired faked state while paused.
func (f *StatusFaker) Resume() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = false
}

func (f *StatusFaker) isPaused() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paused
}

func (f *StatusFaker) run() {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-f.ctx.Done():
			return
		case <-t.C:
			if f.isPaused() {
				continue
			}
			if err := fakeStatuses(f.ctx, f.kc); err != nil && !errors.Is(err, context.Canceled) {
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
