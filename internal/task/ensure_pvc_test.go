package task

import (
	"context"
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest"
)

// --- Fixtures ---

func ensurePVCScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := seiv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// ensurePVCNode returns a full-node SeiNode (uses default storage size).
func ensurePVCNode() *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "node-1",
			Namespace: "default",
			UID:       "uid-node-1",
		},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  "atlantic-2",
			Image:    "sei:v1.0.0",
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
		Status: seiv1alpha1.SeiNodeStatus{
			Plan: &seiv1alpha1.TaskPlan{
				Phase: seiv1alpha1.TaskPlanActive,
				Tasks: []seiv1alpha1.PlannedTask{
					{Type: TaskTypeEnsureDataPVC, ID: "ensure-1", Status: seiv1alpha1.TaskPending},
				},
			},
		},
	}
}

// importNode returns a SeiNode configured with spec.dataVolume.import.pvcName.
func importNode(pvcName string) *seiv1alpha1.SeiNode {
	n := ensurePVCNode()
	n.Spec.DataVolume = &seiv1alpha1.DataVolumeSpec{
		Import: &seiv1alpha1.DataVolumeImport{PVCName: pvcName},
	}
	return n
}

// validImportedPVC returns a PVC that satisfies all seven import requirements.
func validImportedPVC(name, ns string, capacity string) *corev1.PersistentVolumeClaim { //nolint:unparam // test helper designed for reuse
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
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

// validImportedPV returns a PV that matches the given PVC's capacity.
func validImportedPV(name, capacity string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse(capacity),
			},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound},
	}
}

// newEnsurePVCExec builds a task execution against a fake client populated
// with objs. The node is the resource carried in the ExecutionConfig.
func newEnsurePVCExec(t *testing.T, node *seiv1alpha1.SeiNode, objs ...client.Object) (TaskExecution, client.Client) {
	t.Helper()
	s := ensurePVCScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	cfg := ExecutionConfig{
		KubeClient: c,
		APIReader:  c,
		Scheme:     s,
		Resource:   node,
		Platform:   platformtest.Config(),
	}
	params := EnsureDataPVCParams{NodeName: node.Name, Namespace: node.Namespace}
	raw, _ := json.Marshal(params)
	exec, err := deserializeEnsureDataPVC("ensure-1", raw, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return exec, c
}

// importReasonFor returns the Reason of the ImportPVCReady condition, or
// "" if the condition is not set.
func importReasonFor(node *seiv1alpha1.SeiNode) string {
	for _, c := range node.Status.Conditions {
		if c.Type == seiv1alpha1.ConditionImportPVCReady {
			return c.Reason
		}
	}
	return ""
}

// --- Create-path tests ---

func TestEnsureDataPVC_Create_PVCMissing_CreatesAndCompletes(t *testing.T) {
	g := NewWithT(t)
	node := ensurePVCNode()
	exec, c := newEnsurePVCExec(t, node)

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionComplete))

	pvc := &corev1.PersistentVolumeClaim{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: noderesource.DataPVCName(node), Namespace: node.Namespace,
	}, pvc)).To(Succeed())
	g.Expect(pvc.Spec.AccessModes).To(ConsistOf(corev1.ReadWriteOnce))
}

func TestEnsureDataPVC_Create_PVCExistsOwnedByUs_Completes(t *testing.T) {
	g := NewWithT(t)
	node := ensurePVCNode()

	// Pre-existing PVC owned by this SeiNode (crash-recovery scenario).
	existing := noderesource.GenerateDataPVC(node, platformtest.Config())
	trueVal := true
	existing.OwnerReferences = []metav1.OwnerReference{{
		APIVersion:         seiv1alpha1.GroupVersion.String(),
		Kind:               "SeiNode",
		Name:               node.Name,
		UID:                node.UID,
		Controller:         &trueVal,
		BlockOwnerDeletion: &trueVal,
	}}

	exec, _ := newEnsurePVCExec(t, node, existing)

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionComplete))
}

func TestEnsureDataPVC_Create_PVCExistsNotOwned_TerminalError(t *testing.T) {
	g := NewWithT(t)
	node := ensurePVCNode()

	// Pre-existing PVC with no owner references — the #104 bug scenario.
	foreign := noderesource.GenerateDataPVC(node, platformtest.Config())
	foreign.OwnerReferences = nil

	exec, _ := newEnsurePVCExec(t, node, foreign)

	err := exec.Execute(context.Background())
	g.Expect(err).To(HaveOccurred())
	var termErr *TerminalError
	g.Expect(err).To(BeAssignableToTypeOf(termErr))
	g.Expect(err.Error()).To(ContainSubstring("already exists and is not owned"))
}

func TestEnsureDataPVC_Create_AlreadyExistsRace_Requeues(t *testing.T) {
	g := NewWithT(t)
	node := ensurePVCNode()

	// Start with no PVC in the cache. We'll use an intercepting client
	// that returns AlreadyExists on Create.
	s := ensurePVCScheme(t)
	base := fake.NewClientBuilder().WithScheme(s).Build()
	c := &raceClient{Client: base}

	cfg := ExecutionConfig{
		KubeClient: c,
		APIReader:  c,
		Scheme:     s,
		Resource:   node,
		Platform:   platformtest.Config(),
	}
	raw, _ := json.Marshal(EnsureDataPVCParams{NodeName: node.Name, Namespace: node.Namespace})
	exec, err := deserializeEnsureDataPVC("ensure-1", raw, cfg)
	g.Expect(err).NotTo(HaveOccurred())

	runErr := exec.Execute(context.Background())
	g.Expect(runErr).To(HaveOccurred())
	// Transient (non-terminal) — plain error, executor will retry.
	var termErr *TerminalError
	g.Expect(runErr).NotTo(BeAssignableToTypeOf(termErr))
	g.Expect(runErr.Error()).To(ContainSubstring("concurrently"))
}

// raceClient returns AlreadyExists on Create, simulating a race between
// Get-NotFound and Create.
type raceClient struct {
	client.Client
}

func (r *raceClient) Create(_ context.Context, _ client.Object, _ ...client.CreateOption) error {
	// Return a Kubernetes AlreadyExists error.
	return &statusErr{reason: metav1.StatusReasonAlreadyExists}
}

type statusErr struct{ reason metav1.StatusReason }

func (e *statusErr) Error() string { return string(e.reason) }
func (e *statusErr) Status() metav1.Status {
	return metav1.Status{Status: metav1.StatusFailure, Reason: e.reason}
}

// --- Import-path: happy path ---

func TestEnsureDataPVC_Import_AllRequirementsMet_Completes(t *testing.T) {
	g := NewWithT(t)
	node := importNode("data-archive-0-0")

	// full-node uses StorageSizeDefault = "2000Gi" per platformtest.Config.
	pvc := validImportedPVC("data-archive-0-0", "default", "2000Gi")
	pv := validImportedPV("pv-data-archive-0-0", "2000Gi")

	exec, _ := newEnsurePVCExec(t, node, pvc, pv)

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionComplete))
	g.Expect(importReasonFor(node)).To(Equal(seiv1alpha1.ReasonPVCValidated))
}

// --- Import-path: per-requirement failures ---

func TestEnsureDataPVC_Import_PVCNotFound_Transient(t *testing.T) {
	g := NewWithT(t)
	node := importNode("data-missing")
	exec, _ := newEnsurePVCExec(t, node)

	err := exec.Execute(context.Background())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
	g.Expect(importReasonFor(node)).To(Equal(seiv1alpha1.ReasonPVCNotReady))
}

func TestEnsureDataPVC_Import_PVCNotFound_ThenAppears_Completes(t *testing.T) {
	g := NewWithT(t)
	node := importNode("data-late")

	// First pass: no PVC yet.
	s := ensurePVCScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	cfg := ExecutionConfig{
		KubeClient: c, APIReader: c, Scheme: s, Resource: node, Platform: platformtest.Config(),
	}
	raw, _ := json.Marshal(EnsureDataPVCParams{NodeName: node.Name, Namespace: node.Namespace})
	exec, err := deserializeEnsureDataPVC("ensure-1", raw, cfg)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))

	// PVC and PV show up; on the next Execute, task completes.
	g.Expect(c.Create(context.Background(), validImportedPVC("data-late", "default", "2000Gi"))).To(Succeed())
	g.Expect(c.Create(context.Background(), validImportedPV("pv-data-late", "2000Gi"))).To(Succeed())

	// Re-deserialize (executor does this every reconcile).
	exec, err = deserializeEnsureDataPVC("ensure-1", raw, cfg)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionComplete))
}

func TestEnsureDataPVC_Import_PVCTerminating_Transient(t *testing.T) {
	g := NewWithT(t)
	node := importNode("data-term")
	now := metav1.Now()
	pvc := validImportedPVC("data-term", "default", "2000Gi")
	pvc.DeletionTimestamp = &now
	pvc.Finalizers = []string{"kubernetes.io/pvc-protection"}

	exec, _ := newEnsurePVCExec(t, node, pvc)

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
	g.Expect(importReasonFor(node)).To(Equal(seiv1alpha1.ReasonPVCNotReady))
}

func TestEnsureDataPVC_Import_PVCPending_Transient(t *testing.T) {
	g := NewWithT(t)
	node := importNode("data-pending")
	pvc := validImportedPVC("data-pending", "default", "2000Gi")
	pvc.Status.Phase = corev1.ClaimPending

	exec, _ := newEnsurePVCExec(t, node, pvc)

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
	g.Expect(importReasonFor(node)).To(Equal(seiv1alpha1.ReasonPVCNotReady))
}

func TestEnsureDataPVC_Import_PVCReleased_Transient(t *testing.T) {
	g := NewWithT(t)
	node := importNode("data-released")
	pvc := validImportedPVC("data-released", "default", "2000Gi")
	pvc.Status.Phase = corev1.PersistentVolumeClaimPhase("Released")

	exec, _ := newEnsurePVCExec(t, node, pvc)

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
	g.Expect(importReasonFor(node)).To(Equal(seiv1alpha1.ReasonPVCNotReady))
}

func TestEnsureDataPVC_Import_PVCLost_Terminal(t *testing.T) {
	g := NewWithT(t)
	node := importNode("data-lost")
	pvc := validImportedPVC("data-lost", "default", "2000Gi")
	pvc.Status.Phase = corev1.ClaimLost

	exec, _ := newEnsurePVCExec(t, node, pvc)

	err := exec.Execute(context.Background())
	g.Expect(err).To(HaveOccurred())
	var termErr *TerminalError
	g.Expect(err).To(BeAssignableToTypeOf(termErr))
	g.Expect(err.Error()).To(ContainSubstring("Lost"))
	g.Expect(importReasonFor(node)).To(Equal(seiv1alpha1.ReasonPVCInvalid))
}

func TestEnsureDataPVC_Import_WrongAccessMode_Terminal(t *testing.T) {
	g := NewWithT(t)
	node := importNode("data-rox")
	pvc := validImportedPVC("data-rox", "default", "2000Gi")
	pvc.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany}

	exec, _ := newEnsurePVCExec(t, node, pvc)

	err := exec.Execute(context.Background())
	g.Expect(err).To(HaveOccurred())
	var termErr *TerminalError
	g.Expect(err).To(BeAssignableToTypeOf(termErr))
	g.Expect(err.Error()).To(ContainSubstring("ReadWriteOnce"))
	g.Expect(importReasonFor(node)).To(Equal(seiv1alpha1.ReasonPVCInvalid))
}

// RWOP unblocks the SELinux-mount-labeling optimization on EKS 1.34
// (SELinuxMountReadWriteOncePod, GA since K8s 1.27, default-on) without
// needing the SELinuxMount feature gate that EKS doesn't expose. The
// validator must accept it as a single-writer access mode.
func TestEnsureDataPVC_Import_ReadWriteOncePod_AccessMode_Completes(t *testing.T) {
	g := NewWithT(t)
	node := importNode("data-rwop")
	pvc := validImportedPVC("data-rwop", "default", "2000Gi")
	pvc.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}
	pv := validImportedPV("pv-data-rwop", "2000Gi")
	pv.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}

	exec, _ := newEnsurePVCExec(t, node, pvc, pv)

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(importReasonFor(node)).To(Equal(seiv1alpha1.ReasonPVCValidated))
}

func TestEnsureDataPVC_Import_CapacityTooSmall_Terminal(t *testing.T) {
	g := NewWithT(t)
	node := importNode("data-small")
	pvc := validImportedPVC("data-small", "default", "100Gi") // needs 2000Gi
	pv := validImportedPV("pv-data-small", "100Gi")

	exec, _ := newEnsurePVCExec(t, node, pvc, pv)

	err := exec.Execute(context.Background())
	g.Expect(err).To(HaveOccurred())
	var termErr *TerminalError
	g.Expect(err).To(BeAssignableToTypeOf(termErr))
	g.Expect(err.Error()).To(ContainSubstring("less than required"))
	g.Expect(importReasonFor(node)).To(Equal(seiv1alpha1.ReasonPVCInvalid))
}

// The import floor is the RESOLVED size. Defense in depth only: import+storage
// together is rejected by admission, so this state is unreachable and only the
// fake client can build it; CapacityTooSmall_Terminal covers the reachable case.
func TestEnsureDataPVC_Import_FloorUsesResolvedSize(t *testing.T) {
	withCRDSize := func(node *seiv1alpha1.SeiNode, size string) *seiv1alpha1.SeiNode {
		node.Spec.DataVolume.Storage = &seiv1alpha1.DataVolumeStorage{
			Resources: &seiv1alpha1.VolumeClaimResources{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(size),
				},
			},
		}
		return node
	}

	t.Run("a 500Gi import passes when the CRD asks for 500Gi", func(t *testing.T) {
		g := NewWithT(t)
		node := withCRDSize(importNode("data-500"), "500Gi")
		exec, _ := newEnsurePVCExec(t, node,
			validImportedPVC("data-500", "default", "500Gi"),
			validImportedPV("pv-data-500", "500Gi"))

		g.Expect(exec.Execute(context.Background())).To(Succeed())
		g.Expect(importReasonFor(node)).To(Equal(seiv1alpha1.ReasonPVCValidated))
	})

	t.Run("the same import fails when the CRD asks for 2000Gi", func(t *testing.T) {
		g := NewWithT(t)
		node := withCRDSize(importNode("data-500"), "2000Gi")
		exec, _ := newEnsurePVCExec(t, node,
			validImportedPVC("data-500", "default", "500Gi"),
			validImportedPV("pv-data-500", "500Gi"))

		err := exec.Execute(context.Background())
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("less than required"))
		g.Expect(importReasonFor(node)).To(Equal(seiv1alpha1.ReasonPVCInvalid))
	})
}

func TestEnsureDataPVC_Import_CapacityUnset_Transient(t *testing.T) {
	g := NewWithT(t)
	node := importNode("data-nocap")
	pvc := validImportedPVC("data-nocap", "default", "2000Gi")
	pvc.Status.Capacity = nil // not reported yet

	exec, _ := newEnsurePVCExec(t, node, pvc)

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
	g.Expect(importReasonFor(node)).To(Equal(seiv1alpha1.ReasonPVCNotReady))
}

func TestEnsureDataPVC_Import_PVCapacityMismatch_Terminal(t *testing.T) {
	g := NewWithT(t)
	node := importNode("data-mismatch")
	pvc := validImportedPVC("data-mismatch", "default", "2000Gi")
	pv := validImportedPV("pv-data-mismatch", "3000Gi")

	exec, _ := newEnsurePVCExec(t, node, pvc, pv)

	err := exec.Execute(context.Background())
	g.Expect(err).To(HaveOccurred())
	var termErr *TerminalError
	g.Expect(err).To(BeAssignableToTypeOf(termErr))
	g.Expect(err.Error()).To(ContainSubstring("does not match"))
	g.Expect(importReasonFor(node)).To(Equal(seiv1alpha1.ReasonPVCInvalid))
}

func TestEnsureDataPVC_Import_PVMissing_Transient(t *testing.T) {
	g := NewWithT(t)
	node := importNode("data-nopv")
	pvc := validImportedPVC("data-nopv", "default", "2000Gi")

	exec, _ := newEnsurePVCExec(t, node, pvc) // no PV

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
	g.Expect(importReasonFor(node)).To(Equal(seiv1alpha1.ReasonPVCNotReady))
}

func TestEnsureDataPVC_Import_PVFailed_Terminal(t *testing.T) {
	g := NewWithT(t)
	node := importNode("data-pvfail")
	pvc := validImportedPVC("data-pvfail", "default", "2000Gi")
	pv := validImportedPV("pv-data-pvfail", "2000Gi")
	pv.Status.Phase = corev1.VolumeFailed

	exec, _ := newEnsurePVCExec(t, node, pvc, pv)

	err := exec.Execute(context.Background())
	g.Expect(err).To(HaveOccurred())
	var termErr *TerminalError
	g.Expect(err).To(BeAssignableToTypeOf(termErr))
	g.Expect(err.Error()).To(ContainSubstring("phase Failed"))
	g.Expect(importReasonFor(node)).To(Equal(seiv1alpha1.ReasonPVCInvalid))
}

// --- VolumeAttributesClass gate ---
//
// The node reconciler owns the pre-flight and the VolumeAttributesClassReady
// condition (see internal/controller/node/volumeattributesclass.go); it resolves
// both before the plan executes. The task's job is narrower: hold provisioning
// while that condition is not True, and never write it. These tests seed the
// condition the way the reconciler would have.

// vacNode returns a full-node SeiNode selecting the named VolumeAttributesClass.
func vacNode(name string) *seiv1alpha1.SeiNode {
	n := ensurePVCNode()
	n.Spec.DataVolume = &seiv1alpha1.DataVolumeSpec{
		Storage: &seiv1alpha1.DataVolumeStorage{VolumeAttributesClassName: &name},
	}
	return n
}

// seedVACCondition stands in for the reconciler's resolve, which always runs
// ahead of plan execution.
func seedVACCondition(node *seiv1alpha1.SeiNode, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               seiv1alpha1.ConditionVolumeAttributesClassReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: node.Generation,
	})
}

func vacConditionFor(node *seiv1alpha1.SeiNode) *metav1.Condition {
	for i := range node.Status.Conditions {
		if node.Status.Conditions[i].Type == seiv1alpha1.ConditionVolumeAttributesClassReady {
			return &node.Status.Conditions[i]
		}
	}
	return nil
}

func dataPVCFor(t *testing.T, c client.Client, node *seiv1alpha1.SeiNode) (*corev1.PersistentVolumeClaim, error) {
	t.Helper()
	pvc := &corev1.PersistentVolumeClaim{}
	err := c.Get(context.Background(), types.NamespacedName{
		Name: noderesource.DataPVCName(node), Namespace: node.Namespace,
	}, pvc)
	return pvc, err
}

// A resolved-True selection provisions, and the claim carries the name. The
// condition must come back byte-identical: the reconciler is its only writer, so
// a task-side rewrite would be a second writer racing the same status patch.
func TestEnsureDataPVC_VAC_ConditionTrue_StampsAndLeavesConditionAlone(t *testing.T) {
	g := NewWithT(t)
	node := vacNode("vac-alpha")
	seedVACCondition(node, metav1.ConditionTrue,
		seiv1alpha1.ReasonVolumeAttributesClassFound, "resolved by the reconciler")
	before := *vacConditionFor(node)

	exec, c := newEnsurePVCExec(t, node)

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionComplete))

	pvc, err := dataPVCFor(t, c, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pvc.Spec.VolumeAttributesClassName).NotTo(BeNil())
	g.Expect(*pvc.Spec.VolumeAttributesClassName).To(Equal("vac-alpha"))

	g.Expect(*vacConditionFor(node)).To(Equal(before),
		"the task must not write the condition — the reconciler is its single writer")
}

// The landmine the gate defuses: the PVC is created once, so a claim must not be
// created against a class that did not pre-flight True. The hold is transient,
// so the platform adding the class releases it on the next poll.
func TestEnsureDataPVC_VAC_ConditionNotTrue_HoldsProvision(t *testing.T) {
	for _, tc := range []struct{ name, reason string }{
		{"class missing", seiv1alpha1.ReasonVolumeAttributesClassNotFound},
		{"class unreadable", seiv1alpha1.ReasonVolumeAttributesClassLookupError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			node := vacNode("vac-absent")
			seedVACCondition(node, metav1.ConditionFalse, tc.reason, "resolved by the reconciler")

			exec, c := newEnsurePVCExec(t, node)

			err := exec.Execute(context.Background())
			g.Expect(err).To(HaveOccurred())
			var termErr *TerminalError
			g.Expect(err).NotTo(BeAssignableToTypeOf(termErr),
				"a missing class is fixed by adding it, so the task must retry rather than park the node in Failed")
			g.Expect(err.Error()).To(ContainSubstring("vac-absent"))
			g.Expect(err.Error()).To(ContainSubstring(tc.reason))

			_, getErr := dataPVCFor(t, c, node)
			g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(),
				"no PVC may be created while the selection has not pre-flighted True — the claim binds the name once")
		})
	}
}

// Fails closed on an unresolved condition. Unreachable while the reconciler
// resolves ahead of plan execution; pinned so a reordering cannot silently
// start provisioning against an unverified name.
func TestEnsureDataPVC_VAC_ConditionAbsent_HoldsProvision(t *testing.T) {
	g := NewWithT(t)
	node := vacNode("vac-alpha")
	g.Expect(vacConditionFor(node)).To(BeNil())

	exec, c := newEnsurePVCExec(t, node)

	err := exec.Execute(context.Background())
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("has not resolved"))

	_, getErr := dataPVCFor(t, c, node)
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue())
}

// The handoff: the reconciler re-resolves each reconcile, so the same task
// proceeds once the platform has added the class.
func TestEnsureDataPVC_VAC_ConditionFlipsTrue_Proceeds(t *testing.T) {
	g := NewWithT(t)
	node := vacNode("vac-late")
	seedVACCondition(node, metav1.ConditionFalse,
		seiv1alpha1.ReasonVolumeAttributesClassNotFound, "not found yet")

	s := ensurePVCScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	cfg := ExecutionConfig{
		KubeClient: c, APIReader: c, Scheme: s, Resource: node, Platform: platformtest.Config(),
	}
	raw, _ := json.Marshal(EnsureDataPVCParams{NodeName: node.Name, Namespace: node.Namespace})

	exec, err := deserializeEnsureDataPVC("ensure-1", raw, cfg)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(exec.Execute(context.Background())).To(HaveOccurred())

	// The platform adds the class; the next reconcile's resolve flips the gate.
	seedVACCondition(node, metav1.ConditionTrue,
		seiv1alpha1.ReasonVolumeAttributesClassFound, "found")

	exec, err = deserializeEnsureDataPVC("ensure-1", raw, cfg)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionComplete))

	pvc, getErr := dataPVCFor(t, c, node)
	g.Expect(getErr).NotTo(HaveOccurred())
	g.Expect(*pvc.Spec.VolumeAttributesClassName).To(Equal("vac-late"))
}

// A selection-free node can never be held by the gate, even with no condition
// resolved at all — there is no reference to verify, and a false block here
// would stall every node that never asked for a class.
func TestEnsureDataPVC_VAC_NoSelection_NeverHeld(t *testing.T) {
	for name, dv := range map[string]*seiv1alpha1.DataVolumeSpec{
		"dataVolume absent": nil,
		"size only": {Storage: &seiv1alpha1.DataVolumeStorage{
			Resources: &seiv1alpha1.VolumeClaimResources{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("500Gi")},
			},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			node := ensurePVCNode()
			node.Spec.DataVolume = dv
			exec, c := newEnsurePVCExec(t, node)

			g.Expect(exec.Execute(context.Background())).To(Succeed())
			g.Expect(exec.Status(context.Background())).To(Equal(ExecutionComplete))

			pvc, err := dataPVCFor(t, c, node)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pvc.Spec.VolumeAttributesClassName).To(BeNil(),
				"with no selection the claim carries no volumeAttributesClassName at all")
		})
	}
}

// Req 3.3: a node that imports a pre-existing volume keeps the importer's
// parameters. The sentinel is what makes this test bite — a nil-to-nil assertion
// would pass whether or not anything is preserved.
func TestEnsureDataPVC_VAC_Import_PreservesImporterSelection(t *testing.T) {
	g := NewWithT(t)
	const importerVAC = "importer-owned-class"
	node := importNode("data-imported")
	pvc := validImportedPVC("data-imported", "default", "2000Gi")
	pvc.Spec.VolumeAttributesClassName = ptr.To(importerVAC)
	pv := validImportedPV("pv-data-imported", "2000Gi")

	exec, c := newEnsurePVCExec(t, node, pvc, pv)

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionComplete))

	got := &corev1.PersistentVolumeClaim{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: "data-imported", Namespace: "default",
	}, got)).To(Succeed())
	g.Expect(got.Spec.VolumeAttributesClassName).NotTo(BeNil(),
		"the controller must not strip the importer's volume attributes")
	g.Expect(*got.Spec.VolumeAttributesClassName).To(Equal(importerVAC),
		"the importer's class must survive untouched")
}

// --- Poll cadence regression guard ---

func TestEnsureDataPVC_Import_TransientValidationRepeats(t *testing.T) {
	g := NewWithT(t)
	node := importNode("data-stuck")

	s := ensurePVCScheme(t)
	base := fake.NewClientBuilder().WithScheme(s).Build()
	counter := &countingClient{Client: base}
	cfg := ExecutionConfig{
		KubeClient: counter, APIReader: counter, Scheme: s, Resource: node, Platform: platformtest.Config(),
	}
	raw, _ := json.Marshal(EnsureDataPVCParams{NodeName: node.Name, Namespace: node.Namespace})

	// Simulate N reconciles against a persistently-missing PVC. Each should
	// issue exactly one Get (no extra polling or stored state).
	const reconciles = 3
	for range reconciles {
		exec, err := deserializeEnsureDataPVC("ensure-1", raw, cfg)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(exec.Execute(context.Background())).To(Succeed())
		g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
	}
	g.Expect(counter.getCount).To(Equal(reconciles),
		"expected one Get per reconcile when stuck in transient-missing")
}

// countingClient counts Get calls.
type countingClient struct {
	client.Client
	getCount int
}

func (c *countingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.getCount++
	return c.Client.Get(ctx, key, obj, opts...)
}
