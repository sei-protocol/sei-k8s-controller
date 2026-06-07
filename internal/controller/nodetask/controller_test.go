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
	testPeerAddr = "abc@10.0.0.1:26656"

	testPeerRegion     = "us-east-1"
	testPeerTagKey     = "role"
	testPeerTagVal     = "validator"
	testNDPSelectorKey = "sei.io/nodedeployment"
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
		ConfigFor: func(_ context.Context, _ *seiv1alpha1.SeiNodeTask, target *seiv1alpha1.SeiNode) task.ExecutionConfig {
			return task.ExecutionConfig{
				KubeClient: c,
				APIReader:  c,
				Scheme:     s,
				Resource:   target,
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
	getCalls  int
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
	f.getCalls++
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

// ---------------------------------------------------------------------------
// DiscoverPeers
// ---------------------------------------------------------------------------

func newDiscoverPeersTask() *seiv1alpha1.SeiNodeTask {
	return &seiv1alpha1.SeiNodeTask{
		ObjectMeta: metav1.ObjectMeta{Name: testTaskName, Namespace: testNS, UID: "task-uid-discover", Generation: 1},
		Spec: seiv1alpha1.SeiNodeTaskSpec{
			Kind: seiv1alpha1.SeiNodeTaskKindDiscoverPeers,
			Target: seiv1alpha1.SeiNodeTaskTarget{
				NodeRef:      seiv1alpha1.SeiNodeTaskNodeRef{Name: testNodeName},
				RequirePhase: seiv1alpha1.PhaseRunning,
			},
			DiscoverPeers: &seiv1alpha1.DiscoverPeersPayload{},
		},
	}
}

// nil target (early-validation path) must not error and must not build sources
// yet — source-building is deferred to driveTask, which has the target.
func TestTaskParamsForKind_DiscoverPeers_NilTargetDefersSources(t *testing.T) {
	g := NewWithT(t)
	cr := newDiscoverPeersTask()
	taskType, raw, err := taskParamsForKind(cr, nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(taskType).To(Equal(sidecar.TaskTypeDiscoverPeers))
	g.Expect(raw).To(BeNil())
}

// ec2Tags + label sources resolve into one ec2Tags PeerSource and one static
// PeerSource carrying the target's status.resolvedPeers (label routes as
// static so the sidecar writes the composed entries verbatim).
func TestTaskParamsForKind_DiscoverPeers_EC2TagsAndLabel(t *testing.T) {
	g := NewWithT(t)
	cr := newDiscoverPeersTask()
	target := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testNodeName, Namespace: testNS},
		Spec: seiv1alpha1.SeiNodeSpec{
			Peers: []seiv1alpha1.PeerSource{
				{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{
					Region: testPeerRegion,
					Tags:   map[string]string{testPeerTagKey: testPeerTagVal},
				}},
				{Label: &seiv1alpha1.LabelPeerSource{
					Selector: map[string]string{testNDPSelectorKey: "g1"},
				}},
			},
		},
		Status: seiv1alpha1.SeiNodeStatus{
			ResolvedPeers: []string{testPeerAddr},
		},
	}

	taskType, raw, err := taskParamsForKind(cr, target)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(taskType).To(Equal(sidecar.TaskTypeDiscoverPeers))

	var got sidecar.DiscoverPeersTask
	g.Expect(json.Unmarshal(raw, &got)).To(Succeed())
	g.Expect(got.Sources).To(Equal([]sidecar.PeerSource{
		{Type: sidecar.PeerSourceEC2Tags, Region: testPeerRegion, Tags: map[string]string{testPeerTagKey: testPeerTagVal}},
		{Type: sidecar.PeerSourceStatic, Addresses: []string{testPeerAddr}},
	}))
}

// ec2Tags + label where the label has not resolved yet (empty resolvedPeers):
// the empty label source is skipped, but the valid ec2Tags source is preserved —
// the skip must not nuke a mixed source set, and len(sources)>0 so it does not
// trip the empty-peers fail-fast.
func TestTaskParamsForKind_DiscoverPeers_EC2TagsAndEmptyLabel(t *testing.T) {
	g := NewWithT(t)
	cr := newDiscoverPeersTask()
	target := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testNodeName, Namespace: testNS},
		Spec: seiv1alpha1.SeiNodeSpec{
			Peers: []seiv1alpha1.PeerSource{
				{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{
					Region: testPeerRegion,
					Tags:   map[string]string{testPeerTagKey: testPeerTagVal},
				}},
				{Label: &seiv1alpha1.LabelPeerSource{
					Selector: map[string]string{testNDPSelectorKey: "g1"},
				}},
			},
		},
		// ResolvedPeers intentionally empty: label has not resolved.
	}

	taskType, raw, err := taskParamsForKind(cr, target)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(taskType).To(Equal(sidecar.TaskTypeDiscoverPeers))

	var got sidecar.DiscoverPeersTask
	g.Expect(json.Unmarshal(raw, &got)).To(Succeed())
	g.Expect(got.Sources).To(Equal([]sidecar.PeerSource{
		{Type: sidecar.PeerSourceEC2Tags, Region: testPeerRegion, Tags: map[string]string{testPeerTagKey: testPeerTagVal}},
	}))
}

func TestTaskParamsForKind_DiscoverPeers_Static(t *testing.T) {
	g := NewWithT(t)
	cr := newDiscoverPeersTask()
	target := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testNodeName, Namespace: testNS},
		Spec: seiv1alpha1.SeiNodeSpec{
			Peers: []seiv1alpha1.PeerSource{
				{Static: &seiv1alpha1.StaticPeerSource{Addresses: []string{"def@10.0.0.2:26656"}}},
			},
		},
	}

	_, raw, err := taskParamsForKind(cr, target)
	g.Expect(err).NotTo(HaveOccurred())
	var got sidecar.DiscoverPeersTask
	g.Expect(json.Unmarshal(raw, &got)).To(Succeed())
	g.Expect(got.Sources).To(Equal([]sidecar.PeerSource{
		{Type: sidecar.PeerSourceStatic, Addresses: []string{"def@10.0.0.2:26656"}},
	}))
}

// Empty spec.peers on the target → the task fails fast with a clear error
// rather than submitting an invalid (zero-source) discover-peers task that the
// sidecar would reject.
func TestTaskParamsForKind_DiscoverPeers_EmptyPeers_Errors(t *testing.T) {
	g := NewWithT(t)
	cr := newDiscoverPeersTask()
	target := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testNodeName, Namespace: testNS},
		Spec:       seiv1alpha1.SeiNodeSpec{}, // no peers
	}

	_, _, err := taskParamsForKind(cr, target)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("no usable peer sources"))
}

// A label-only target whose status.resolvedPeers is still empty yields zero
// usable sources: the nodetask builder fails fast rather than submitting a
// zero-source discover-peers (which would wipe persistent-peers). This is the
// nodetask's deliberate divergence from the planner's init path, which would
// freeze a zero-address static source and submit it.
func TestTaskParamsForKind_DiscoverPeers_LabelUnresolved_FailsFast(t *testing.T) {
	g := NewWithT(t)
	cr := newDiscoverPeersTask()
	target := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testNodeName, Namespace: testNS},
		Spec: seiv1alpha1.SeiNodeSpec{
			Peers: []seiv1alpha1.PeerSource{
				{Label: &seiv1alpha1.LabelPeerSource{Selector: map[string]string{testNDPSelectorKey: "g1"}}},
			},
		},
		// ResolvedPeers intentionally empty.
	}

	_, _, err := taskParamsForKind(cr, target)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("no usable peer sources"))
}

func TestReconcile_DiscoverPeers_EndToEnd(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	t0 := time.Now()
	cr := newDiscoverPeersTask()
	node := newRunningNode()
	node.Spec.Peers = []seiv1alpha1.PeerSource{
		{Static: &seiv1alpha1.StaticPeerSource{Addresses: []string{testPeerAddr}}},
	}
	fakeSC := newFakeSidecarClient()

	r, c := newReconcilerWithSidecar(t, t0, fakeSC, cr, node)

	// R1: synthesize task.
	_, err := r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	got := getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseRunning))
	taskID, perr := uuid.Parse(got.Status.Task.ID)
	g.Expect(perr).NotTo(HaveOccurred())

	// R2: Execute submits discover-peers to the sidecar.
	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	fakeSC.mu.Lock()
	g.Expect(fakeSC.submitted).To(HaveLen(1))
	g.Expect(fakeSC.submitted[0].Type).To(Equal(sidecar.TaskTypeDiscoverPeers))
	fakeSC.mu.Unlock()

	// Sidecar completes the task.
	fakeSC.setResult(taskID, sidecar.Completed, "")
	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	got = getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseComplete))
}

// Empty peers on the target surfaces as a terminal Failed during driveTask.
func TestReconcile_DiscoverPeers_EmptyPeers_Fails(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	cr := newDiscoverPeersTask()
	node := newRunningNode() // no spec.peers
	fakeSC := newFakeSidecarClient()

	r, c := newReconcilerWithSidecar(t, time.Now(), fakeSC, cr, node)

	// R1 synthesize, R2 driveTask → ParamsBuildFailed → Failed.
	_, err := r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())

	got := getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseFailed))
	failed := findFailedCond(got)
	g.Expect(failed).NotTo(BeNil())
	g.Expect(failed.Reason).To(Equal("ParamsBuildFailed"))
}

// The execution timeout is measured from execution start (when the target
// became Ready and the task was dispatched), NOT from status.startedAt. A
// target that reaches Running only after a wait LONGER than the per-kind
// default exec timeout (DiscoverPeers=2m) must NOT immediately Fail(Timeout):
// execution gets its full budget from when it starts. Regression guard for the
// "Task timeout includes target wait" finding.
func TestReconcile_DiscoverPeers_LongTargetWait_DoesNotImmediatelyTimeOut(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	t0 := time.Now()
	cr := newDiscoverPeersTask()
	// requirePhaseTimeout (default 5m) must comfortably exceed the wait we
	// simulate (3m > DiscoverPeers exec default 2m, < 5m requirePhase budget).
	node := newRunningNode()
	node.Status.Phase = seiv1alpha1.PhaseInitializing
	node.Spec.Peers = []seiv1alpha1.PeerSource{
		{Static: &seiv1alpha1.StaticPeerSource{Addresses: []string{testPeerAddr}}},
	}
	fakeSC := newFakeSidecarClient()

	r, c := newReconcilerWithSidecar(t, t0, fakeSC, cr, node)

	// R1: target not Running → wait. Stamps startedAt=t0.
	res, err := r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(targetWaitInterval))
	g.Expect(getTask(t, ctx, c).Status.Phase).NotTo(Equal(seiv1alpha1.SeiNodeTaskPhaseFailed))

	// Target reaches Running only after 3m — longer than the 2m DiscoverPeers
	// default exec timeout, but within the 5m requirePhase budget.
	tReady := t0.Add(defaultDiscoverPeersTimeout + time.Minute)
	r.Now = func() time.Time { return tReady }
	node.Status.Phase = seiv1alpha1.PhaseRunning
	g.Expect(c.Status().Update(ctx, node)).To(Succeed())

	// R2: target now Ready → synthesize task. executionStartedAt=tReady.
	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	got := getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseRunning))
	g.Expect(got.Status.Task).NotTo(BeNil())
	g.Expect(got.Status.Task.ExecutionStartedAt).NotTo(BeNil())
	// Stamped at synthesis (R2), not at first reconcile (R1, t0).
	g.Expect(got.Status.Task.ExecutionStartedAt.Time).To(BeTemporally("~", tReady, time.Second))
	g.Expect(got.Status.Task.ExecutionStartedAt.Time).NotTo(BeTemporally("~", t0, time.Second))

	// R3: Execute submits; Status still running. now is just past tReady — the
	// task must NOT be Failed even though now-startedAt already exceeds the 2m
	// exec timeout (the wait must not be charged against the exec budget).
	r.Now = func() time.Time { return tReady.Add(time.Second) }
	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	got = getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseRunning))

	// Sidecar completes within the exec budget → Complete.
	taskID, perr := uuid.Parse(got.Status.Task.ID)
	g.Expect(perr).NotTo(HaveOccurred())
	fakeSC.setResult(taskID, sidecar.Completed, "")
	r.Now = func() time.Time { return tReady.Add(time.Minute) }
	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getTask(t, ctx, c).Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseComplete))
}

// ---------------------------------------------------------------------------
// MarkReady
// ---------------------------------------------------------------------------

func newMarkReadyTask() *seiv1alpha1.SeiNodeTask {
	return &seiv1alpha1.SeiNodeTask{
		ObjectMeta: metav1.ObjectMeta{Name: testTaskName, Namespace: testNS, UID: "task-uid-markready", Generation: 1},
		Spec: seiv1alpha1.SeiNodeTaskSpec{
			Kind: seiv1alpha1.SeiNodeTaskKindMarkReady,
			Target: seiv1alpha1.SeiNodeTaskTarget{
				NodeRef:      seiv1alpha1.SeiNodeTaskNodeRef{Name: testNodeName},
				RequirePhase: seiv1alpha1.PhaseRunning,
			},
			MarkReady: &seiv1alpha1.MarkReadyPayload{},
		},
	}
}

// MarkReady is fire-and-forget (registered sidecarTask[...](true)): Execute
// completes the task the moment SubmitTask is acked, so Complete means "the
// mark-ready request was accepted" and lands at the submit reconcile, with
// Status reading back the cached terminal state. The test pins that contract:
// Complete at R2, and getCalls==0 confirms the completion served from cache.
func TestReconcile_MarkReady_EndToEnd(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	t0 := time.Now()
	cr := newMarkReadyTask()
	node := newRunningNode()
	fakeSC := newFakeSidecarClient()

	r, c := newReconcilerWithSidecar(t, t0, fakeSC, cr, node)

	// R1: synthesize task.
	_, err := r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getTask(t, ctx, c).Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseRunning))

	// R2: Execute submits mark-ready and completes immediately on the ack —
	// no staged result, no further reconcile required.
	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getTask(t, ctx, c).Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseComplete))

	fakeSC.mu.Lock()
	defer fakeSC.mu.Unlock()
	g.Expect(fakeSC.submitted).To(HaveLen(1))
	g.Expect(fakeSC.submitted[0].Type).To(Equal(sidecar.TaskTypeMarkReady))
	// Completion served from the cached terminal state set at submit; getCalls==0
	// pins the fire-and-forget contract (a sidecarTask[...](false) regression,
	// which polls GetTask to terminal, would trip this).
	g.Expect(fakeSC.getCalls).To(Equal(0))
}

// Execution-start timeout still fires: a DiscoverPeers task whose sidecar never
// completes Fails(Timeout) at executionStartedAt + default, confirming the
// budget is enforced (just from the right reference point).
func TestReconcile_DiscoverPeers_ExecTimeout_FromExecutionStart(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	t0 := time.Now()
	cr := newDiscoverPeersTask()
	node := newRunningNode()
	node.Spec.Peers = []seiv1alpha1.PeerSource{
		{Static: &seiv1alpha1.StaticPeerSource{Addresses: []string{testPeerAddr}}},
	}
	fakeSC := newFakeSidecarClient()

	r, c := newReconcilerWithSidecar(t, t0, fakeSC, cr, node)

	_, err := r.Reconcile(ctx, req()) // R1 synthesize (executionStartedAt=t0)
	g.Expect(err).NotTo(HaveOccurred())
	_, err = r.Reconcile(ctx, req()) // R2 Execute + Status → Running (sidecar never completes)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getTask(t, ctx, c).Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseRunning))

	r.Now = func() time.Time { return t0.Add(defaultDiscoverPeersTimeout + time.Second) }
	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	got := getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseFailed))
	failed := findFailedCond(got)
	g.Expect(failed).NotTo(BeNil())
	g.Expect(failed.Reason).To(Equal("Timeout"))
}

// Backward-compat: a task synthesized by a pre-ExecutionStartedAt controller has
// a populated status.task but a nil anchor. The first post-upgrade reconcile must
// lazy-stamp executionStartedAt (to now, not status.startedAt) so the task gets a
// bounded fresh budget instead of running unbounded, then time out at
// stamp + effectiveTimeout. Regression guard for the "In-flight tasks lose
// timeout" finding.
func TestReconcile_DiscoverPeers_PreUpgradeNilExecutionStart_StampsAndTimesOut(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	t0 := time.Now()
	cr := newDiscoverPeersTask()
	node := newRunningNode()
	node.Spec.Peers = []seiv1alpha1.PeerSource{
		{Static: &seiv1alpha1.StaticPeerSource{Addresses: []string{testPeerAddr}}},
	}
	// Simulate a pre-upgrade in-flight task: already Running, status.task populated
	// and submitted, but executionStartedAt nil (the field did not exist).
	submitted := metav1.NewTime(t0.Add(-time.Hour))
	cr.Status.Phase = seiv1alpha1.SeiNodeTaskPhaseRunning
	cr.Status.StartedAt = &submitted
	cr.Status.Task = &seiv1alpha1.SeiNodeTaskExecution{
		ID:                 task.DeterministicTaskID(string(cr.UID), string(cr.Spec.Kind), 0),
		Status:             seiv1alpha1.TaskPending,
		SubmittedAt:        &submitted,
		ExecutionStartedAt: nil,
	}
	fakeSC := newFakeSidecarClient()

	r, c := newReconcilerWithSidecar(t, t0, fakeSC, cr, node)

	// R1: sidecar still running (no result set). The nil anchor is lazy-stamped to
	// now (t0), NOT status.startedAt (1h ago) — anchoring to startedAt would put the
	// deadline in the past and spuriously fail on the first reconcile.
	_, err := r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	got := getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseRunning))
	g.Expect(got.Status.Task.ExecutionStartedAt).NotTo(BeNil())
	g.Expect(got.Status.Task.ExecutionStartedAt.Time).To(BeTemporally("~", t0, time.Second))

	// Past the fresh budget measured from the upgrade moment → Failed(Timeout).
	r.Now = func() time.Time { return t0.Add(defaultDiscoverPeersTimeout + time.Second) }
	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	got = getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseFailed))
	failed := findFailedCond(got)
	g.Expect(failed).NotTo(BeNil())
	g.Expect(failed.Reason).To(Equal("Timeout"))
}

// ---------------------------------------------------------------------------
// RestartSeid
// ---------------------------------------------------------------------------

func newRestartSeidTask() *seiv1alpha1.SeiNodeTask {
	return &seiv1alpha1.SeiNodeTask{
		ObjectMeta: metav1.ObjectMeta{Name: testTaskName, Namespace: testNS, UID: "task-uid-restart", Generation: 1},
		Spec: seiv1alpha1.SeiNodeTaskSpec{
			Kind: seiv1alpha1.SeiNodeTaskKindRestartSeid,
			Target: seiv1alpha1.SeiNodeTaskTarget{
				NodeRef:      seiv1alpha1.SeiNodeTaskNodeRef{Name: testNodeName},
				RequirePhase: seiv1alpha1.PhaseRunning,
			},
			RestartSeid: &seiv1alpha1.RestartSeidPayload{},
		},
	}
}

func TestTaskParamsForKind_RestartSeid(t *testing.T) {
	g := NewWithT(t)
	cr := newRestartSeidTask()
	taskType, raw, err := taskParamsForKind(cr, nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(taskType).To(Equal(sidecar.TaskTypeRestartSeid))

	var got sidecar.RestartSeidTask
	g.Expect(json.Unmarshal(raw, &got)).To(Succeed())
	g.Expect(got).To(Equal(sidecar.RestartSeidTask{}))
}

// RestartSeid is poll-to-completion (registered sidecarTask[...](false)): unlike
// MarkReady's fire-and-forget ack, the controller polls GetTask until the
// restart-seid task reports terminal (seid's RPC back up). Mirrors the
// DiscoverPeers poll shape.
func TestReconcile_RestartSeid_EndToEnd(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	t0 := time.Now()
	cr := newRestartSeidTask()
	node := newRunningNode()
	fakeSC := newFakeSidecarClient()

	r, c := newReconcilerWithSidecar(t, t0, fakeSC, cr, node)

	// R1: synthesize task.
	_, err := r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	got := getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseRunning))
	taskID, perr := uuid.Parse(got.Status.Task.ID)
	g.Expect(perr).NotTo(HaveOccurred())

	// R2: Execute submits restart-seid to the sidecar; not yet terminal → Running.
	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	fakeSC.mu.Lock()
	g.Expect(fakeSC.submitted).To(HaveLen(1))
	g.Expect(fakeSC.submitted[0].Type).To(Equal(sidecar.TaskTypeRestartSeid))
	fakeSC.mu.Unlock()
	g.Expect(getTask(t, ctx, c).Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseRunning))
	// Poll contract: the controller queried GetTask (not served from a submit-time
	// cache like a fire-and-forget task would).
	fakeSC.mu.Lock()
	g.Expect(fakeSC.getCalls).To(BeNumerically(">", 0))
	fakeSC.mu.Unlock()

	// Sidecar reports the restart-seid task complete (seid RPC back up).
	fakeSC.setResult(taskID, sidecar.Completed, "")
	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	got = getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseComplete))
}

// A RestartSeid whose sidecar restart-seid never completes must transition to
// Failed at the per-kind default timeout (spec.timeoutSeconds=0), not requeue
// forever.
func TestReconcile_RestartSeid_NeverCompletes_TimesOut(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	t0 := time.Now()
	cr := newRestartSeidTask() // timeoutSeconds unset → default applies
	node := newRunningNode()
	fakeSC := newFakeSidecarClient()

	r, c := newReconcilerWithSidecar(t, t0, fakeSC, cr, node)

	_, err := r.Reconcile(ctx, req()) // R1 synthesize (executionStartedAt=t0)
	g.Expect(err).NotTo(HaveOccurred())
	_, err = r.Reconcile(ctx, req()) // R2 Execute + Status → Running (never completes)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getTask(t, ctx, c).Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseRunning))

	r.Now = func() time.Time { return t0.Add(defaultRestartSeidTimeout + time.Second) }
	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())

	got := getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseFailed))
	failed := findFailedCond(got)
	g.Expect(failed).NotTo(BeNil())
	g.Expect(failed.Reason).To(Equal("Timeout"))
}

// A target with a label peer source but empty status.resolvedPeers must fail
// fast (no resolved addresses) rather than submit a zero-source discover-peers
// that would silently wipe persistent-peers.
func TestReconcile_DiscoverPeers_LabelEmptyResolved_Fails(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	cr := newDiscoverPeersTask()
	node := newRunningNode()
	node.Spec.Peers = []seiv1alpha1.PeerSource{
		{Label: &seiv1alpha1.LabelPeerSource{Selector: map[string]string{testNDPSelectorKey: "g1"}}},
	}
	// status.resolvedPeers intentionally empty.
	fakeSC := newFakeSidecarClient()

	r, c := newReconcilerWithSidecar(t, time.Now(), fakeSC, cr, node)

	_, err := r.Reconcile(ctx, req()) // R1 synthesize
	g.Expect(err).NotTo(HaveOccurred())
	_, err = r.Reconcile(ctx, req()) // R2 driveTask → ParamsBuildFailed
	g.Expect(err).NotTo(HaveOccurred())

	fakeSC.mu.Lock()
	g.Expect(fakeSC.submitted).To(BeEmpty(), "must not submit a zero-source discover-peers")
	fakeSC.mu.Unlock()

	got := getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseFailed))
	failed := findFailedCond(got)
	g.Expect(failed).NotTo(BeNil())
	g.Expect(failed.Reason).To(Equal("ParamsBuildFailed"))
}

// An unwired kind fails fast at synthesis with reason=UnsupportedKind — the
// reason travels with the error (task.FailureReason), distinct from the
// ParamsBuildFailed reason used for wired-kind payload errors, with no
// conditional at the call site.
func TestReconcile_UnsupportedKind_FailsWithReason(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	cr := newUpdateImageTask()
	cr.Spec.Kind = seiv1alpha1.SeiNodeTaskKind("FutureUnwiredKind")
	cr.Spec.UpdateNodeImage = nil
	node := newRunningNode()

	r, c := newReconciler(t, time.Now(), cr, node)

	_, err := r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())

	got := getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseFailed))
	failed := findFailedCond(got)
	g.Expect(failed).NotTo(BeNil())
	g.Expect(failed.Reason).To(Equal("UnsupportedKind"))
}

// findFailedCond returns the Failed condition, the only condition the failure
// tests assert on. Kept arg-free to avoid an always-constant parameter.
func findFailedCond(cr *seiv1alpha1.SeiNodeTask) *metav1.Condition {
	for i := range cr.Status.Conditions {
		if cr.Status.Conditions[i].Type == seiv1alpha1.ConditionSeiNodeTaskFailed {
			return &cr.Status.Conditions[i]
		}
	}
	return nil
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

// The headline correctness property of restart-seid: the sidecar task FAILS LOUD
// (never SIGKILLs) when seid does not exit in the grace window, and that failure
// must surface as a Failed SeiNodeTask carrying the sidecar's error — not a
// silent success or an indefinite requeue. Mirrors TestReconcile_GovVote_SidecarFailure.
func TestReconcile_RestartSeid_SidecarFailLoud_Fails(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	t0 := time.Now()
	cr := newRestartSeidTask()
	node := newRunningNode()
	fakeSC := newFakeSidecarClient()

	r, c := newReconcilerWithSidecar(t, t0, fakeSC, cr, node)

	// R1: synthesize.
	_, err := r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	got := getTask(t, ctx, c)
	taskID, perr := uuid.Parse(got.Status.Task.ID)
	g.Expect(perr).NotTo(HaveOccurred())

	// Stage the fail-loud result BEFORE R2 so the first Status poll after Execute
	// sees Failed.
	const sidecarErr = "seid pid still alive after SIGTERM; leaving it running"
	fakeSC.setResult(taskID, sidecar.Failed, sidecarErr)

	// R2: Execute submits restart-seid; Status polls and sees Failed → CR Failed.
	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	got = getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseFailed))
	g.Expect(got.Status.Task.Status).To(Equal(seiv1alpha1.TaskFailed))
	g.Expect(got.Status.Task.Err).To(ContainSubstring(sidecarErr))

	failed := findFailedCond(got)
	g.Expect(failed).NotTo(BeNil())
	g.Expect(failed.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(failed.Reason).To(Equal("TaskFailed"))
	g.Expect(failed.Message).To(ContainSubstring(sidecarErr))
}
