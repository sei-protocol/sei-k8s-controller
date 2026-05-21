package noderesource

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// SyncStatefulSet brings the live StatefulSet for node into line with
// the rendered desired spec using typed Get + Create/Update — no
// server-side apply, no Unstructured. Returns the resulting live
// object so callers can record its identity (Name/UID) on the SeiNode
// status.
//
// Behaviors:
//   - Spec.Paused=true forces Replicas=0 in the desired render.
//   - When Status.StatefulSet tracks a UID and the live object has a
//     different one, the impostor is deleted and recreated (out-of-
//     band recreation by an operator).
//   - When the live object is absent, it is created. When present and
//     mutable fields diverge, it is Updated; otherwise no apiserver
//     write is issued.
//
// Status mutation is left to the caller. The controller updates
// Status.StatefulSet on first observation; the apply-statefulset task
// can ignore the return because the controller catches up on the next
// reconcile.
func SyncStatefulSet(
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	node *seiv1alpha1.SeiNode,
	platform PlatformConfig,
) (*appsv1.StatefulSet, error) {
	desired, err := GenerateStatefulSet(node, platform)
	if err != nil {
		return nil, fmt.Errorf("generating statefulset: %w", err)
	}
	if err := ctrl.SetControllerReference(node, desired, scheme); err != nil {
		return nil, fmt.Errorf("setting owner reference: %w", err)
	}
	if node.Spec.Paused {
		desired.Spec.Replicas = ptr.To(int32(0))
	}

	existing := &appsv1.StatefulSet{}
	key := client.ObjectKeyFromObject(desired)
	getErr := c.Get(ctx, key, existing)

	if apierrors.IsNotFound(getErr) {
		if err := c.Create(ctx, desired); err != nil {
			return nil, fmt.Errorf("creating statefulset: %w", err)
		}
		return desired, nil
	}
	if getErr != nil {
		return nil, fmt.Errorf("fetching statefulset: %w", getErr)
	}

	if node.Status.StatefulSet != nil && existing.UID != node.Status.StatefulSet.UID {
		if err := c.Delete(ctx, existing); err != nil && !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("deleting impostor statefulset: %w", err)
		}
		if err := c.Create(ctx, desired); err != nil {
			return nil, fmt.Errorf("recreating statefulset after UID mismatch: %w", err)
		}
		return desired, nil
	}

	if mutateStatefulSet(existing, desired) {
		if err := c.Update(ctx, existing); err != nil {
			return nil, fmt.Errorf("updating statefulset: %w", err)
		}
	}
	return existing, nil
}

// mutateStatefulSet copies mutable Spec fields from desired into
// existing and returns whether anything changed. Immutable fields
// (Selector, ServiceName, VolumeClaimTemplates, PodManagementPolicy)
// are intentionally left alone so a deterministic-but-not-byte-
// identical render never trips an apiserver immutability check.
func mutateStatefulSet(existing, desired *appsv1.StatefulSet) bool {
	changed := false
	if !int32PtrEq(existing.Spec.Replicas, desired.Spec.Replicas) {
		existing.Spec.Replicas = desired.Spec.Replicas
		changed = true
	}
	if !apiequality.Semantic.DeepEqual(existing.Spec.Template, desired.Spec.Template) {
		existing.Spec.Template = desired.Spec.Template
		changed = true
	}
	if !apiequality.Semantic.DeepEqual(existing.Spec.UpdateStrategy, desired.Spec.UpdateStrategy) {
		existing.Spec.UpdateStrategy = desired.Spec.UpdateStrategy
		changed = true
	}
	if existing.Spec.MinReadySeconds != desired.Spec.MinReadySeconds {
		existing.Spec.MinReadySeconds = desired.Spec.MinReadySeconds
		changed = true
	}
	if !apiequality.Semantic.DeepEqual(existing.Labels, desired.Labels) {
		existing.Labels = desired.Labels
		changed = true
	}
	if !apiequality.Semantic.DeepEqual(existing.Annotations, desired.Annotations) {
		existing.Annotations = desired.Annotations
		changed = true
	}
	return changed
}

func int32PtrEq(a, b *int32) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}
