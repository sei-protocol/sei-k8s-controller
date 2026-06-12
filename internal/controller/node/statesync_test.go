package node

import (
	"context"
	"slices"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
)

const (
	testSyncerCMName = "canonical-syncers"
	testSyncerCMNS   = "sei-platform"
	testChainID      = "arctic-1"
	testNamespace    = "default"
	testImage        = "sei:latest"
	testNodeName     = "sei-test"
	syncerA          = "a:26657"
	syncerB          = "b:26657"
	syncerSingle     = "only-one:26657"
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
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: chainID,
			Image:   testImage,
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
		{"newline separated", "b:26657\na:26657", []string{syncerA, syncerB}},
		{"comma separated", "b:26657,a:26657", []string{syncerA, syncerB}},
		{"mixed with blanks", "a:26657,\n b:26657 ,,\n", []string{syncerA, syncerB}},
		{"dedup", "a:26657\na:26657\nb:26657", []string{syncerA, syncerB}},
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

	transient := r.reconcileStateSyncGate(ctx, node)
	g.Expect(transient).To(BeFalse())

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

	transient := r.reconcileStateSyncGate(ctx, node)
	g.Expect(transient).To(BeFalse())

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

	transient := r.reconcileStateSyncGate(ctx, node)
	g.Expect(transient).To(BeFalse())

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

	transient := r.reconcileStateSyncGate(ctx, node)
	g.Expect(transient).To(BeFalse())
	g.Expect(stateSyncCondition(node).Reason).To(Equal(seiv1alpha1.ReasonStateSyncNoSyncersConfigured))
}

func TestStateSyncGate_UnconfiguredConfigMapRef_FailsClosed(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := stateSyncNode("n", testChainID)
	r, _ := newNodeReconciler(t, node) // Platform leaves the ConfigMap name empty.

	transient := r.reconcileStateSyncGate(ctx, node)
	g.Expect(transient).To(BeFalse())
	g.Expect(stateSyncCondition(node).Reason).To(Equal(seiv1alpha1.ReasonStateSyncNoSyncersConfigured))
}

func TestStateSyncGate_Disabled_NotApplicable(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	// S3 snapshot node: state sync not enabled.
	node := newSnapshotNode("n", testNamespace)
	r, _ := newNodeReconciler(t, node)
	withSyncerConfigMap(r)

	transient := r.reconcileStateSyncGate(ctx, node)
	g.Expect(transient).To(BeFalse())

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
		ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: testNamespace},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  testNodeName,
			Image:    testImage,
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
	}
	r, _ := newNodeReconciler(t, node)
	withSyncerConfigMap(r)

	transient := r.reconcileStateSyncGate(ctx, node)
	g.Expect(transient).To(BeFalse())
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

	transient := r.reconcileStateSyncGate(ctx, node)
	g.Expect(transient).To(BeFalse())
	g.Expect(node.Status.ResolvedStateSyncers).To(BeNil())
}

// The gate must not block (or even set ResolvedStateSyncers on) a non-state-sync
// node — regression guard for the full reconcile path.
func TestStateSyncGate_NonStateSyncNodeUnaffected(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newSnapshotNode("n", testNamespace)
	r, _ := newNodeReconciler(t, node)

	transient := r.reconcileStateSyncGate(ctx, node)
	g.Expect(transient).To(BeFalse())
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

	node := stateSyncNode("ss-0", testNamespace)
	node.Spec.ChainID = testChainID
	cm := syncerConfigMap(map[string]string{testChainID: syncerSingle})
	r, c := newNodeReconciler(t, node, cm)
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec
	withSyncerConfigMap(r)

	_, err := r.Reconcile(ctx, nodeReqFor("ss-0", testNamespace))
	g.Expect(err).NotTo(HaveOccurred())

	fetched := getSeiNode(t, ctx, c, "ss-0", testNamespace)
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

// Full reconcile path: a paused node still gets the always-present
// StateSyncReady condition seeded, even though reconcile returns early on
// spec.paused before the gate enforcement. State-sync enabled with no syncers
// resolves to False/NoSyncersConfigured; state-sync disabled resolves to
// False/NotApplicable. Either way the condition must be present — and a paused
// node must build NO plan (pause semantics preserved).
func TestReconcile_PausedNode_StateSyncReadyStillSeeded(t *testing.T) {
	cases := []struct {
		name       string
		node       *seiv1alpha1.SeiNode
		withCM     bool
		wantReason string
	}{
		{
			name:       "state-sync enabled, no syncers",
			node:       stateSyncNode("paused-ss", testNamespace),
			withCM:     true,
			wantReason: seiv1alpha1.ReasonStateSyncNoSyncersConfigured,
		},
		{
			name:       "state-sync disabled",
			node:       newSnapshotNode("paused-s3", testNamespace),
			withCM:     false,
			wantReason: seiv1alpha1.ReasonStateSyncNotApplicable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			ctx := context.Background()

			tc.node.Spec.Paused = true
			r, c := newNodeReconciler(t, tc.node)
			if tc.withCM {
				withSyncerConfigMap(r)
			}

			_, err := r.Reconcile(ctx, nodeReqFor(tc.node.Name, testNamespace))
			g.Expect(err).NotTo(HaveOccurred())

			fetched := getSeiNode(t, ctx, c, tc.node.Name, testNamespace)
			cond := stateSyncCondition(fetched)
			g.Expect(cond).NotTo(BeNil(), "StateSyncReady must be present even on a paused node")
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(tc.wantReason))
			g.Expect(fetched.Status.Plan).To(BeNil(), "a paused node must build no plan")
		})
	}
}

// Fail-closed must NOT block terminal-plan cleanup. A state-sync node with <2
// syncers and a terminal plan on status must have that plan cleared
// (handleTerminalPlan runs inside ResolvePlan, which now runs unconditionally)
// while still building NO new state-sync plan.
func TestReconcile_StateSyncFailClosed_ClearsTerminalPlan(t *testing.T) {
	cases := []struct {
		name  string
		phase seiv1alpha1.TaskPlanPhase
	}{
		{"complete", seiv1alpha1.TaskPlanComplete},
		{"failed", seiv1alpha1.TaskPlanFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			ctx := context.Background()

			node := stateSyncNode("ss-term", testNamespace)
			node.Spec.ChainID = testChainID
			node.Status.Phase = seiv1alpha1.PhaseInitializing
			node.Status.Plan = &seiv1alpha1.TaskPlan{
				ID:    "stale-plan",
				Phase: tc.phase,
				Tasks: []seiv1alpha1.PlannedTask{{Type: planner.TaskConfigApply, Status: seiv1alpha1.TaskComplete}},
			}
			cm := syncerConfigMap(map[string]string{testChainID: syncerSingle})
			r, c := newNodeReconciler(t, node, cm)
			withSyncerConfigMap(r)

			_, err := r.Reconcile(ctx, nodeReqFor("ss-term", testNamespace))
			g.Expect(err).NotTo(HaveOccurred())

			fetched := getSeiNode(t, ctx, c, "ss-term", testNamespace)
			g.Expect(fetched.Status.Plan).To(BeNil(),
				"terminal plan must be cleared even when fail-closed")
			cond := stateSyncCondition(fetched)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonStateSyncNoSyncersConfigured))
		})
	}
}

// A non-NotFound ConfigMap read error must be transient: reconcileStatefulSet,
// Paused/Failed handling, and the status flush still run, the reconcile
// requeues instead of hard-aborting, and StateSyncReady reflects it via
// Unknown/ConfigMapReadError.
func TestReconcile_StateSyncConfigMapReadError_Transient(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := stateSyncNode("ss-err", testNamespace)
	node.Spec.ChainID = testChainID
	r, c := newNodeReconcilerWithGetError(t, node, func(key client.ObjectKey) error {
		if key.Name == testSyncerCMName {
			return apierrors.NewServiceUnavailable("etcd unavailable")
		}
		return nil
	})
	withSyncerConfigMap(r)

	res, err := r.Reconcile(ctx, nodeReqFor("ss-err", testNamespace))
	g.Expect(err).NotTo(HaveOccurred(), "transient ConfigMap error must not hard-abort")
	g.Expect(res.RequeueAfter).To(BeNumerically(">", 0), "transient error must requeue")

	fetched := getSeiNode(t, ctx, c, "ss-err", testNamespace)
	cond := stateSyncCondition(fetched)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionUnknown))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonStateSyncConfigMapReadError))
	g.Expect(fetched.Status.Plan).To(BeNil(), "no state-sync plan while the gate is unresolved")
	// StatefulSet sync still ran despite the ConfigMap error.
	sts := &appsv1.StatefulSet{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "ss-err", Namespace: testNamespace}, sts)).To(Succeed(),
		"reconcileStatefulSet must run even when the ConfigMap read fails")
}

// The StateSyncBlocked Warning fires once on the transition into fail-closed,
// not on every requeue.
func TestReconcile_StateSyncBlocked_EventFiresOncePerTransition(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := stateSyncNode("ss-evt", testNamespace)
	node.Spec.ChainID = testChainID
	cm := syncerConfigMap(map[string]string{testChainID: syncerSingle})
	r, _ := newNodeReconciler(t, node, cm)
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec
	withSyncerConfigMap(r)

	countBlocked := func() int {
		n := 0
		for {
			select {
			case e := <-rec.Events:
				if strings.Contains(e, "StateSyncBlocked") {
					n++
				}
				continue
			default:
			}
			return n
		}
	}

	_, err := r.Reconcile(ctx, nodeReqFor("ss-evt", testNamespace))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(countBlocked()).To(Equal(1), "first fail-closed reconcile emits StateSyncBlocked")

	// Plan stays nil and the condition is already False/NoSyncersConfigured, so a
	// second reconcile is a no-op transition — no repeat event.
	_, err = r.Reconcile(ctx, nodeReqFor("ss-evt", testNamespace))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(countBlocked()).To(Equal(0), "steady-state requeue must not re-emit StateSyncBlocked")
}

// Full reconcile path: a state-sync node with >=2 syncers proceeds — a plan is
// built carrying the canonical syncers, and StateSyncReady is True.
func TestReconcile_StateSyncReady_BuildsPlanWithSyncers(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := stateSyncNode("ss-1", testNamespace)
	node.Spec.ChainID = testChainID
	cm := syncerConfigMap(map[string]string{
		testChainID: "a.arctic-1.example.com:26657\nb.arctic-1.example.com:26657",
	})
	r, c := newNodeReconciler(t, node, cm)
	withSyncerConfigMap(r)

	_, err := r.Reconcile(ctx, nodeReqFor("ss-1", testNamespace))
	g.Expect(err).NotTo(HaveOccurred())

	fetched := getSeiNode(t, ctx, c, "ss-1", testNamespace)
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
