package node

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
)

const (
	testChainID   = "arctic-1"
	testNamespace = "default"
	testImage     = "sei:latest"
	testNodeName  = "sei-test"
	syncerA       = "a:26657"
	syncerB       = "b:26657"
	syncerSingle  = "only-one:26657"
)

// syncerFileYAML renders a chainID -> [host:port] map as the read-only
// controller-config file content, wrapped under the stateSync.syncers section
// (matching canonicalSyncers' expected platform.FileConfig shape).
func syncerFileYAML(byChain map[string][]string) string {
	var b strings.Builder
	b.WriteString("stateSync:\n")
	b.WriteString("  syncers:\n")
	for chain, syncers := range byChain {
		b.WriteString("    ")
		b.WriteString(chain)
		b.WriteString(":\n")
		for _, s := range syncers {
			b.WriteString("      - ")
			b.WriteString(s)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// writeSyncerFile writes content to a temp file and points the reconciler's
// platform at it. The file lives under t.TempDir(), auto-cleaned by the test.
func writeSyncerFile(t *testing.T, r *SeiNodeReconciler, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing controller config file: %v", err)
	}
	r.Platform.ControllerConfigFile = path
}

// withSyncers writes a chainID -> syncers file and wires the reconciler to it.
func withSyncers(t *testing.T, r *SeiNodeReconciler, byChain map[string][]string) {
	t.Helper()
	writeSyncerFile(t, r, syncerFileYAML(byChain))
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

	node := stateSyncNode("n", testChainID)
	r, _ := newNodeReconciler(t, node)
	withSyncers(t, r, map[string][]string{
		testChainID: {"syncer-1.arctic-1.example.com:26657", "syncer-0.arctic-1.example.com:26657"},
	})

	blocked := r.reconcileStateSyncGate(node)
	g.Expect(blocked).To(BeFalse())

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

// Spec-declared rpcServers replace the registry: the gate resolves True from
// the spec without reading the syncer file at all. The malformed-but-present
// file proves the read is skipped (a read would yield SyncerSourceError), not
// merely absent.
func TestStateSyncGate_SpecRpcServers_BypassRegistry(t *testing.T) {
	g := NewWithT(t)

	node := stateSyncNode("n", testChainID)
	node.Spec.FullNode.Snapshot.RpcServers = []string{syncerB, syncerA}
	r, _ := newNodeReconciler(t, node)
	writeSyncerFile(t, r, "{ this is not: valid yaml: at all") // must never be read

	blocked := r.reconcileStateSyncGate(node)
	g.Expect(blocked).To(BeFalse())

	cond := stateSyncCondition(node)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonStateSyncReady))
	g.Expect(cond.Message).To(ContainSubstring("declared on spec"),
		"condition must attribute the witness source")
	// Normalized (sorted) like the registry path.
	g.Expect(node.Status.ResolvedStateSyncers).To(Equal([]string{syncerA, syncerB}))
}

// Spec items are atomic: a comma inside an item (rejected at admission, but
// possible past a schema downgrade) must not be split into fragments the way
// comma-joined registry values are — it stays one (broken, runtime-visible)
// witness instead of becoming several unvalidated ones.
func TestStateSyncGate_SpecRpcServers_ItemsNotCommaSplit(t *testing.T) {
	g := NewWithT(t)

	node := stateSyncNode("n", testChainID)
	node.Spec.FullNode.Snapshot.RpcServers = []string{"a:26657,b:26657", syncerA}
	r, _ := newNodeReconciler(t, node)

	blocked := r.reconcileStateSyncGate(node)
	g.Expect(blocked).To(BeFalse())
	g.Expect(node.Status.ResolvedStateSyncers).To(Equal([]string{syncerA, "a:26657,b:26657"}))
}

// Admission enforces >=2 unique rpcServers, but the gate keeps a live floor as
// the fail-closed backstop: a set that normalizes below the floor (possible
// only past admission, e.g. after a CRD schema downgrade) falls through to the
// registry path — Ready when the registry satisfies the floor, fail-closed when
// it doesn't.
func TestStateSyncGate_SpecRpcServersBelowFloor_FallsThroughToRegistry(t *testing.T) {
	cases := []struct {
		name       string
		registry   map[string][]string
		wantReason string
		wantStatus metav1.ConditionStatus
	}{
		{
			name:       "registry satisfies floor",
			registry:   map[string][]string{testChainID: {syncerA, syncerB}},
			wantReason: seiv1alpha1.ReasonStateSyncReady,
			wantStatus: metav1.ConditionTrue,
		},
		{
			name:       "registry empty",
			registry:   map[string][]string{},
			wantReason: seiv1alpha1.ReasonStateSyncNoSyncersConfigured,
			wantStatus: metav1.ConditionFalse,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			node := stateSyncNode("n", testChainID)
			// Duplicates normalize to a single entry — below the floor.
			node.Spec.FullNode.Snapshot.RpcServers = []string{syncerSingle, syncerSingle}
			r, _ := newNodeReconciler(t, node)
			withSyncers(t, r, tc.registry)

			blocked := r.reconcileStateSyncGate(node)
			g.Expect(blocked).To(Equal(tc.wantStatus != metav1.ConditionTrue))
			cond := stateSyncCondition(node)
			g.Expect(cond.Status).To(Equal(tc.wantStatus))
			g.Expect(cond.Reason).To(Equal(tc.wantReason))
		})
	}
}

func TestStateSyncGate_EnabledWithOneSyncer_FailsClosed(t *testing.T) {
	g := NewWithT(t)

	node := stateSyncNode("n", testChainID)
	r, _ := newNodeReconciler(t, node)
	withSyncers(t, r, map[string][]string{testChainID: {"only-one.arctic-1.example.com:26657"}})

	blocked := r.reconcileStateSyncGate(node)
	g.Expect(blocked).To(BeTrue())

	cond := stateSyncCondition(node)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonStateSyncNoSyncersConfigured))
	g.Expect(node.Status.ResolvedStateSyncers).To(BeNil())
}

// A configured path pointing at a file that doesn't exist (the backing
// ConfigMap isn't provisioned yet) fails closed, not transient.
func TestStateSyncGate_EnabledMissingFile_FailsClosed(t *testing.T) {
	g := NewWithT(t)

	node := stateSyncNode("n", testChainID)
	r, _ := newNodeReconciler(t, node)
	r.Platform.ControllerConfigFile = filepath.Join(t.TempDir(), "absent.yaml") // never created

	blocked := r.reconcileStateSyncGate(node)
	g.Expect(blocked).To(BeTrue())

	cond := stateSyncCondition(node)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonStateSyncNoSyncersConfigured))
}

func TestStateSyncGate_EnabledNoChainEntry_FailsClosed(t *testing.T) {
	g := NewWithT(t)

	node := stateSyncNode("n", testChainID)
	r, _ := newNodeReconciler(t, node)
	withSyncers(t, r, map[string][]string{"other-chain": {syncerA, syncerB}})

	blocked := r.reconcileStateSyncGate(node)
	g.Expect(blocked).To(BeTrue())
	g.Expect(stateSyncCondition(node).Reason).To(Equal(seiv1alpha1.ReasonStateSyncNoSyncersConfigured))
}

// An unreadable/unparseable file is transient (Unknown + requeue), distinct
// from absence which fails closed.
func TestStateSyncGate_ParseError_Transient(t *testing.T) {
	g := NewWithT(t)

	node := stateSyncNode("n", testChainID)
	r, _ := newNodeReconciler(t, node)
	writeSyncerFile(t, r, "this: [is: not: valid: yaml") // malformed

	blocked := r.reconcileStateSyncGate(node)
	g.Expect(blocked).To(BeTrue())

	cond := stateSyncCondition(node)
	g.Expect(cond.Status).To(Equal(metav1.ConditionUnknown))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonStateSyncSyncerSourceError))
	g.Expect(node.Status.ResolvedStateSyncers).To(BeNil())
}

func TestStateSyncGate_UnconfiguredSource_FailsClosed(t *testing.T) {
	g := NewWithT(t)

	node := stateSyncNode("n", testChainID)
	r, _ := newNodeReconciler(t, node)
	r.Platform.ControllerConfigFile = "" // unconfigured source

	blocked := r.reconcileStateSyncGate(node)
	g.Expect(blocked).To(BeTrue())
	g.Expect(stateSyncCondition(node).Reason).To(Equal(seiv1alpha1.ReasonStateSyncNoSyncersConfigured))
}

// An s3-restore node applies its snapshot via CometBFT state-sync
// (use-local-snapshot), so it ALSO needs canonical rpc-server witnesses to
// verify the trust point. The gate must resolve syncers for it (Ready when >=2
// are configured) rather than declaring NotApplicable — the latter left
// ResolvedStateSyncers nil and the sidecar fell back to unreachable peers.
func TestStateSyncGate_S3Restore_ResolvesSyncers(t *testing.T) {
	g := NewWithT(t)

	node := newSnapshotNode("n", testNamespace) // s3 source, ChainID "sei-test"
	r, _ := newNodeReconciler(t, node)
	withSyncers(t, r, map[string][]string{node.Spec.ChainID: {syncerA, syncerB}})

	blocked := r.reconcileStateSyncGate(node)
	g.Expect(blocked).To(BeFalse())

	cond := stateSyncCondition(node)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonStateSyncReady))
	g.Expect(node.Status.ResolvedStateSyncers).To(Equal([]string{syncerA, syncerB}))
}

// An s3-restore node with <2 configured syncers must fail closed — same as a
// stateSync node. This is the bug being fixed: previously it was NotApplicable
// and proceeded witness-less.
func TestStateSyncGate_S3Restore_OneSyncer_FailsClosed(t *testing.T) {
	g := NewWithT(t)

	node := newSnapshotNode("n", testNamespace)
	r, _ := newNodeReconciler(t, node)
	withSyncers(t, r, map[string][]string{node.Spec.ChainID: {syncerSingle}})

	blocked := r.reconcileStateSyncGate(node)
	g.Expect(blocked).To(BeTrue())

	cond := stateSyncCondition(node)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonStateSyncNoSyncersConfigured))
	g.Expect(node.Status.ResolvedStateSyncers).To(BeNil())
}

// A node with no snapshot source at all (e.g. genesis validator) carries no
// ConfigureStateSync task: NotApplicable, never blocks the plan.
func TestStateSyncGate_NoSnapshotSource_NotApplicable(t *testing.T) {
	g := NewWithT(t)

	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: testNamespace},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  testNodeName,
			Image:    testImage,
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
	}
	r, _ := newNodeReconciler(t, node)
	withSyncers(t, r, map[string][]string{testChainID: {syncerA, syncerB}})

	blocked := r.reconcileStateSyncGate(node)
	g.Expect(blocked).To(BeFalse())
	g.Expect(stateSyncCondition(node).Reason).To(Equal(seiv1alpha1.ReasonStateSyncNotApplicable))
}

// Stale ResolvedStateSyncers must be cleared when the gate later fails closed,
// so a previously-good set can't leak into ConfigureStateSyncTask.
func TestStateSyncGate_FailClosedClearsStaleSyncers(t *testing.T) {
	g := NewWithT(t)

	node := stateSyncNode("n", testChainID)
	node.Status.ResolvedStateSyncers = []string{"old-a:26657", "old-b:26657"}
	r, _ := newNodeReconciler(t, node)
	r.Platform.ControllerConfigFile = "" // unconfigured source → fail closed

	blocked := r.reconcileStateSyncGate(node)
	g.Expect(blocked).To(BeTrue())
	g.Expect(node.Status.ResolvedStateSyncers).To(BeNil())
}

// The gate must not block (or even set ResolvedStateSyncers on) a node that
// carries no ConfigureStateSync task — a genesis node with no snapshot source.
// Regression guard for the full reconcile path.
func TestStateSyncGate_GenesisNodeUnaffected(t *testing.T) {
	g := NewWithT(t)

	node := newGenesisNode("n", testNamespace)
	r, _ := newNodeReconciler(t, node)

	blocked := r.reconcileStateSyncGate(node)
	g.Expect(blocked).To(BeFalse())
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
	r, c := newNodeReconciler(t, node)
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec
	withSyncers(t, r, map[string][]string{testChainID: {syncerSingle}})

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

// A pre-Running, fail-closed state-sync node builds no plan, so nothing else
// drives a requeue and the mounted syncer file has no watch. The gate must
// requeue on the poll interval; once the file gains >=2 syncers the next
// reconcile re-resolves the gate and builds the plan (unblocks).
func TestReconcile_StateSyncFailClosed_RequeuesAndUnblocks(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := stateSyncNode("ss-poll", testNamespace)
	node.Spec.ChainID = testChainID
	node.Status.Phase = seiv1alpha1.PhasePending
	r, c := newNodeReconciler(t, node)
	withSyncers(t, r, map[string][]string{testChainID: {syncerSingle}})

	res, err := r.Reconcile(ctx, nodeReqFor("ss-poll", testNamespace))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(statusPollInterval),
		"fail-closed pre-Running node must poll the mounted syncer file")

	fetched := getSeiNode(t, ctx, c, "ss-poll", testNamespace)
	g.Expect(fetched.Status.Plan).To(BeNil())
	g.Expect(stateSyncCondition(fetched).Reason).To(Equal(seiv1alpha1.ReasonStateSyncNoSyncersConfigured))

	// GitOps provisions the syncers; the mounted file now satisfies the floor.
	withSyncers(t, r, map[string][]string{testChainID: {syncerA, syncerB}})

	_, err = r.Reconcile(ctx, nodeReqFor("ss-poll", testNamespace))
	g.Expect(err).NotTo(HaveOccurred())

	fetched = getSeiNode(t, ctx, c, "ss-poll", testNamespace)
	g.Expect(fetched.Status.Plan).NotTo(BeNil(), "gate unblocks once the file has >=2 syncers")
	g.Expect(findPlannedTask(fetched.Status.Plan, planner.TaskConfigureStateSync)).NotTo(BeNil())
	g.Expect(stateSyncCondition(fetched).Status).To(Equal(metav1.ConditionTrue))
}

// Full reconcile path: a paused node still gets the always-present
// StateSyncReady condition seeded, even though reconcile returns early on
// spec.paused before the gate enforcement. State-sync enabled with no syncers
// resolves to False/NoSyncersConfigured; state-sync disabled resolves to
// False/NotApplicable. Either way the condition must be present — and a paused
// node must build NO plan (pause semantics preserved).
func TestReconcile_PausedNode_StateSyncReadyStillSeeded(t *testing.T) {
	cases := []struct {
		name        string
		node        *seiv1alpha1.SeiNode
		withSyncers bool
		wantReason  string
	}{
		{
			name:        "state-sync enabled, no syncers",
			node:        stateSyncNode("paused-ss", testNamespace),
			withSyncers: true,
			wantReason:  seiv1alpha1.ReasonStateSyncNoSyncersConfigured,
		},
		{
			name:        "no snapshot source",
			node:        newGenesisNode("paused-gen", testNamespace),
			withSyncers: false,
			wantReason:  seiv1alpha1.ReasonStateSyncNotApplicable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			ctx := context.Background()

			tc.node.Spec.Paused = true
			r, c := newNodeReconciler(t, tc.node)
			if tc.withSyncers {
				// Empty map for the chain → fail closed, but source is wired.
				withSyncers(t, r, map[string][]string{"other-chain": {syncerA, syncerB}})
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
			r, c := newNodeReconciler(t, node)
			withSyncers(t, r, map[string][]string{testChainID: {syncerSingle}})

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

// An unparseable syncer file must be transient: Paused/Failed handling and the
// status flush still run, the reconcile requeues instead of hard-aborting, and
// StateSyncReady reflects it via Unknown/SyncerSourceError. On a pre-Running
// node the unresolved gate also holds initial StatefulSet creation — the init
// plan (and with it EnsureDataPVC) is suppressed, so an STS created now would
// strand a Pending pod on a claim nothing creates.
func TestReconcile_StateSyncSyncerSourceError_Transient(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := stateSyncNode("ss-err", testNamespace)
	node.Spec.ChainID = testChainID
	r, c := newNodeReconciler(t, node)
	writeSyncerFile(t, r, "{ this is not: valid yaml: at all") // malformed

	res, err := r.Reconcile(ctx, nodeReqFor("ss-err", testNamespace))
	g.Expect(err).NotTo(HaveOccurred(), "transient syncer-source error must not hard-abort")
	g.Expect(res.RequeueAfter).To(BeNumerically(">", 0), "transient error must requeue")

	fetched := getSeiNode(t, ctx, c, "ss-err", testNamespace)
	cond := stateSyncCondition(fetched)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionUnknown))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonStateSyncSyncerSourceError))
	g.Expect(fetched.Status.Plan).To(BeNil(), "no state-sync plan while the gate is unresolved")
	// Initial STS creation is held while the gate is unresolved on a
	// pre-Running node.
	sts := &appsv1.StatefulSet{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "ss-err", Namespace: testNamespace}, sts)).NotTo(Succeed(),
		"initial StatefulSet creation must be held while the gate is unresolved")
	g.Expect(fetched.Status.StatefulSet).To(BeNil())
}

// A Running node's STS must keep syncing through a transient syncer-source
// error: the blocked gate is pre-Running-only (an update plan carries no
// state-sync task), so holding the STS here would turn a registry blip into a
// rollout stall.
func TestReconcile_RunningNode_SyncerSourceError_StatefulSetStillSynced(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := stateSyncNode("ss-run-err", testNamespace)
	node.Spec.ChainID = testChainID
	node.Status.Phase = seiv1alpha1.PhaseRunning
	r, c := newNodeReconciler(t, node)
	writeSyncerFile(t, r, "{ this is not: valid yaml: at all") // malformed

	_, err := r.Reconcile(ctx, nodeReqFor("ss-run-err", testNamespace))
	g.Expect(err).NotTo(HaveOccurred())

	sts := &appsv1.StatefulSet{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "ss-run-err", Namespace: testNamespace}, sts)).To(Succeed(),
		"a Running node's StatefulSet must sync despite a syncer-source error")
}

// A blocked pre-Running node must not get its initial StatefulSet: the
// suppressed init plan means EnsureDataPVC never runs, and the pod mounts the
// data PVC by direct claimName — an STS created now would strand a Pending
// pod. Once the gate opens, the same reconcile creates STS and plan together.
func TestReconcile_StateSyncBlocked_HoldsInitialStatefulSet(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := stateSyncNode("ss-hold", testNamespace)
	node.Spec.ChainID = testChainID
	r, c := newNodeReconciler(t, node)
	withSyncers(t, r, map[string][]string{testChainID: {syncerSingle}})

	_, err := r.Reconcile(ctx, nodeReqFor("ss-hold", testNamespace))
	g.Expect(err).NotTo(HaveOccurred())

	fetched := getSeiNode(t, ctx, c, "ss-hold", testNamespace)
	g.Expect(fetched.Status.StatefulSet).To(BeNil())
	sts := &appsv1.StatefulSet{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "ss-hold", Namespace: testNamespace}, sts)).NotTo(Succeed(),
		"no StatefulSet while the state-sync gate blocks the init plan")

	// GitOps provisions the syncers; STS and plan appear on the same reconcile.
	withSyncers(t, r, map[string][]string{testChainID: {syncerA, syncerB}})
	_, err = r.Reconcile(ctx, nodeReqFor("ss-hold", testNamespace))
	g.Expect(err).NotTo(HaveOccurred())

	fetched = getSeiNode(t, ctx, c, "ss-hold", testNamespace)
	g.Expect(fetched.Status.StatefulSet).NotTo(BeNil())
	g.Expect(fetched.Status.Plan).NotTo(BeNil())
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "ss-hold", Namespace: testNamespace}, sts)).To(Succeed())
}

// The StateSyncBlocked Warning fires once on the transition into fail-closed,
// not on every requeue.
func TestReconcile_StateSyncBlocked_EventFiresOncePerTransition(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := stateSyncNode("ss-evt", testNamespace)
	node.Spec.ChainID = testChainID
	r, _ := newNodeReconciler(t, node)
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec
	withSyncers(t, r, map[string][]string{testChainID: {syncerSingle}})

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
	r, c := newNodeReconciler(t, node)
	withSyncers(t, r, map[string][]string{
		testChainID: {"a.arctic-1.example.com:26657", "b.arctic-1.example.com:26657"},
	})

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
