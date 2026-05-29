package nodetask

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/gomega"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const (
	testNS       = "default"
	testTaskName = "upgrade-v2"
	testNodeName = "validator-0"
	testImageV1  = "ghcr.io/sei-protocol/seid:v1.0.0"
	testImageV2  = "ghcr.io/sei-protocol/seid:v2.0.0"
	testChainID  = "sei-test"
	testKeyName  = "operator"
	testFees     = "2000usei"
)

func newScheme(t *testing.T) *k8sruntime.Scheme {
	t.Helper()
	s := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := seiv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newReconciler(t *testing.T, now time.Time, objs ...client.Object) (*SeiNodeTaskReconciler, client.Client) {
	t.Helper()
	s := newScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&seiv1alpha1.SeiNodeTask{}, &seiv1alpha1.SeiNode{}).
		Build()
	r := &SeiNodeTaskReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(100),
		Platform: platformtest.Config(),
		Now:      func() time.Time { return now },
		ConfigFor: func(_ context.Context, _ *seiv1alpha1.SeiNodeTask, _ *seiv1alpha1.SeiNode) task.ExecutionConfig {
			return task.ExecutionConfig{
				KubeClient: c,
				APIReader:  c,
				Scheme:     s,
			}
		},
	}
	return r, c
}

func newUpdateImageTask() *seiv1alpha1.SeiNodeTask {
	return &seiv1alpha1.SeiNodeTask{
		ObjectMeta: metav1.ObjectMeta{Name: testTaskName, Namespace: testNS, UID: "task-uid-1", Generation: 1},
		Spec: seiv1alpha1.SeiNodeTaskSpec{
			Kind: seiv1alpha1.SeiNodeTaskKindUpdateNodeImage,
			Target: seiv1alpha1.SeiNodeTaskTarget{
				NodeRef:      seiv1alpha1.SeiNodeTaskNodeRef{Name: testNodeName},
				RequirePhase: seiv1alpha1.PhaseRunning,
			},
			UpdateNodeImage: &seiv1alpha1.UpdateNodeImagePayload{Image: testImageV2},
		},
	}
}

func newRunningNode() *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testNodeName, Namespace: testNS, UID: "node-uid-1"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "sei-test", Image: testImageV1,
			Validator: &seiv1alpha1.ValidatorSpec{},
		},
		Status: seiv1alpha1.SeiNodeStatus{
			Phase:        seiv1alpha1.PhaseRunning,
			CurrentImage: testImageV1,
		},
	}
}

func req() ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: testTaskName, Namespace: testNS}}
}

func getTask(t *testing.T, ctx context.Context, c client.Client) *seiv1alpha1.SeiNodeTask {
	t.Helper()
	out := &seiv1alpha1.SeiNodeTask{}
	if err := c.Get(ctx, types.NamespacedName{Name: testTaskName, Namespace: testNS}, out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestReconcile_NotFound(t *testing.T) {
	g := NewWithT(t)
	r, _ := newReconciler(t, time.Now())
	res, err := r.Reconcile(context.Background(), req())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
}

func TestReconcile_TargetMissing_FailsTerminal(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	cr := newUpdateImageTask()
	r, c := newReconciler(t, time.Now(), cr)

	_, err := r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())

	got := getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseFailed))
}

func TestReconcile_TargetNotRunning_WaitsThenTimesOut(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	t0 := time.Now()
	cr := newUpdateImageTask()
	short := metav1.Duration{Duration: 100 * time.Millisecond}
	cr.Spec.Target.RequirePhaseTimeout = &short
	node := newRunningNode()
	node.Status.Phase = seiv1alpha1.PhaseInitializing

	r, c := newReconciler(t, t0, cr, node)

	// First reconcile: stamps TargetFirstObservedAt, requeues.
	res, err := r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(targetWaitInterval))
	got := getTask(t, ctx, c)
	g.Expect(got.Status.TargetFirstObservedAt).NotTo(BeNil())
	g.Expect(got.Status.Phase).NotTo(Equal(seiv1alpha1.SeiNodeTaskPhaseFailed))

	// Advance past the timeout.
	r.Now = func() time.Time { return t0.Add(time.Second) }
	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	got = getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseFailed))
}

func TestReconcile_UpdateNodeImage_HappyPath(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	t0 := time.Now()
	cr := newUpdateImageTask()
	node := newRunningNode()

	r, c := newReconciler(t, t0, cr, node)

	// R1: synthesize task, phase Running, requeue immediate.
	_, err := r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	got := getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseRunning))
	g.Expect(got.Status.Task).NotTo(BeNil())
	g.Expect(got.Status.Task.ID).To(Equal(task.DeterministicTaskID(string(cr.UID), string(cr.Spec.Kind), 0)))

	// R2: Execute patches target.spec.image. Status polls, still Running.
	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	updatedNode := &seiv1alpha1.SeiNode{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: testNodeName, Namespace: testNS}, updatedNode)).To(Succeed())
	g.Expect(updatedNode.Spec.Image).To(Equal(testImageV2))
	got = getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseRunning))

	// Simulate the SeiNode controller observing rollout completion.
	updatedNode.Status.CurrentImage = testImageV2
	g.Expect(c.Status().Update(ctx, updatedNode)).To(Succeed())

	// R3: Status sees currentImage matches → Complete.
	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	got = getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseComplete))
	g.Expect(got.Status.Outputs).NotTo(BeNil())
	g.Expect(got.Status.Outputs.UpdateNodeImage).NotTo(BeNil())
	g.Expect(got.Status.Outputs.UpdateNodeImage.AppliedImage).To(Equal(testImageV2))
}

func TestReconcile_TerminalNoOp(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	cr := newUpdateImageTask()
	cr.Status.Phase = seiv1alpha1.SeiNodeTaskPhaseComplete
	r, c := newReconciler(t, time.Now(), cr)

	res, err := r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
	g.Expect(getTask(t, ctx, c).Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseComplete))
}

// fakeSidecarClient is a minimal in-memory sidecar.SidecarClient impl for
// nodetask controller tests. SubmitTask records each request and assigns the
// caller-provided UUID (deterministic — the controller stamps task IDs via
// task.DeterministicTaskID). GetTask returns whatever result has been staged
// for that ID via setResult, or sidecar.ErrNotFound otherwise.
type fakeSidecarClient struct {
	mu        sync.Mutex
	submitted []sidecar.TaskRequest
	results   map[uuid.UUID]*sidecar.TaskResult
}

func newFakeSidecarClient() *fakeSidecarClient {
	return &fakeSidecarClient{results: make(map[uuid.UUID]*sidecar.TaskResult)}
}

func (f *fakeSidecarClient) SubmitTask(_ context.Context, req sidecar.TaskRequest) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submitted = append(f.submitted, req)
	if req.Id != nil {
		return *req.Id, nil
	}
	return uuid.New(), nil
}

func (f *fakeSidecarClient) GetTask(_ context.Context, id uuid.UUID) (*sidecar.TaskResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.results[id]; ok {
		return r, nil
	}
	return nil, sidecar.ErrNotFound
}

func (f *fakeSidecarClient) Healthz(_ context.Context) (bool, error)     { return true, nil }
func (f *fakeSidecarClient) GetNodeID(_ context.Context) (string, error) { return "", nil }

func (f *fakeSidecarClient) setResult(id uuid.UUID, status sidecar.TaskResultStatus, errMsg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	res := &sidecar.TaskResult{
		Id:          id,
		Status:      status,
		Type:        "test",
		SubmittedAt: time.Now(),
	}
	if errMsg != "" {
		e := errMsg
		res.Error = &e
	}
	f.results[id] = res
}

// newReconcilerWithSidecar wires a reconciler whose ExecutionConfig.BuildSidecarClient
// returns the supplied fake. Separate from newReconciler because the 5 existing
// non-sidecar tests do not need (and should not pay for) a sidecar fixture.
// Intentionally returns only (reconciler, client) — the test retains its own
// reference to the fake it passed in, so a third return value would just be
// discarded with `_` at every call site (unparam).
func newReconcilerWithSidecar(t *testing.T, now time.Time, sc task.SidecarClient, objs ...client.Object) (*SeiNodeTaskReconciler, client.Client) {
	t.Helper()
	s := newScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&seiv1alpha1.SeiNodeTask{}, &seiv1alpha1.SeiNode{}).
		Build()
	r := &SeiNodeTaskReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(100),
		Platform: platformtest.Config(),
		Now:      func() time.Time { return now },
		ConfigFor: func(_ context.Context, _ *seiv1alpha1.SeiNodeTask, _ *seiv1alpha1.SeiNode) task.ExecutionConfig {
			return task.ExecutionConfig{
				KubeClient:         c,
				APIReader:          c,
				Scheme:             s,
				BuildSidecarClient: func() (task.SidecarClient, error) { return sc, nil },
			}
		},
	}
	return r, c
}

// newGovVoteTask is a fixture for GovVote test cases; payload values are the
// minimum to pass validation (chainId/keyName/proposalId/option/fees/gas).
func newGovVoteTask() *seiv1alpha1.SeiNodeTask {
	return &seiv1alpha1.SeiNodeTask{
		ObjectMeta: metav1.ObjectMeta{Name: testTaskName, Namespace: testNS, UID: "task-uid-govvote", Generation: 1},
		Spec: seiv1alpha1.SeiNodeTaskSpec{
			Kind: seiv1alpha1.SeiNodeTaskKindGovVote,
			Target: seiv1alpha1.SeiNodeTaskTarget{
				NodeRef:      seiv1alpha1.SeiNodeTaskNodeRef{Name: testNodeName},
				RequirePhase: seiv1alpha1.PhaseRunning,
			},
			GovVote: &seiv1alpha1.GovVotePayload{
				ChainID:    testChainID,
				KeyName:    testKeyName,
				ProposalID: 42,
				Option:     "yes",
				Fees:       testFees,
				Gas:        200000,
			},
		},
	}
}

func TestTaskParamsForKind_GovVote(t *testing.T) {
	g := NewWithT(t)
	cr := newGovVoteTask()
	taskType, raw, err := taskParamsForKind(cr, nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(taskType).To(Equal(sidecar.TaskTypeGovVote))

	var got sidecar.GovVoteTask
	g.Expect(json.Unmarshal(raw, &got)).To(Succeed())
	g.Expect(got).To(Equal(sidecar.GovVoteTask{
		ChainID:    testChainID,
		KeyName:    testKeyName,
		ProposalID: 42,
		Option:     "yes",
		Fees:       testFees,
		Gas:        200000,
	}))
}

func TestTaskParamsForKind_GovSoftwareUpgrade(t *testing.T) {
	g := NewWithT(t)
	cr := &seiv1alpha1.SeiNodeTask{
		ObjectMeta: metav1.ObjectMeta{Name: testTaskName, Namespace: testNS, UID: "task-uid-gsu", Generation: 1},
		Spec: seiv1alpha1.SeiNodeTaskSpec{
			Kind: seiv1alpha1.SeiNodeTaskKindGovSoftwareUpgrade,
			Target: seiv1alpha1.SeiNodeTaskTarget{
				NodeRef: seiv1alpha1.SeiNodeTaskNodeRef{Name: testNodeName},
			},
			GovSoftwareUpgrade: &seiv1alpha1.GovSoftwareUpgradePayload{
				ChainID:        testChainID,
				KeyName:        testKeyName,
				Title:          "v2 upgrade",
				Description:    "schedule v2",
				UpgradeName:    "v2",
				UpgradeHeight:  1_000_000,
				InitialDeposit: "10000000usei",
				Fees:           testFees,
				Gas:            300000,
			},
		},
	}

	taskType, raw, err := taskParamsForKind(cr, nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(taskType).To(Equal(sidecar.TaskTypeGovSoftwareUpgrade))

	var got sidecar.GovSoftwareUpgradeTask
	g.Expect(json.Unmarshal(raw, &got)).To(Succeed())
	g.Expect(got.ChainID).To(Equal(testChainID))
	g.Expect(got.UpgradeHeight).To(Equal(int64(1_000_000)))
	g.Expect(got.UpgradeName).To(Equal("v2"))
	g.Expect(got.Fees).To(Equal(testFees))
}

// Empty keyName on the CR + .secret unset on the target → controller resolves
// to the gentx convention uid "validator".
func TestTaskParamsForKind_GovVote_DerivesKeyNameFromGentxDefault(t *testing.T) {
	g := NewWithT(t)
	cr := newGovVoteTask()
	cr.Spec.GovVote.KeyName = ""
	target := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testNodeName, Namespace: testNS},
		Spec:       seiv1alpha1.SeiNodeSpec{Validator: &seiv1alpha1.ValidatorSpec{}},
	}

	_, raw, err := taskParamsForKind(cr, target)
	g.Expect(err).NotTo(HaveOccurred())
	var got sidecar.GovVoteTask
	g.Expect(json.Unmarshal(raw, &got)).To(Succeed())
	g.Expect(got.KeyName).To(Equal(seiv1alpha1.GentxOperatorKeyName),
		"empty CR keyName + no .secret resolves to the gentx-written uid")
}

// Empty keyName on the CR + .secret set → controller resolves to the Secret's
// declared KeyName (default node_admin).
func TestTaskParamsForKind_GovVote_DerivesKeyNameFromSecret(t *testing.T) {
	g := NewWithT(t)
	cr := newGovVoteTask()
	cr.Spec.GovVote.KeyName = ""
	target := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testNodeName, Namespace: testNS},
		Spec: seiv1alpha1.SeiNodeSpec{
			Validator: &seiv1alpha1.ValidatorSpec{
				OperatorKeyring: &seiv1alpha1.OperatorKeyringSource{
					Secret: &seiv1alpha1.SecretOperatorKeyringSource{
						SecretName: "validator-0-opk",
						KeyName:    "custom_operator",
					},
				},
			},
		},
	}

	_, raw, err := taskParamsForKind(cr, target)
	g.Expect(err).NotTo(HaveOccurred())
	var got sidecar.GovVoteTask
	g.Expect(json.Unmarshal(raw, &got)).To(Succeed())
	g.Expect(got.KeyName).To(Equal("custom_operator"))
}

// Explicit keyName on the CR wins over derivation from target.
func TestTaskParamsForKind_GovVote_ExplicitKeyNameOverrides(t *testing.T) {
	g := NewWithT(t)
	cr := newGovVoteTask()
	cr.Spec.GovVote.KeyName = "explicit_override"
	target := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testNodeName, Namespace: testNS},
		Spec: seiv1alpha1.SeiNodeSpec{
			Validator: &seiv1alpha1.ValidatorSpec{
				OperatorKeyring: &seiv1alpha1.OperatorKeyringSource{
					Secret: &seiv1alpha1.SecretOperatorKeyringSource{
						SecretName: "validator-0-opk",
						KeyName:    "would_be_picked_if_empty",
					},
				},
			},
		},
	}

	_, raw, err := taskParamsForKind(cr, target)
	g.Expect(err).NotTo(HaveOccurred())
	var got sidecar.GovVoteTask
	g.Expect(json.Unmarshal(raw, &got)).To(Succeed())
	g.Expect(got.KeyName).To(Equal("explicit_override"))
}

func TestTaskParamsForKind_AwaitCondition_Height(t *testing.T) {
	g := NewWithT(t)
	cr := &seiv1alpha1.SeiNodeTask{
		ObjectMeta: metav1.ObjectMeta{Name: testTaskName, Namespace: testNS, UID: "task-uid-await", Generation: 1},
		Spec: seiv1alpha1.SeiNodeTaskSpec{
			Kind: seiv1alpha1.SeiNodeTaskKindAwaitCondition,
			Target: seiv1alpha1.SeiNodeTaskTarget{
				NodeRef: seiv1alpha1.SeiNodeTaskNodeRef{Name: testNodeName},
			},
			AwaitCondition: &seiv1alpha1.AwaitConditionPayload{
				Height: &seiv1alpha1.AwaitHeightCondition{TargetHeight: 12345},
				Action: sidecar.ActionSIGTERM,
			},
		},
	}

	taskType, raw, err := taskParamsForKind(cr, nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(taskType).To(Equal(sidecar.TaskTypeAwaitCondition))

	var got sidecar.AwaitConditionTask
	g.Expect(json.Unmarshal(raw, &got)).To(Succeed())
	g.Expect(got).To(Equal(sidecar.AwaitConditionTask{
		Condition:    sidecar.ConditionHeight,
		TargetHeight: 12345,
		Action:       sidecar.ActionSIGTERM,
	}))
}

func TestTaskParamsForKind_AwaitCondition_MissingHeight(t *testing.T) {
	g := NewWithT(t)
	cr := &seiv1alpha1.SeiNodeTask{
		ObjectMeta: metav1.ObjectMeta{Name: testTaskName, Namespace: testNS, UID: "task-uid-await-missing", Generation: 1},
		Spec: seiv1alpha1.SeiNodeTaskSpec{
			Kind: seiv1alpha1.SeiNodeTaskKindAwaitCondition,
			Target: seiv1alpha1.SeiNodeTaskTarget{
				NodeRef: seiv1alpha1.SeiNodeTaskNodeRef{Name: testNodeName},
			},
			AwaitCondition: &seiv1alpha1.AwaitConditionPayload{},
		},
	}

	_, _, err := taskParamsForKind(cr, nil)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("height is required"))
}

// AwaitNodesAtHeight on a single-node target maps to the sidecar's
// await-condition(height=H) primitive. Pinning the routing decision so a
// future refactor cannot silently switch to a deployment-scoped fan-out
// task without an explicit test failure.
func TestTaskParamsForKind_AwaitNodesAtHeight_MapsToAwaitCondition(t *testing.T) {
	g := NewWithT(t)
	cr := &seiv1alpha1.SeiNodeTask{
		ObjectMeta: metav1.ObjectMeta{Name: testTaskName, Namespace: testNS, UID: "task-uid-anah", Generation: 1},
		Spec: seiv1alpha1.SeiNodeTaskSpec{
			Kind: seiv1alpha1.SeiNodeTaskKindAwaitNodesAtHeight,
			Target: seiv1alpha1.SeiNodeTaskTarget{
				NodeRef: seiv1alpha1.SeiNodeTaskNodeRef{Name: testNodeName},
			},
			AwaitNodesAtHeight: &seiv1alpha1.AwaitNodesAtHeightPayload{TargetHeight: 555},
		},
	}

	taskType, raw, err := taskParamsForKind(cr, nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(taskType).To(Equal(sidecar.TaskTypeAwaitCondition))

	var got sidecar.AwaitConditionTask
	g.Expect(json.Unmarshal(raw, &got)).To(Succeed())
	g.Expect(got.Condition).To(Equal(sidecar.ConditionHeight))
	g.Expect(got.TargetHeight).To(Equal(int64(555)))
}

func TestTaskParamsForKind_MissingPayloads(t *testing.T) {
	cases := []struct {
		name string
		kind seiv1alpha1.SeiNodeTaskKind
	}{
		{"UpdateNodeImage", seiv1alpha1.SeiNodeTaskKindUpdateNodeImage},
		{"GovVote", seiv1alpha1.SeiNodeTaskKindGovVote},
		{"GovSoftwareUpgrade", seiv1alpha1.SeiNodeTaskKindGovSoftwareUpgrade},
		{"AwaitCondition", seiv1alpha1.SeiNodeTaskKindAwaitCondition},
		{"AwaitNodesAtHeight", seiv1alpha1.SeiNodeTaskKindAwaitNodesAtHeight},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			cr := &seiv1alpha1.SeiNodeTask{
				ObjectMeta: metav1.ObjectMeta{Name: testTaskName, Namespace: testNS, UID: "task-uid-nopayload", Generation: 1},
				Spec: seiv1alpha1.SeiNodeTaskSpec{
					Kind: tc.kind,
					Target: seiv1alpha1.SeiNodeTaskTarget{
						NodeRef: seiv1alpha1.SeiNodeTaskNodeRef{Name: testNodeName},
					},
				},
			}
			_, _, err := taskParamsForKind(cr, nil)
			g.Expect(err).To(HaveOccurred(), "kind %s with nil payload must error", tc.kind)
		})
	}
}

func TestReconcile_GovVote_EndToEnd(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	t0 := time.Now()
	cr := newGovVoteTask()
	node := newRunningNode()
	fakeSC := newFakeSidecarClient()

	r, c := newReconcilerWithSidecar(t, t0, fakeSC, cr, node)

	// R1: synthesize task. The deterministic task ID is derived from
	// UID + Kind; we recompute it to drive the fake.
	_, err := r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	got := getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseRunning))
	g.Expect(got.Status.Task).NotTo(BeNil())
	taskIDStr := got.Status.Task.ID
	taskID, perr := uuid.Parse(taskIDStr)
	g.Expect(perr).NotTo(HaveOccurred())

	// R2: Execute submits to the fake sidecar; Status polls. Sidecar has
	// not produced a result yet → fake returns ErrNotFound → still Running.
	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	got = getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseRunning))
	g.Expect(got.Status.Task.SubmittedAt).NotTo(BeNil())

	// Verify the submitted TaskRequest body. The dispatcher must marshal
	// PascalCase fields on sidecar.GovVoteTask, which the sidecar Go
	// client serializes as camelCase via ToTaskRequest. The dispatcher
	// however marshals the struct directly — relying on field names. The
	// assertion is loose (presence) rather than exact wire shape because
	// the sidecar handler unmarshals back into the same Go struct.
	fakeSC.mu.Lock()
	submittedCount := len(fakeSC.submitted)
	fakeSC.mu.Unlock()
	g.Expect(submittedCount).To(BeNumerically(">=", 1))

	// Stage a Completed result for the synthesized task ID.
	fakeSC.setResult(taskID, sidecar.Completed, "")

	// R3: Status sees Completed → phase Complete.
	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	got = getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseComplete))
	g.Expect(got.Status.Task.Status).To(Equal(seiv1alpha1.TaskComplete))

	// Regression guard: typed GovVote outputs intentionally stay nil in
	// this PR. populateOutputs only stamps UpdateNodeImage today; flipping
	// that without updating the LLD (chain-as-medium, no task-to-task
	// currying) should fail this assertion loudly.
	if got.Status.Outputs != nil {
		g.Expect(got.Status.Outputs.GovVote).To(BeNil(),
			"populateOutputs unexpectedly populated GovVote — see PR 3 scope notes in controller.go")
	}
}

func TestReconcile_GovVote_SidecarFailure(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	t0 := time.Now()
	cr := newGovVoteTask()
	node := newRunningNode()
	fakeSC := newFakeSidecarClient()

	r, c := newReconcilerWithSidecar(t, t0, fakeSC, cr, node)

	// R1: synthesize.
	_, err := r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	got := getTask(t, ctx, c)
	taskID, perr := uuid.Parse(got.Status.Task.ID)
	g.Expect(perr).NotTo(HaveOccurred())

	// Stage the sidecar failure result BEFORE R2 so the first Status poll
	// after Execute sees Failed.
	const sidecarErr = "rpc: insufficient fees"
	fakeSC.setResult(taskID, sidecar.Failed, sidecarErr)

	// R2: Execute submits; Status polls and sees Failed → CR marked Failed.
	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	got = getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseFailed))
	g.Expect(got.Status.Task.Status).To(Equal(seiv1alpha1.TaskFailed))
	g.Expect(got.Status.Task.Err).To(ContainSubstring(sidecarErr))
}
