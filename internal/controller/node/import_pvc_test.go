package node

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// importTestNode returns a SeiNode wired for importing a pre-existing PVC.
func importTestNode(name, namespace, pvcName string) *seiv1alpha1.SeiNode { //nolint:unparam // test helper designed for reuse
	n := newSnapshotNode(name, namespace)
	n.Spec.DataVolume = &seiv1alpha1.DataVolumeSpec{
		Import: &seiv1alpha1.DataVolumeImport{PVCName: pvcName},
	}
	return n
}

// boundPVC returns a validly-configured PVC for import tests.
func boundPVC(name, namespace, capacity string) *corev1.PersistentVolumeClaim { //nolint:unparam // test helper designed for reuse
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			VolumeName:  "pv-" + name,
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound,
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse(capacity),
			},
		},
	}
}

// boundPV returns a matching PV.
func boundPV(name, capacity string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(capacity)},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound},
	}
}

func TestController_Import_Happy_ReachesRunning(t *testing.T) {
	g := NewWithT(t)

	node := importTestNode("imp-0", "default", "external-data-0")
	pvc := boundPVC("external-data-0", "default", "2000Gi")
	pv := boundPV("pv-external-data-0", "2000Gi")

	mock := &mockSidecarClient{}
	r, c := newProgressionReconciler(t, mock, node, pvc, pv)

	fetch := func() *seiv1alpha1.SeiNode {
		return fetchNode(t, c, node.Name, node.Namespace)
	}

	// Reconcile 1: finalizer + build plan.
	reconcileOnce(t, g, r, node.Name, node.Namespace)
	n := fetch()
	g.Expect(n.Status.Plan).NotTo(BeNil())

	// Reconcile 2: drives import validation to completion and advances
	// through the remaining infrastructure tasks.
	reconcileOnce(t, g, r, node.Name, node.Namespace)
	n = fetch()
	cond := meta.FindStatusCondition(n.Status.Conditions, seiv1alpha1.ConditionImportPVCReady)
	g.Expect(cond).NotTo(BeNil(), "ImportPVCReady condition should be set after validation")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonPVCValidated))

	// Drive the sidecar progression through to completion.
	driveTask(t, g, r, mock, fetch, "snapshot-restore")
	driveTask(t, g, r, mock, fetch, "configure-genesis")
	driveTask(t, g, r, mock, fetch, "config-apply")
	driveTask(t, g, r, mock, fetch, "configure-state-sync")

	updated := fetch()
	g.Expect(updated.Status.Phase).To(Equal(seiv1alpha1.PhaseRunning))

	// Condition stays True once init completes (plan goes away).
	cond = meta.FindStatusCondition(updated.Status.Conditions, seiv1alpha1.ConditionImportPVCReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
}

func TestController_Import_LatePVC_Converges(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := importTestNode("imp-late", "default", "late-pvc")

	mock := &mockSidecarClient{}
	r, c := newProgressionReconciler(t, mock, node)

	fetch := func() *seiv1alpha1.SeiNode {
		return fetchNode(t, c, node.Name, node.Namespace)
	}

	// Reconcile 1: finalizer + build plan. Reconcile 2: first validation pass
	// sees no PVC → transient, condition goes False/PVCNotFound.
	reconcileOnce(t, g, r, node.Name, node.Namespace)
	reconcileOnce(t, g, r, node.Name, node.Namespace)

	n := fetch()
	g.Expect(n.Status.Phase).To(Equal(seiv1alpha1.PhaseInitializing))
	cond := meta.FindStatusCondition(n.Status.Conditions, seiv1alpha1.ConditionImportPVCReady)
	g.Expect(cond).NotTo(BeNil(), "ImportPVCReady condition should be set while stuck")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonPVCNotReady))

	// Operator creates the PVC and PV out-of-band.
	g.Expect(c.Create(ctx, boundPVC("late-pvc", "default", "2000Gi"))).To(Succeed())
	g.Expect(c.Create(ctx, boundPV("pv-late-pvc", "2000Gi"))).To(Succeed())

	// Next reconcile: validation passes, task completes, plan advances.
	reconcileOnce(t, g, r, node.Name, node.Namespace)

	n = fetch()
	cond = meta.FindStatusCondition(n.Status.Conditions, seiv1alpha1.ConditionImportPVCReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonPVCValidated))
}

func TestController_Import_TerminalFailure_MarksFailed(t *testing.T) {
	g := NewWithT(t)

	node := importTestNode("imp-bad", "default", "bad-access")
	pvc := boundPVC("bad-access", "default", "2000Gi")
	pvc.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany}

	mock := &mockSidecarClient{}
	r, c := newProgressionReconciler(t, mock, node, pvc)

	fetch := func() *seiv1alpha1.SeiNode {
		return fetchNode(t, c, node.Name, node.Namespace)
	}

	// Reconcile 1: build plan. Reconcile 2: execute ensure-data-pvc,
	// which returns Terminal and fails the plan. Reconcile 3: planner
	// observes the failed plan and clears it.
	reconcileOnce(t, g, r, node.Name, node.Namespace)
	reconcileOnce(t, g, r, node.Name, node.Namespace)

	n := fetch()
	g.Expect(n.Status.Phase).To(Equal(seiv1alpha1.PhaseFailed))
	g.Expect(n.Status.Plan).NotTo(BeNil())
	g.Expect(n.Status.Plan.Phase).To(Equal(seiv1alpha1.TaskPlanFailed))
	g.Expect(n.Status.Plan.FailedTaskDetail).NotTo(BeNil())
	g.Expect(n.Status.Plan.FailedTaskDetail.Error).To(ContainSubstring("ReadWriteOnce"))

	cond := meta.FindStatusCondition(n.Status.Conditions, seiv1alpha1.ConditionImportPVCReady)
	g.Expect(cond).NotTo(BeNil(), "condition should reflect the terminal failure")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonPVCInvalid))
}

func TestController_Import_Deletion_PreservesPVC(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := importTestNode("imp-del", "default", "preserve-me")
	node.Finalizers = []string{nodeFinalizerName}
	pvc := boundPVC("preserve-me", "default", "2000Gi")

	mock := &mockSidecarClient{}
	r, c := newProgressionReconciler(t, mock, node, pvc)

	g.Expect(c.Delete(ctx, node)).To(Succeed())
	// Re-fetch after delete so DeletionTimestamp is populated.
	_ = c.Get(ctx, types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, node)

	_, err := r.Reconcile(ctx, nodeReqFor(node.Name, node.Namespace))
	g.Expect(err).NotTo(HaveOccurred())

	// PVC must still exist — imported PVCs are never deleted by the controller.
	remaining := &corev1.PersistentVolumeClaim{}
	err = c.Get(ctx, types.NamespacedName{Name: "preserve-me", Namespace: "default"}, remaining)
	g.Expect(err).NotTo(HaveOccurred(), "imported PVC must be preserved across SeiNode deletion")
}

func TestController_NoImport_Deletion_DeletesPVC(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	// Node without dataVolume.import — standard create-path; PVC should be
	// cleaned up by the finalizer on deletion.
	node := newSnapshotNode("noimp-del", "default")
	node.Finalizers = []string{nodeFinalizerName}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "data-noimp-del",
			Namespace: "default",
		},
	}

	mock := &mockSidecarClient{}
	r, c := newProgressionReconciler(t, mock, node, pvc)

	g.Expect(c.Delete(ctx, node)).To(Succeed())
	_ = c.Get(ctx, types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, node)

	_, err := r.Reconcile(ctx, nodeReqFor(node.Name, node.Namespace))
	g.Expect(err).NotTo(HaveOccurred())

	remaining := &corev1.PersistentVolumeClaim{}
	err = c.Get(ctx, types.NamespacedName{Name: "data-noimp-del", Namespace: "default"}, remaining)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "non-imported PVC should be deleted")
}
