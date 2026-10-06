package node

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// Spec 011 Requirements 3 and 4: the reconciler grows a nodeConfig node's owned
// data PVC to spec.dataVolume.storage, never lowers it, never touches an
// imported or unowned claim, and reports progress on an always-present
// DataVolumeResizeInProgress condition.

const resizeNodeUID = types.UID("resize-node-uid")

// resizeNode returns a full node; nodeConfig and size are optional.
func resizeNode(name string, nodeConfig bool, size string) *seiv1alpha1.SeiNode {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, UID: resizeNodeUID, Generation: 4},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  testChainID,
			Image:    testImage,
			FullNode: &seiv1alpha1.FullNodeSpec{},
			Sidecar:  &seiv1alpha1.SidecarConfig{Port: 7777},
		},
	}
	if nodeConfig {
		node.Spec.NodeConfig = &seiv1alpha1.NodeConfig{
			ConfigRef: seiv1alpha1.ConfigFileRef{Name: "rpc-config-v1"},
			AppRef:    seiv1alpha1.ConfigFileRef{Name: "rpc-app-v1"},
		}
	}
	if size != "" {
		node.Spec.DataVolume = &seiv1alpha1.DataVolumeSpec{Storage: &seiv1alpha1.DataVolumeStorage{
			Resources: &seiv1alpha1.VolumeClaimResources{Requests: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse(size),
			}},
		}}
	}
	return node
}

// dataPVC returns the node's bound data PVC with the given request and
// capacity, controlled by the node unless owned is false.
func dataPVC(node *seiv1alpha1.SeiNode, request, capacity string, owned bool) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-" + node.Name, Namespace: node.Namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse(request),
			}},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase:    corev1.ClaimBound,
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(capacity)},
		},
	}
	if owned {
		pvc.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: seiv1alpha1.GroupVersion.String(),
			Kind:       "SeiNode",
			Name:       node.Name,
			UID:        node.UID,
			Controller: new(true),
		}}
	}
	return pvc
}

func resizeCondition(node *seiv1alpha1.SeiNode) *metav1.Condition {
	return apimeta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionDataVolumeResizeInProgress)
}

func pvcRequest(t *testing.T, c client.Client, node *seiv1alpha1.SeiNode) string {
	t.Helper()
	pvc := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "data-" + node.Name, Namespace: node.Namespace}, pvc); err != nil {
		t.Fatal(err)
	}
	q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	return q.String()
}

// --- Condition branch matrix (011 Req 4) ---

func TestDataVolumeResize_NotApplicable(t *testing.T) {
	imported := resizeNode("dvr-import", true, "")
	imported.Spec.DataVolume = &seiv1alpha1.DataVolumeSpec{Import: &seiv1alpha1.DataVolumeImport{PVCName: "adopted"}}

	cases := map[string]struct {
		node    *seiv1alpha1.SeiNode
		objs    func(*seiv1alpha1.SeiNode) []client.Object
		message string
	}{
		// 011 Req 4.5: no nodeConfig.
		"no nodeConfig": {node: resizeNode("dvr-plain", false, "2Ti"), message: "only a node with spec.nodeConfig"},
		// 011 Req 4.5: an imported PVC.
		"imported PVC":        {node: imported, message: "imported from PVC"},
		"no size set":         {node: resizeNode("dvr-nosize", true, ""), message: "is unset"},
		"PVC not created yet": {node: resizeNode("dvr-nopvc", true, "2Ti"), message: "does not exist yet"},
		"unowned PVC": {
			node:    resizeNode("dvr-unowned", true, "2Ti"),
			objs:    func(n *seiv1alpha1.SeiNode) []client.Object { return []client.Object{dataPVC(n, "1Ti", "1Ti", false)} },
			message: "not controlled by this SeiNode",
		},
		"unbound PVC": {
			node: resizeNode("dvr-unbound", true, "2Ti"),
			objs: func(n *seiv1alpha1.SeiNode) []client.Object {
				pvc := dataPVC(n, "1Ti", "1Ti", true)
				pvc.Status.Phase = corev1.ClaimPending
				pvc.Status.Capacity = nil
				return []client.Object{pvc}
			},
			message: "not Bound",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			objs := []client.Object{tc.node}
			if tc.objs != nil {
				objs = append(objs, tc.objs(tc.node)...)
			}
			r, _ := newNodeReconciler(t, objs...)

			r.reconcileDataVolumeResize(context.Background(), tc.node)

			cond := resizeCondition(tc.node)
			g.Expect(cond).NotTo(BeNil(), "the condition is always present")
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonDataVolumeResizeNotApplicable))
			g.Expect(cond.Message).To(ContainSubstring(tc.message))
			g.Expect(cond.ObservedGeneration).To(Equal(tc.node.Generation))
		})
	}
}

// 011 Req 4.2: capacity below the request reads Resizing and repeats the PVC's
// own resize signals.
func TestDataVolumeResize_ResizingCarriesPVCSignals(t *testing.T) {
	g := NewWithT(t)
	node := resizeNode("dvr-resizing", true, "2Ti")
	pvc := dataPVC(node, "2Ti", "1Ti", true)
	pvc.Status.Conditions = []corev1.PersistentVolumeClaimCondition{
		{Type: corev1.PersistentVolumeClaimResizing, Status: corev1.ConditionTrue, Message: "waiting for the volume modification"},
		{Type: corev1.PersistentVolumeClaimFileSystemResizePending, Status: corev1.ConditionFalse},
	}
	pvc.Status.AllocatedResourceStatuses = map[corev1.ResourceName]corev1.ClaimResourceStatus{
		corev1.ResourceStorage: corev1.PersistentVolumeClaimControllerResizeInProgress,
	}
	r, _ := newNodeReconciler(t, node, pvc)

	r.reconcileDataVolumeResize(context.Background(), node)

	cond := resizeCondition(node)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonDataVolumeResizing))
	g.Expect(cond.Message).To(ContainSubstring("capacity 1Ti, requested 2Ti"))
	g.Expect(cond.Message).To(ContainSubstring("Resizing: waiting for the volume modification"))
	g.Expect(cond.Message).To(ContainSubstring("allocatedResourceStatus: ControllerResizeInProgress"))
	g.Expect(cond.Message).NotTo(ContainSubstring("FileSystemResizePending"), "a False PVC condition is not a signal")
}

// 011 Req 4.4: capacity at or above the request reads ResizeComplete.
func TestDataVolumeResize_Complete(t *testing.T) {
	g := NewWithT(t)
	node := resizeNode("dvr-complete", true, "2Ti")
	r, _ := newNodeReconciler(t, node, dataPVC(node, "2Ti", "2Ti", true))

	r.reconcileDataVolumeResize(context.Background(), node)

	cond := resizeCondition(node)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonDataVolumeResizeComplete))
}

// --- PVC growth (011 Req 3) ---

// 011 Req 3.1: a grown size raises the owned PVC's request.
func TestGrowDataPVC_RaisesRequest(t *testing.T) {
	g := NewWithT(t)
	node := resizeNode("dvr-grow", true, "2Ti")
	r, c := newNodeReconciler(t, node, dataPVC(node, "1Ti", "1Ti", true))

	r.growDataPVC(context.Background(), node)

	g.Expect(pvcRequest(t, c, node)).To(Equal("2Ti"))
	g.Expect(drainEvents(r)).To(ContainElement(ContainSubstring("DataVolumeResizeRequested")))
}

// 011 Req 3.2: a request already above the spec is never lowered.
func TestGrowDataPVC_NeverLowers(t *testing.T) {
	g := NewWithT(t)
	node := resizeNode("dvr-nolower", true, "1Ti")
	r, c := newNodeReconciler(t, node, dataPVC(node, "2Ti", "2Ti", true))

	r.growDataPVC(context.Background(), node)

	g.Expect(pvcRequest(t, c, node)).To(Equal("2Ti"))
	g.Expect(drainEvents(r)).To(BeEmpty())
}

// 011 Req 2.5 with Req 3.2: a size added after creation below the claim's
// current request changes nothing, and the condition reads ResizeComplete
// because the capacity already meets it.
func TestGrowDataPVC_AddedSizeBelowRequestLeavesClaimAlone(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	// The claim was provisioned at the per-mode default before any size was set;
	// the operator then adds a smaller explicit size.
	node := resizeNode("dvr-added-below", true, "1Ti")
	r, c := newNodeReconciler(t, node, dataPVC(node, "2Ti", "2Ti", true))

	r.reconcileDataVolumeResize(ctx, node)
	r.growDataPVC(ctx, node)

	g.Expect(pvcRequest(t, c, node)).To(Equal("2Ti"), "an added size below the request must not lower it")
	g.Expect(drainEvents(r)).To(BeEmpty(), "nothing was written, so nothing is reported")
	cond := resizeCondition(node)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonDataVolumeResizeComplete))
}

// 011 Req 3.3: an unowned claim is never written, and neither is a claim on a
// node without nodeConfig.
func TestGrowDataPVC_LeavesOtherClaimsAlone(t *testing.T) {
	t.Run("unowned", func(t *testing.T) {
		g := NewWithT(t)
		node := resizeNode("dvr-unowned-grow", true, "2Ti")
		r, c := newNodeReconciler(t, node, dataPVC(node, "1Ti", "1Ti", false))

		r.growDataPVC(context.Background(), node)
		g.Expect(pvcRequest(t, c, node)).To(Equal("1Ti"))
	})
	t.Run("no nodeConfig", func(t *testing.T) {
		g := NewWithT(t)
		node := resizeNode("dvr-plain-grow", false, "2Ti")
		r, c := newNodeReconciler(t, node, dataPVC(node, "1Ti", "1Ti", true))

		r.growDataPVC(context.Background(), node)
		g.Expect(pvcRequest(t, c, node)).To(Equal("1Ti"))
	})
}

// 011 Req 4.3: a refused PVC update reads ResizeFailed with the API error, and
// records a Warning event, without failing anything else.
func TestGrowDataPVC_RefusedPatchReportsResizeFailed(t *testing.T) {
	g := NewWithT(t)
	node := resizeNode("dvr-refused", true, "2Ti")
	pvc := dataPVC(node, "1Ti", "1Ti", true)

	s := newNodeTestScheme(t)
	base := fake.NewClientBuilder().WithScheme(s).WithObjects(node, pvc).
		WithStatusSubresource(&seiv1alpha1.SeiNode{}).Build()
	refused := errors.New("only dynamically provisioned pvc can be resized and the storageclass that provisions the pvc must support resize")
	c := interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
				return refused
			}
			return cl.Patch(ctx, obj, patch, opts...)
		},
	})
	r := &SeiNodeReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	r.reconcileDataVolumeResize(context.Background(), node)
	r.growDataPVC(context.Background(), node)

	cond := resizeCondition(node)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonDataVolumeResizeFailed))
	g.Expect(cond.Message).To(ContainSubstring("storageclass that provisions the pvc must support resize"))
	g.Expect(drainEvents(r)).To(ContainElement(ContainSubstring("DataVolumeResizeFailed")))
}

// --- Reconcile paths ---

// 011 Req 3.1 through Reconcile: a Running nodeConfig node grows its claim and
// reports Resizing until the provider catches up.
func TestReconcile_DataVolumeGrows(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	node := resizeNode("dvr-reconcile", true, "2Ti")
	r, c := newNodeReconciler(t, node, dataPVC(node, "1Ti", "1Ti", true))

	_, err := r.Reconcile(ctx, nodeReqFor("dvr-reconcile", testNamespace))
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(pvcRequest(t, c, node)).To(Equal("2Ti"))
	cond := resizeCondition(getSeiNode(t, ctx, c, "dvr-reconcile", testNamespace))
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonDataVolumeResizing))
}

// spec.paused promises no derived-resource write beyond the StatefulSet: the
// claim waits, and the condition still rides the paused flush and says why.
func TestReconcile_PausedNode_DataVolumeWaits(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	node := resizeNode("dvr-paused", true, "2Ti")
	node.Spec.Paused = true
	r, c := newNodeReconciler(t, node, dataPVC(node, "1Ti", "1Ti", true))

	_, err := r.Reconcile(ctx, nodeReqFor("dvr-paused", testNamespace))
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(pvcRequest(t, c, node)).To(Equal("1Ti"), "a paused node must not grow its claim")
	cond := resizeCondition(getSeiNode(t, ctx, c, "dvr-paused", testNamespace))
	g.Expect(cond).NotTo(BeNil(), "the condition must ride the paused flush")
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonDataVolumeResizing))
	g.Expect(cond.Message).To(ContainSubstring("spec.paused"))
}

// A Failed node returns early and grows nothing, but still carries the condition.
func TestReconcile_FailedNode_DataVolumeConditionSeeded(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	node := resizeNode("dvr-failed", true, "2Ti")
	node.Status.Phase = seiv1alpha1.PhaseFailed
	r, c := newNodeReconciler(t, node, dataPVC(node, "1Ti", "1Ti", true))

	_, err := r.Reconcile(ctx, nodeReqFor("dvr-failed", testNamespace))
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(pvcRequest(t, c, node)).To(Equal("1Ti"), "a Failed node must not grow its claim")
	g.Expect(resizeCondition(getSeiNode(t, ctx, c, "dvr-failed", testNamespace))).NotTo(BeNil())
}

// drainEvents returns the events the fake recorder has buffered.
func drainEvents(r *SeiNodeReconciler) []string {
	rec, ok := r.Recorder.(*record.FakeRecorder)
	if !ok {
		return nil
	}
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}
