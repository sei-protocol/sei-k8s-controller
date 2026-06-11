package node

import (
	"context"
	"slices"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
)

const (
	testSyncerCMName = "canonical-syncers"
	testSyncerCMNS   = "sei-platform"
	testChainID      = "arctic-1"
)

func syncerConfigMap(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testSyncerCMName, Namespace: testSyncerCMNS},
		Data:       data,
	}
}

// stateSyncNode returns a FullNode with state sync enabled on the given chain.
func stateSyncNode(name, chainID string) *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: chainID,
			Image:   "sei:latest",
			FullNode: &seiv1alpha1.FullNodeSpec{
				Snapshot: &seiv1alpha1.SnapshotSource{StateSync: &seiv1alpha1.StateSyncSource{}},
			},
		},
	}
}

func withSyncerConfigMap(r *SeiNodeReconciler) {
	r.Platform.StateSyncSyncersConfigMap = testSyncerCMName
	r.Platform.StateSyncSyncersNamespace = testSyncerCMNS
}

func stateSyncCondition(node *seiv1alpha1.SeiNode) *metav1.Condition {
	return apimeta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionStateSyncReady)
}

func TestParseSyncerList(t *testing.T) {
	g := NewWithT(t)
	cases := []struct {
		name, in string
		want     []string
	}{
		{"empty", "", nil},
		{"whitespace only", "  \n\t ", nil},
		{"newline separated", "b:26657\na:26657", []string{"a:26657", "b:26657"}},
		{"comma separated", "b:26657,a:26657", []string{"a:26657", "b:26657"}},
		{"mixed with blanks", "a:26657,\n b:26657 ,,\n", []string{"a:26657", "b:26657"}},
		{"dedup", "a:26657\na:26657\nb:26657", []string{"a:26657", "b:26657"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g.Expect(parseSyncerList(tc.in)).To(Equal(tc.want))
		})
	}
}

func TestStateSyncGate_EnabledWithTwoSyncers_Ready(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := stateSyncNode("n", testChainID)
	cm := syncerConfigMap(map[string]string{
		testChainID: "syncer-1.arctic-1.example.com:26657\nsyncer-0.arctic-1.example.com:26657",
	})
	r, _ := newNodeReconciler(t, node, cm)
	withSyncerConfigMap(r)

	ready, err := r.reconcileStateSyncGate(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())

	cond := stateSyncCondition(node)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonStateSyncReady))
	g.Expect(cond.ObservedGeneration).To(Equal(node.Generation))
	// Sorted + fed verbatim into the task source.
	g.Expect(node.Status.ResolvedStateSyncers).To(Equal([]string{
		"syncer-0.arctic-1.example.com:26657",
		"syncer-1.arctic-1.example.com:26657",
	}))
}

func TestStateSyncGate_EnabledWithOneSyncer_FailsClosed(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := stateSyncNode("n", testChainID)
	cm := syncerConfigMap(map[string]string{testChainID: "only-one.arctic-1.example.com:26657"})
	r, _ := newNodeReconciler(t, node, cm)
	withSyncerConfigMap(r)

	ready, err := r.reconcileStateSyncGate(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())

	cond := stateSyncCondition(node)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonStateSyncNoSyncersConfigured))
	g.Expect(node.Status.ResolvedStateSyncers).To(BeNil())
}

func TestStateSyncGate_EnabledMissingConfigMap_FailsClosed(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := stateSyncNode("n", testChainID)
	r, _ := newNodeReconciler(t, node) // no ConfigMap object
	withSyncerConfigMap(r)

	ready, err := r.reconcileStateSyncGate(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())

	cond := stateSyncCondition(node)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonStateSyncNoSyncersConfigured))
}

func TestStateSyncGate_EnabledNoChainEntry_FailsClosed(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := stateSyncNode("n", testChainID)
	cm := syncerConfigMap(map[string]string{"other-chain": "a:26657\nb:26657"})
	r, _ := newNodeReconciler(t, node, cm)
	withSyncerConfigMap(r)

	ready, err := r.reconcileStateSyncGate(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
	g.Expect(stateSyncCondition(node).Reason).To(Equal(seiv1alpha1.ReasonStateSyncNoSyncersConfigured))
}

func TestStateSyncGate_UnconfiguredConfigMapRef_FailsClosed(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := stateSyncNode("n", testChainID)
	r, _ := newNodeReconciler(t, node) // Platform leaves the ConfigMap name empty.

	ready, err := r.reconcileStateSyncGate(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
	g.Expect(stateSyncCondition(node).Reason).To(Equal(seiv1alpha1.ReasonStateSyncNoSyncersConfigured))
}

func TestStateSyncGate_Disabled_NotApplicable(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	// S3 snapshot node: state sync not enabled.
	node := newSnapshotNode("n", "default")
	r, _ := newNodeReconciler(t, node)
	withSyncerConfigMap(r)

	ready, err := r.reconcileStateSyncGate(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())

	cond := stateSyncCondition(node)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonStateSyncNotApplicable))
	g.Expect(node.Status.ResolvedStateSyncers).To(BeNil())
}

// A node with no snapshot source at all (e.g. genesis validator) is treated as
// state-sync-disabled: NotApplicable, never blocks the plan.
func TestStateSyncGate_NoSnapshotSource_NotApplicable(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  "sei-test",
			Image:    "sei:latest",
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
	}
	r, _ := newNodeReconciler(t, node)
	withSyncerConfigMap(r)

	ready, err := r.reconcileStateSyncGate(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())
	g.Expect(stateSyncCondition(node).Reason).To(Equal(seiv1alpha1.ReasonStateSyncNotApplicable))
}

// Stale ResolvedStateSyncers must be cleared when the gate later fails closed,
// so a previously-good set can't leak into ConfigureStateSyncTask.
func TestStateSyncGate_FailClosedClearsStaleSyncers(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := stateSyncNode("n", testChainID)
	node.Status.ResolvedStateSyncers = []string{"old-a:26657", "old-b:26657"}
	r, _ := newNodeReconciler(t, node) // unconfigured ref → fail closed
	withSyncerConfigMap(r)
	r.Platform.StateSyncSyncersConfigMap = "" // force unconfigured

	ready, err := r.reconcileStateSyncGate(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
	g.Expect(node.Status.ResolvedStateSyncers).To(BeNil())
}

// The gate must not block (or even set ResolvedStateSyncers on) a non-state-sync
// node — regression guard for the full reconcile path.
func TestStateSyncGate_NonStateSyncNodeUnaffected(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newSnapshotNode("n", "default")
	r, _ := newNodeReconciler(t, node)

	ready, err := r.reconcileStateSyncGate(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())
	g.Expect(node.Status.ResolvedStateSyncers).To(BeEmpty())
	g.Expect(slices.Contains([]string{
		seiv1alpha1.ReasonStateSyncReady, seiv1alpha1.ReasonStateSyncNoSyncersConfigured,
	}, stateSyncCondition(node).Reason)).To(BeFalse())
}

// Full reconcile path: a state-sync node with <2 syncers fails closed — no plan
// is built, the condition is persisted via the single status patch, and a
// Warning Event is emitted.
func TestReconcile_StateSyncFailClosed_NoPlanBuilt(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := stateSyncNode("ss-0", "default")
	node.Spec.ChainID = testChainID
	cm := syncerConfigMap(map[string]string{testChainID: "only-one:26657"})
	r, c := newNodeReconciler(t, node, cm)
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec
	withSyncerConfigMap(r)

	_, err := r.Reconcile(ctx, nodeReqFor("ss-0", "default"))
	g.Expect(err).NotTo(HaveOccurred())

	fetched := getSeiNode(t, ctx, c, "ss-0", "default")
	g.Expect(fetched.Status.Plan).To(BeNil(), "no state-sync plan must be built when fail-closed")
	cond := stateSyncCondition(fetched)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonStateSyncNoSyncersConfigured))

	var sawWarning bool
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, "StateSyncBlocked") {
				sawWarning = true
			}
			continue
		default:
		}
		break
	}
	g.Expect(sawWarning).To(BeTrue(), "expected a StateSyncBlocked Warning Event")
}

// Full reconcile path: a state-sync node with >=2 syncers proceeds — a plan is
// built carrying the canonical syncers, and StateSyncReady is True.
func TestReconcile_StateSyncReady_BuildsPlanWithSyncers(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := stateSyncNode("ss-1", "default")
	node.Spec.ChainID = testChainID
	cm := syncerConfigMap(map[string]string{
		testChainID: "a.arctic-1.example.com:26657\nb.arctic-1.example.com:26657",
	})
	r, c := newNodeReconciler(t, node, cm)
	withSyncerConfigMap(r)

	_, err := r.Reconcile(ctx, nodeReqFor("ss-1", "default"))
	g.Expect(err).NotTo(HaveOccurred())

	fetched := getSeiNode(t, ctx, c, "ss-1", "default")
	g.Expect(fetched.Status.Plan).NotTo(BeNil(), "a plan must be built when state-sync is ready")
	g.Expect(findPlannedTask(fetched.Status.Plan, planner.TaskConfigureStateSync)).
		NotTo(BeNil(), "plan must carry the configure-state-sync task")
	g.Expect(fetched.Status.ResolvedStateSyncers).To(Equal([]string{
		"a.arctic-1.example.com:26657",
		"b.arctic-1.example.com:26657",
	}))
	cond := stateSyncCondition(fetched)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonStateSyncReady))
}
