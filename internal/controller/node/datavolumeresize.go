package node

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
)

// A node with spec.nodeConfig may grow spec.dataVolume.storage after creation
// (spec 011). Two halves serve it, split the way the reconcile already splits
// condition resolution from owned-resource writes:
//
//   - reconcileDataVolumeResize resolves the always-present
//     DataVolumeResizeInProgress condition. It only reads, so it runs before the
//     Failed and Paused early returns and the condition rides every path.
//   - growDataPVC raises the owned PVC's storage request. It is a steady-state
//     write, like reconcileStatefulSet, and runs after the Paused early return:
//     spec.paused promises no derived-resource mutation beyond the StatefulSet.
//
// The PVC watch (Owns) wakes the node when the provider reports new capacity,
// so the condition follows the growth without polling.

// reconcileDataVolumeResize sets ConditionDataVolumeResizeInProgress from the
// spec and the owned PVC. It never writes the PVC.
func (r *SeiNodeReconciler) reconcileDataVolumeResize(ctx context.Context, node *seiv1alpha1.SeiNode) {
	want, reason := growableSize(node)
	if want == nil {
		setDataVolumeResize(node, metav1.ConditionFalse, seiv1alpha1.ReasonDataVolumeResizeNotApplicable, reason)
		return
	}

	pvc, err := r.ownedDataPVC(ctx, node)
	switch {
	case apierrors.IsNotFound(err):
		setDataVolumeResize(node, metav1.ConditionFalse, seiv1alpha1.ReasonDataVolumeResizeNotApplicable,
			fmt.Sprintf("data PVC %q does not exist yet; ensure-data-pvc provisions it at %s",
				noderesource.DataPVCName(node), want.String()))
		return
	case err != nil:
		setDataVolumeResize(node, metav1.ConditionUnknown, seiv1alpha1.ReasonDataVolumePVCLookupError,
			fmt.Sprintf("reading data PVC %q: %v", noderesource.DataPVCName(node), err))
		return
	case pvc == nil:
		setDataVolumeResize(node, metav1.ConditionFalse, seiv1alpha1.ReasonDataVolumeResizeNotApplicable,
			fmt.Sprintf("data PVC %q is not controlled by this SeiNode, so the controller never resizes it",
				noderesource.DataPVCName(node)))
		return
	}

	if pvc.Status.Phase != corev1.ClaimBound {
		setDataVolumeResize(node, metav1.ConditionFalse, seiv1alpha1.ReasonDataVolumeResizeNotApplicable,
			fmt.Sprintf("data PVC %q is %s, not Bound; a claim grows only once it is bound", pvc.Name, pvc.Status.Phase))
		return
	}

	capacity := pvc.Status.Capacity[corev1.ResourceStorage]
	if capacity.Cmp(*want) >= 0 {
		setDataVolumeResize(node, metav1.ConditionFalse, seiv1alpha1.ReasonDataVolumeResizeComplete,
			fmt.Sprintf("data PVC %q capacity %s meets the requested %s", pvc.Name, capacity.String(), want.String()))
		return
	}

	msg := fmt.Sprintf("data PVC %q capacity %s, requested %s", pvc.Name, capacity.String(), want.String())
	if detail := pvcResizeDetail(pvc); detail != "" {
		msg += "; " + detail
	}
	if node.Spec.Paused {
		msg += "; the controller raises the claim's request only while spec.paused is false"
	}
	setDataVolumeResize(node, metav1.ConditionTrue, seiv1alpha1.ReasonDataVolumeResizing, msg)
}

// growDataPVC raises the owned data PVC's storage request to the node's
// growable size. It never lowers a request — a size added below the claim's
// current request changes nothing — and never touches an imported or
// unowned PVC. A refused patch — for example a StorageClass without
// allowVolumeExpansion — sets ResizeFailed and does not fail the reconcile: the
// plan work below must still run, and a Running node requeues on
// statusPollInterval, which retries the patch.
func (r *SeiNodeReconciler) growDataPVC(ctx context.Context, node *seiv1alpha1.SeiNode) {
	want, _ := growableSize(node)
	if want == nil {
		return
	}
	pvc, err := r.ownedDataPVC(ctx, node)
	if err != nil || pvc == nil {
		// reconcileDataVolumeResize already reported why; nothing to grow yet.
		return
	}

	current := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if current.Cmp(*want) >= 0 {
		return
	}

	patch := client.MergeFrom(pvc.DeepCopy())
	if pvc.Spec.Resources.Requests == nil {
		pvc.Spec.Resources.Requests = corev1.ResourceList{}
	}
	pvc.Spec.Resources.Requests[corev1.ResourceStorage] = *want
	if err := r.Patch(ctx, pvc, patch); err != nil {
		log.FromContext(ctx).Error(err, "raising data PVC storage request", "pvc", pvc.Name)
		setDataVolumeResize(node, metav1.ConditionTrue, seiv1alpha1.ReasonDataVolumeResizeFailed,
			fmt.Sprintf("raising data PVC %q request from %s to %s: %v", pvc.Name, current.String(), want.String(), err))
		r.Recorder.Eventf(node, corev1.EventTypeWarning, "DataVolumeResizeFailed",
			"Cannot raise data PVC %s request from %s to %s: %v", pvc.Name, current.String(), want.String(), err)
		return
	}
	r.Recorder.Eventf(node, corev1.EventTypeNormal, "DataVolumeResizeRequested",
		"Raised data PVC %s request from %s to %s", pvc.Name, current.String(), want.String())
}

// growableSize returns the size the node's volume must reach, or nil with the
// reason the node cannot grow it.
func growableSize(node *seiv1alpha1.SeiNode) (*resource.Quantity, string) {
	if dv := node.Spec.DataVolume; dv != nil && dv.Import != nil && dv.Import.PVCName != "" {
		return nil, fmt.Sprintf("data volume is imported from PVC %q; the importer owns its size", dv.Import.PVCName)
	}
	if node.Spec.NodeConfig == nil {
		return nil, "only a node with spec.nodeConfig can grow its data volume after creation"
	}
	want := noderesource.GrowableStorageSize(node)
	if want == nil {
		return nil, "spec.dataVolume.storage.resources.requests.storage is unset; set it to grow the volume"
	}
	return want, ""
}

// ownedDataPVC reads the node's data PVC. It returns (nil, nil) for a claim the
// node does not control, and the Get error otherwise, NotFound included.
func (r *SeiNodeReconciler) ownedDataPVC(ctx context.Context, node *seiv1alpha1.SeiNode) (*corev1.PersistentVolumeClaim, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	key := types.NamespacedName{Name: noderesource.DataPVCName(node), Namespace: node.Namespace}
	if err := r.Get(ctx, key, pvc); err != nil {
		return nil, err
	}
	if !metav1.IsControlledBy(pvc, node) {
		return nil, nil
	}
	return pvc, nil
}

// pvcResizeDetail repeats the PVC's own resize signals: the resize conditions
// that are True, and the allocated-resource status for storage.
func pvcResizeDetail(pvc *corev1.PersistentVolumeClaim) string {
	var parts []string
	for _, c := range pvc.Status.Conditions {
		switch c.Type {
		case corev1.PersistentVolumeClaimResizing,
			corev1.PersistentVolumeClaimFileSystemResizePending,
			corev1.PersistentVolumeClaimControllerResizeError,
			corev1.PersistentVolumeClaimNodeResizeError:
		default:
			continue
		}
		if c.Status != corev1.ConditionTrue {
			continue
		}
		part := string(c.Type)
		if c.Message != "" {
			part += ": " + c.Message
		}
		parts = append(parts, part)
	}
	if s, ok := pvc.Status.AllocatedResourceStatuses[corev1.ResourceStorage]; ok {
		parts = append(parts, "allocatedResourceStatus: "+string(s))
	}
	return strings.Join(parts, "; ")
}

// setDataVolumeResize sets ConditionDataVolumeResizeInProgress with
// ObservedGeneration stamped, following the always-present condition discipline.
func setDataVolumeResize(node *seiv1alpha1.SeiNode, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               seiv1alpha1.ConditionDataVolumeResizeInProgress,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: node.Generation,
	})
}
