package node

import (
	"context"
	"fmt"

	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
)

// The pre-flight is a cluster-scoped READ and nothing more — the catalog is
// platform-owned (GitOps), so the controller never creates a class.
// get;list;watch is the whole set: the read goes through the manager's cached
// client, whose informer needs list and watch.
// +kubebuilder:rbac:groups=storage.k8s.io,resources=volumeattributesclasses,verbs=get;list;watch

// reconcileVolumeAttributesClass pre-flights the node's VolumeAttributesClass
// selection — a read-only Get of the named cluster-scoped class — and sets the
// always-present ConditionVolumeAttributesClassReady. It mutates node.Status
// in-memory only; the caller's single status patch flushes it. Run before the
// Failed/Paused early-returns so the condition is seeded on every path.
//
// It lives here, not in the ensure-data-pvc task that consumes it, because the
// task is not on every path: a Failed or Paused node returns early, a
// state-sync-gated node builds no plan, and — the case that decides it — an
// existing Running node with no drift builds no plan at all. A node that
// predates the field would then never acquire the condition, leaving a consumer
// to infer "not configured" from absence, which is exactly what the always-
// present discipline forbids.
//
// Sole writer of the condition. Enforcement lives downstream in the
// ensure-data-pvc task, which holds provisioning while this condition is not
// True (see holdForVolumeAttributesClass) — that keeps the hold on the one path
// that can bind a class name to a volume and keeps this method a resolver. No
// requeue is needed for a blocked selection: the only work a missing class
// blocks is provisioning, and the holding task's transient error already drives
// the executor's poll until the platform adds the class.
//
// Because the task consumes the PERSISTED condition, this resolve must keep
// running before plan execution — see the ordering note on
// holdForVolumeAttributesClass before moving either one.
//
// What it establishes is best-effort EXISTENCE at resolve time — not a binding
// guarantee, and not a claim about the class's contents either. Three limits,
// each deliberate:
//
//   - Freshness. The Get reads the manager's informer cache, which can lag the
//     API, and the class can be deleted between here and the Create the task
//     performs. A class yanked mid-provision still lands as a Pending volume,
//     and the condition re-resolves to False on the next reconcile.
//   - DRIVER MATCH — an explicit assumption, not an oversight. Nothing compares
//     vac.DriverName (read just below, for the message) against the CSI
//     provisioner of the StorageClass noderesource.StorageForNode picks for the
//     same claim. A same-named class belonging to a DIFFERENT driver pre-flights
//     True/VolumeAttributesClassFound and is then stamped onto a create-once PVC
//     that cannot provision. The assumption this pre-flight makes is
//     single-driver: the platform's VAC catalog and its StorageClasses belong to
//     the same CSI driver, and a cross-driver mismatch is a platform-catalog
//     error this check cannot see. Verifying it would need a `storageclasses`
//     read verb, and DR-001 pins the RBAC set to get;list;watch on
//     volumeattributesclasses and nothing else
//     (docs/specs/001-configurable-node-resources/decisions.md:124-126). Not
//     worth widening for a check that would still be advisory — a matching
//     driver name does not mean the driver accepts these parameters.
//   - Scope. It gates the CLAIM, not the pod: a False holds provisioning, and
//     the pod is still created and sits Pending on the claim nothing created.
//     Named rather than silent is the value, and the defect DR-001 commits
//     against; see the reason semantics on
//     seiv1alpha1.ConditionVolumeAttributesClassReady.
func (r *SeiNodeReconciler) reconcileVolumeAttributesClass(ctx context.Context, node *seiv1alpha1.SeiNode) {
	if dv := node.Spec.DataVolume; dv != nil && dv.Import != nil && dv.Import.PVCName != "" {
		// An imported volume keeps the importer's parameters — the controller
		// never stamps a class onto it — so there is no selection to pre-flight.
		setVolumeAttributesClassReady(node, metav1.ConditionFalse,
			seiv1alpha1.ReasonVolumeAttributesClassNotApplicable,
			fmt.Sprintf("data volume is imported from PVC %q, which keeps the importer's volume attributes", dv.Import.PVCName))
		return
	}

	name := noderesource.VolumeAttributesClassForNode(node)
	if name == nil {
		setVolumeAttributesClassReady(node, metav1.ConditionTrue,
			seiv1alpha1.ReasonModeDefaultStorage,
			"no volumeAttributesClassName selected; the mode-default storage class supplies the volume's performance")
		return
	}

	vac := &storagev1.VolumeAttributesClass{}
	switch err := r.Get(ctx, types.NamespacedName{Name: *name}, vac); {
	case err == nil:
		setVolumeAttributesClassReady(node, metav1.ConditionTrue,
			seiv1alpha1.ReasonVolumeAttributesClassFound,
			fmt.Sprintf("VolumeAttributesClass %q exists (driver %q)", *name, vac.DriverName))
	case apierrors.IsNotFound(err):
		setVolumeAttributesClassReady(node, metav1.ConditionFalse,
			seiv1alpha1.ReasonVolumeAttributesClassNotFound,
			fmt.Sprintf("VolumeAttributesClass %q not found; the platform must add it before this volume can be provisioned", *name))
	default:
		// Includes a cluster that does not serve storage.k8s.io
		// VolumeAttributesClasses at all, which surfaces as a no-matching-kind
		// error rather than not-found. Transient: the task holds and retries.
		setVolumeAttributesClassReady(node, metav1.ConditionFalse,
			seiv1alpha1.ReasonVolumeAttributesClassLookupError,
			fmt.Sprintf("reading VolumeAttributesClass %q: %v", *name, err))
	}
}

// setVolumeAttributesClassReady sets ConditionVolumeAttributesClassReady with
// ObservedGeneration stamped, following the always-present condition discipline.
func setVolumeAttributesClassReady(node *seiv1alpha1.SeiNode, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               seiv1alpha1.ConditionVolumeAttributesClassReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: node.Generation,
	})
}
