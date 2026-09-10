package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// The VolumeAttributesClass pre-flight lives in the reconciler, not in the
// ensure-data-pvc task that consumes it, because the task is not on every path.
// These tests cover the branch matrix and then every path that runs no task at
// all — the reason the condition moved here.

// vacObject returns a cluster-scoped class. Driver and parameters are opaque
// placeholders: the controller references the class by name and never reads it.
func vacObject(name string) *storagev1.VolumeAttributesClass {
	return &storagev1.VolumeAttributesClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		DriverName: "csi.example.com",
		Parameters: map[string]string{"placeholder": "opaque-to-the-controller"},
	}
}

// vacNode returns a full node selecting vac; an empty vac leaves it unset.
func vacNode(name, vac string) *seiv1alpha1.SeiNode {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Generation: 3},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  testChainID,
			Image:    testImage,
			FullNode: &seiv1alpha1.FullNodeSpec{},
			Sidecar:  &seiv1alpha1.SidecarConfig{Port: 7777},
		},
	}
	if vac != "" {
		node.Spec.DataVolume = &seiv1alpha1.DataVolumeSpec{
			Storage: &seiv1alpha1.DataVolumeStorage{VolumeAttributesClassName: &vac},
		}
	}
	return node
}

func vacCondition(node *seiv1alpha1.SeiNode) *metav1.Condition {
	return apimeta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionVolumeAttributesClassReady)
}

// --- Branch matrix ---

func TestVACGate_NoSelection_TrueAndPresent(t *testing.T) {
	g := NewWithT(t)
	node := vacNode("vac-none", "")
	r, _ := newNodeReconciler(t, node)

	r.reconcileVolumeAttributesClass(context.Background(), node)

	cond := vacCondition(node)
	g.Expect(cond).NotTo(BeNil(), "the condition must be present even with no selection")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonNoVolumeAttributesClass))
	g.Expect(cond.ObservedGeneration).To(Equal(node.Generation))
}

func TestVACGate_SelectionFound_True(t *testing.T) {
	g := NewWithT(t)
	node := vacNode("vac-found", "vac-alpha")
	r, _ := newNodeReconciler(t, node, vacObject("vac-alpha"))

	r.reconcileVolumeAttributesClass(context.Background(), node)

	cond := vacCondition(node)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonVolumeAttributesClassFound))
}

func TestVACGate_SelectionMissing_FalseAndNamesTheClass(t *testing.T) {
	g := NewWithT(t)
	node := vacNode("vac-missing", "vac-absent")
	r, _ := newNodeReconciler(t, node) // no class in the cluster

	r.reconcileVolumeAttributesClass(context.Background(), node)

	cond := vacCondition(node)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonVolumeAttributesClassNotFound))
	g.Expect(cond.Message).To(ContainSubstring("vac-absent"),
		"the message must name the class the platform has to add")
}

// A read failure that is not absence — including a cluster that does not serve
// storage.k8s.io VolumeAttributesClasses at all — must not read as no selection.
func TestVACGate_LookupError_False(t *testing.T) {
	g := NewWithT(t)
	node := vacNode("vac-err", "vac-alpha")
	r, _ := newNodeReconciler(t, node)
	r.Client = &vacUnservedClient{Client: r.Client}

	r.reconcileVolumeAttributesClass(context.Background(), node)

	cond := vacCondition(node)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonVolumeAttributesClassLookupError))
}

// An imported volume keeps the importer's parameters, so there is no selection
// to pre-flight — reported as NotApplicable, never left absent.
func TestVACGate_Import_NotApplicable(t *testing.T) {
	g := NewWithT(t)
	node := vacNode("vac-import", "")
	node.Spec.DataVolume = &seiv1alpha1.DataVolumeSpec{
		Import: &seiv1alpha1.DataVolumeImport{PVCName: "adopted-pvc"},
	}
	r, _ := newNodeReconciler(t, node)

	r.reconcileVolumeAttributesClass(context.Background(), node)

	cond := vacCondition(node)
	g.Expect(cond).NotTo(BeNil(), "an import node must still carry the condition")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonVolumeAttributesClassNotApplicable))
}

// --- The paths that run no ensure-data-pvc task ---
//
// One test per bypass. Each proves the condition is PRESENT after a full
// Reconcile, which is what the pre-flight's first home (inside the task) could
// not deliver.

// THE upgrade case, and the reason this moved: a node that predates the field is
// Running with no drift, so the planner builds no plan (full.go returns nil) and
// no task ever executes. Reconciling it must still produce the condition.
func TestReconcile_RunningNoDrift_NoPlan_VACConditionStillPresent(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	// Exactly the shape an already-running node has after the controller rolls
	// out: Running, image already observed, no plan, and — the point — no
	// VolumeAttributesClassReady condition on status.
	node := vacNode("vac-upgrade", "")
	node.Status.Phase = seiv1alpha1.PhaseRunning
	node.Status.CurrentImage = node.Spec.Image
	node.Status.CurrentSidecarImage = "placeholder-sidecar"
	node.Status.Plan = nil
	g.Expect(vacCondition(node)).To(BeNil(), "fixture must start with the condition absent")

	r, c := newNodeReconciler(t, node)
	_, err := r.Reconcile(ctx, nodeReqFor("vac-upgrade", testNamespace))
	g.Expect(err).NotTo(HaveOccurred())

	fetched := getSeiNode(t, ctx, c, "vac-upgrade", testNamespace)
	cond := vacCondition(fetched)
	g.Expect(cond).NotTo(BeNil(),
		"a steady-state Running node must acquire the condition without any plan running")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonNoVolumeAttributesClass))
	g.Expect(findPlannedTask(fetched.Status.Plan, "ensure-data-pvc")).To(BeNil(),
		"no ensure-data-pvc task may have run — the reconciler is what produced the condition")
}

// Failed is terminal and returns early, flushing only what is already on status.
func TestReconcile_FailedNode_VACConditionStillSeeded(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := vacNode("vac-failed", "vac-absent")
	node.Status.Phase = seiv1alpha1.PhaseFailed

	r, c := newNodeReconciler(t, node)
	_, err := r.Reconcile(ctx, nodeReqFor("vac-failed", testNamespace))
	g.Expect(err).NotTo(HaveOccurred())

	cond := vacCondition(getSeiNode(t, ctx, c, "vac-failed", testNamespace))
	g.Expect(cond).NotTo(BeNil(), "a Failed node must still carry the condition")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonVolumeAttributesClassNotFound))
}

// A paused node returns early too, and must build no plan (pause semantics).
func TestReconcile_PausedNode_VACConditionStillSeeded(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := vacNode("vac-paused", "vac-alpha")
	node.Spec.Paused = true

	r, c := newNodeReconciler(t, node, vacObject("vac-alpha"))
	_, err := r.Reconcile(ctx, nodeReqFor("vac-paused", testNamespace))
	g.Expect(err).NotTo(HaveOccurred())

	fetched := getSeiNode(t, ctx, c, "vac-paused", testNamespace)
	cond := vacCondition(fetched)
	g.Expect(cond).NotTo(BeNil(), "a paused node must still carry the condition")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonVolumeAttributesClassFound))
	g.Expect(fetched.Status.Plan).To(BeNil(), "a paused node must build no plan")
}

// The state-sync gate suppresses plan construction entirely, so no
// ensure-data-pvc task exists to produce the condition.
func TestReconcile_StateSyncGated_VACConditionStillSeeded(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := stateSyncNode("vac-gated", testChainID)
	name := "vac-alpha"
	node.Spec.DataVolume = &seiv1alpha1.DataVolumeSpec{
		Storage: &seiv1alpha1.DataVolumeStorage{VolumeAttributesClassName: &name},
	}

	r, c := newNodeReconciler(t, node, vacObject(name))
	withSyncers(t, r, map[string][]string{testChainID: {syncerSingle}}) // below the floor

	_, err := r.Reconcile(ctx, nodeReqFor("vac-gated", testNamespace))
	g.Expect(err).NotTo(HaveOccurred())

	fetched := getSeiNode(t, ctx, c, "vac-gated", testNamespace)
	g.Expect(fetched.Status.Plan).To(BeNil(), "the state-sync gate must suppress the plan")
	cond := vacCondition(fetched)
	g.Expect(cond).NotTo(BeNil(), "a state-sync-gated node must still carry the condition")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonVolumeAttributesClassFound))
}

// The condition tracks the cluster on every reconcile, not just the first: a
// class deleted under a node re-resolves to False rather than latching True.
func TestReconcile_VACConditionReresolvesEachReconcile(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := vacNode("vac-reresolve", "vac-alpha")
	node.Status.Phase = seiv1alpha1.PhaseRunning
	node.Status.CurrentImage = node.Spec.Image
	vac := vacObject("vac-alpha")

	r, c := newNodeReconciler(t, node, vac)
	_, err := r.Reconcile(ctx, nodeReqFor("vac-reresolve", testNamespace))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(vacCondition(getSeiNode(t, ctx, c, "vac-reresolve", testNamespace)).Reason).
		To(Equal(seiv1alpha1.ReasonVolumeAttributesClassFound))

	g.Expect(c.Delete(ctx, vac)).To(Succeed())

	_, err = r.Reconcile(ctx, nodeReqFor("vac-reresolve", testNamespace))
	g.Expect(err).NotTo(HaveOccurred())

	cond := vacCondition(getSeiNode(t, ctx, c, "vac-reresolve", testNamespace))
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonVolumeAttributesClassNotFound))
}

// Guards the single-writer split: the reconciler owns the condition and the task
// only reads it, so a reconcile costs exactly one class read no matter how far
// the plan gets. Two reconciles are needed to exercise both halves — plans are
// persisted on one reconcile and executed on the next (atomic plan creation) —
// so a single reconcile would never run the task and the count would prove
// nothing about it.
func TestReconcile_VACPreflight_ReadsTheClassOncePerReconcile(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	// A missing class: the plan is built, and the task then holds on it, which
	// is the state where a second reader would show up.
	node := vacNode("vac-onceread", "vac-absent")
	r, c := newNodeReconciler(t, node)
	counting := &vacCountingClient{Client: r.Client}
	r.Client = counting

	// Reconcile 1: resolve + persist the plan (no task runs yet).
	_, err := r.Reconcile(ctx, nodeReqFor("vac-onceread", testNamespace))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(counting.reads).To(Equal(1), "the resolve is one read")
	g.Expect(findPlannedTask(getSeiNode(t, ctx, c, "vac-onceread", testNamespace).Status.Plan,
		"ensure-data-pvc")).NotTo(BeNil(), "the init plan must carry ensure-data-pvc for the task to hold on")

	// Reconcile 2: resolve, then execute the plan — the task holds.
	_, err = r.Reconcile(ctx, nodeReqFor("vac-onceread", testNamespace))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(counting.reads).To(Equal(2),
		fmt.Sprintf("a reconcile that also executes the holding task must still read the class once; got %d reads over two reconciles", counting.reads))

	fetched := getSeiNode(t, ctx, c, "vac-onceread", testNamespace)
	g.Expect(vacCondition(fetched).Reason).To(Equal(seiv1alpha1.ReasonVolumeAttributesClassNotFound))
	pvc := &corev1.PersistentVolumeClaim{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "data-vac-onceread", Namespace: testNamespace}, pvc)).
		NotTo(Succeed(), "the task must have held provisioning")
}

// vacUnservedClient fails every VolumeAttributesClass read the way a cluster
// that does not serve the type would: no matching kind, not not-found.
type vacUnservedClient struct {
	client.Client
}

func (v *vacUnservedClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*storagev1.VolumeAttributesClass); ok {
		return &apimeta.NoKindMatchError{
			GroupKind:        schema.GroupKind{Group: storagev1.GroupName, Kind: "VolumeAttributesClass"},
			SearchedVersions: []string{"v1"},
		}
	}
	return v.Client.Get(ctx, key, obj, opts...)
}

// vacCountingClient counts VolumeAttributesClass reads.
type vacCountingClient struct {
	client.Client
	reads int
}

func (v *vacCountingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*storagev1.VolumeAttributesClass); ok {
		v.reads++
	}
	return v.Client.Get(ctx, key, obj, opts...)
}

// TestVACRoleGrantsReadOnly pins the generated ClusterRole to the verb set
// DR-001 fixed for the pre-flight: get;list;watch and nothing else. The
// pre-flight is a cluster-scoped read on a namespaced-workload controller, so a
// widened verb here is a privilege escalation, and the catalog being
// platform-owned means the controller has no reason to write one ever.
//
// `make verify-generated` cannot catch this: it only compares the marker against
// the manifest, so widening the marker and regenerating leaves it green. This
// reads the committed role instead.
//
// The role is parsed rather than text-searched, and every rule that reaches the
// resource is unioned — including one that reaches it through an apiGroups or
// resources wildcard, which a grep for the resource name would miss entirely.
func TestVACRoleGrantsReadOnly(t *testing.T) {
	g := NewWithT(t)

	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "manifests", "role.yaml"))
	g.Expect(err).NotTo(HaveOccurred(), "reading the generated ClusterRole")

	var role rbacv1.ClusterRole
	g.Expect(yaml.Unmarshal(raw, &role)).To(Succeed(), "parsing the generated ClusterRole")
	g.Expect(role.Rules).NotTo(BeEmpty(), "generated role exposes no rules; run `make manifests`")

	covers := func(values []string, want string) bool {
		return slices.Contains(values, want) || slices.Contains(values, rbacv1.APIGroupAll)
	}

	var verbs, matched int
	verbSet := map[string]struct{}{}
	for _, rule := range role.Rules {
		if !covers(rule.APIGroups, storagev1.GroupName) || !covers(rule.Resources, "volumeattributesclasses") {
			continue
		}
		matched++
		for _, v := range rule.Verbs {
			verbSet[v] = struct{}{}
			verbs++
		}
	}
	g.Expect(matched).To(Equal(1),
		"exactly one rule may reach volumeattributesclasses; run `make manifests`")

	got := make([]string, 0, len(verbSet))
	for v := range verbSet {
		got = append(got, v)
	}
	g.Expect(got).To(ConsistOf("get", "list", "watch"),
		"the pre-flight is read-only: DR-001 pins get;list;watch, and the controller must never hold a write on a cluster-scoped class")
}
