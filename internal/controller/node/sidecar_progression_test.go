package node

import (
	"context"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-node-controller/api/v1alpha1"
)

const testUpgradeImageV2 = "sei:v2"

// ---------------------------------------------------------------------------
// Mock sidecar client
// ---------------------------------------------------------------------------

type mockSidecarClient struct {
	status    *StatusResponse
	statusErr error
	submitted []TaskRequest
	submitErr error
}

func (m *mockSidecarClient) Status(_ context.Context) (*StatusResponse, error) {
	return m.status, m.statusErr
}

func (m *mockSidecarClient) SubmitTask(_ context.Context, task TaskRequest) error {
	m.submitted = append(m.submitted, task)
	return m.submitErr
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func newProgressionReconciler(t *testing.T, mock *mockSidecarClient, objs ...client.Object) (*SeiNodeReconciler, client.Client) {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := seiv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&seiv1alpha1.SeiNode{}).
		Build()
	r := &SeiNodeReconciler{
		Client: c,
		Scheme: s,
		BuildSidecarClientFn: func(_ *seiv1alpha1.SeiNode) SidecarStatusClient {
			return mock
		},
	}
	return r, c
}

func snapshotNode() *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-node",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "atlantic-2",
			Image:   "sei:latest",
			Genesis: seiv1alpha1.GenesisConfiguration{ChainID: "atlantic-2"},
			Snapshot: &seiv1alpha1.SnapshotSource{
				Region: "us-east-1",
				Bucket: seiv1alpha1.BucketSnapshot{URI: "s3://my-bucket/snapshots/latest.tar"},
			},
			Sidecar: &seiv1alpha1.SidecarConfig{Image: "sidecar:latest", Port: 7777},
		},
	}
}

func peerSyncNode() *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-node",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "atlantic-2",
			Image:   "sei:latest",
			Genesis: seiv1alpha1.GenesisConfiguration{
				ChainID: "atlantic-2",
				S3: &seiv1alpha1.GenesisS3Source{
					URI:    "s3://sei-testnet-genesis-config/atlantic-2/genesis.json",
					Region: "us-east-2",
				},
			},
			Peers: &seiv1alpha1.PeerConfig{
				Sources: []seiv1alpha1.PeerSource{
					{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{
						Region: "eu-central-1",
						Tags:   map[string]string{"ChainIdentifier": "atlantic-2"},
					}},
				},
			},
			Sidecar: &seiv1alpha1.SidecarConfig{Image: "sidecar:latest", Port: 7777},
		},
	}
}

func genesisNode() *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-node",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "arctic-1",
			Image:   "sei:latest",
			Genesis: seiv1alpha1.GenesisConfiguration{
				ChainID: "arctic-1",
				Fresh:   true,
				PVC:     &seiv1alpha1.GenesisPVCSource{DataPVC: "data-pvc"},
			},
			Sidecar: &seiv1alpha1.SidecarConfig{Image: "sidecar:latest", Port: 7777},
		},
	}
}

// ---------------------------------------------------------------------------
// Tests: bootstrapMode
// ---------------------------------------------------------------------------

func TestBootstrapMode(t *testing.T) {
	tests := []struct {
		name string
		node *seiv1alpha1.SeiNode
		want string
	}{
		{"snapshot", snapshotNode(), "snapshot"},
		{"peer-sync", peerSyncNode(), "peer-sync"},
		{"genesis", genesisNode(), "genesis"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := bootstrapMode(tt.node); got != tt.want {
				t.Errorf("bootstrapMode() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Tests: per-mode task ordering
// ---------------------------------------------------------------------------

func TestSnapshotMode_TaskOrdering(t *testing.T) {
	mock := &mockSidecarClient{status: &StatusResponse{Phase: phaseInitialized}}
	r, _ := newProgressionReconciler(t, mock, snapshotNode())

	_, err := r.reconcileSidecarProgression(context.Background(), snapshotNode())
	if err != nil {
		t.Fatalf("reconcileSidecarProgression() error = %v", err)
	}
	if len(mock.submitted) != 1 {
		t.Fatalf("expected 1 submitted task, got %d", len(mock.submitted))
	}
	if mock.submitted[0].Type != taskSnapshotRestore {
		t.Errorf("first task = %q, want %q", mock.submitted[0].Type, taskSnapshotRestore)
	}
}

func TestPeerSyncMode_TaskOrdering(t *testing.T) {
	mock := &mockSidecarClient{status: &StatusResponse{Phase: phaseInitialized}}
	r, _ := newProgressionReconciler(t, mock, peerSyncNode())

	_, err := r.reconcileSidecarProgression(context.Background(), peerSyncNode())
	if err != nil {
		t.Fatalf("reconcileSidecarProgression() error = %v", err)
	}
	if len(mock.submitted) != 1 {
		t.Fatalf("expected 1 submitted task, got %d", len(mock.submitted))
	}
	if mock.submitted[0].Type != taskDiscoverPeers {
		t.Errorf("first task = %q, want %q", mock.submitted[0].Type, taskDiscoverPeers)
	}
}

func TestGenesisMode_TaskOrdering(t *testing.T) {
	mock := &mockSidecarClient{status: &StatusResponse{Phase: phaseInitialized}}
	r, _ := newProgressionReconciler(t, mock, genesisNode())

	_, err := r.reconcileSidecarProgression(context.Background(), genesisNode())
	if err != nil {
		t.Fatalf("reconcileSidecarProgression() error = %v", err)
	}
	if len(mock.submitted) != 1 {
		t.Fatalf("expected 1 submitted task, got %d", len(mock.submitted))
	}
	if mock.submitted[0].Type != taskConfigPatch {
		t.Errorf("first task = %q, want %q", mock.submitted[0].Type, taskConfigPatch)
	}
}

// ---------------------------------------------------------------------------
// Tests: taskProgressionForNode — dynamic peer injection
// ---------------------------------------------------------------------------

func TestTaskProgressionForNode_GenesisWithPeers_InsertsDiscoverPeers(t *testing.T) {
	node := genesisNode()
	node.Spec.Peers = &seiv1alpha1.PeerConfig{
		Sources: []seiv1alpha1.PeerSource{
			{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{
				Region: "eu-central-1",
				Tags:   map[string]string{"ChainIdentifier": "arctic-1"},
			}},
		},
	}

	got := taskProgressionForNode(node)
	want := []string{taskDiscoverPeers, taskConfigPatch, taskMarkReady}
	if len(got) != len(want) {
		t.Fatalf("progression = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("progression[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestTaskProgressionForNode_GenesisWithoutPeers_NoDiscoverPeers(t *testing.T) {
	node := genesisNode()

	got := taskProgressionForNode(node)
	want := []string{taskConfigPatch, taskMarkReady}
	if len(got) != len(want) {
		t.Fatalf("progression = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("progression[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestTaskProgressionForNode_SnapshotWithPeers_Unchanged(t *testing.T) {
	node := snapshotNode()
	node.Spec.Peers = &seiv1alpha1.PeerConfig{
		Sources: []seiv1alpha1.PeerSource{
			{Static: &seiv1alpha1.StaticPeerSource{Addresses: []string{"a@1.2.3.4:26656"}}},
		},
	}

	got := taskProgressionForNode(node)
	want := []string{taskSnapshotRestore, taskDiscoverPeers, taskConfigPatch, taskMarkReady}
	if len(got) != len(want) {
		t.Fatalf("progression = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("progression[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestGenesisMode_WithPeers_FirstTaskIsDiscoverPeers(t *testing.T) {
	node := genesisNode()
	node.Spec.Peers = &seiv1alpha1.PeerConfig{
		Sources: []seiv1alpha1.PeerSource{
			{Static: &seiv1alpha1.StaticPeerSource{Addresses: []string{"a@1.2.3.4:26656"}}},
		},
	}

	mock := &mockSidecarClient{status: &StatusResponse{Phase: phaseInitialized}}
	r, _ := newProgressionReconciler(t, mock, node)

	_, err := r.reconcileSidecarProgression(context.Background(), node)
	if err != nil {
		t.Fatalf("reconcileSidecarProgression() error = %v", err)
	}
	if len(mock.submitted) != 1 {
		t.Fatalf("expected 1 submitted task, got %d", len(mock.submitted))
	}
	if mock.submitted[0].Type != taskDiscoverPeers {
		t.Errorf("first task = %q, want %q", mock.submitted[0].Type, taskDiscoverPeers)
	}
}

// ---------------------------------------------------------------------------
// Tests: issueNextTask progression
// ---------------------------------------------------------------------------

func TestIssueNextTask_SnapshotProgression(t *testing.T) {
	expected := []string{taskDiscoverPeers, taskConfigPatch, taskMarkReady}
	completed := []string{taskSnapshotRestore, taskDiscoverPeers, taskConfigPatch}

	for i, completedTask := range completed {
		mock := &mockSidecarClient{}
		r, _ := newProgressionReconciler(t, mock, snapshotNode())

		_, err := r.issueNextTask(context.Background(), snapshotNode(), mock, completedTask)
		if err != nil {
			t.Fatalf("issueNextTask(%q) error = %v", completedTask, err)
		}
		if len(mock.submitted) != 1 {
			t.Fatalf("after %q: expected 1 submitted task, got %d", completedTask, len(mock.submitted))
		}
		if mock.submitted[0].Type != expected[i] {
			t.Errorf("after %q: next task = %q, want %q", completedTask, mock.submitted[0].Type, expected[i])
		}
	}
}

func TestIssueNextTask_LastTask_SwitchesToSteadyState(t *testing.T) {
	mock := &mockSidecarClient{}
	r, _ := newProgressionReconciler(t, mock, snapshotNode())

	result, err := r.issueNextTask(context.Background(), snapshotNode(), mock, taskMarkReady)
	if err != nil {
		t.Fatalf("issueNextTask() error = %v", err)
	}
	if len(mock.submitted) != 0 {
		t.Errorf("expected no tasks submitted after last task, got %d", len(mock.submitted))
	}
	if result.RequeueAfter != statusPollInterval {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, statusPollInterval)
	}
}

// ---------------------------------------------------------------------------
// Tests: phase regression handling (Initialized resets retry state)
// ---------------------------------------------------------------------------

func TestPhaseRegression_ResetsRetryState(t *testing.T) {
	node := snapshotNode()
	mock := &mockSidecarClient{
		status: &StatusResponse{Phase: phaseTaskComplete, LastTask: taskSnapshotRestore, LastTaskResult: "error"},
	}
	r, _ := newProgressionReconciler(t, mock, node)

	// Simulate a failure to increment retry count.
	_, _ = r.reconcileSidecarProgression(context.Background(), node)

	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}
	state := r.getRetryState(key, taskSnapshotRestore)
	if state.Count == 0 {
		t.Fatal("expected retry count > 0 after failure")
	}

	// Sidecar restarts → Initialized phase.
	mock.status = &StatusResponse{Phase: phaseInitialized}
	_, _ = r.reconcileSidecarProgression(context.Background(), node)

	// Retry state should be reset.
	state = r.getRetryState(key, taskSnapshotRestore)
	if state.Count != 0 {
		t.Errorf("retry count after phase regression = %d, want 0", state.Count)
	}
}

// ---------------------------------------------------------------------------
// Tests: status writing to SeiNodeStatus
// ---------------------------------------------------------------------------

func TestReconcileSidecarProgression_WritesStatusToNode(t *testing.T) {
	node := snapshotNode()
	mock := &mockSidecarClient{
		status: &StatusResponse{
			Phase:          phaseTaskRunning,
			CurrentTask:    taskSnapshotRestore,
			LastTask:       "",
			LastTaskResult: "",
		},
	}
	r, c := newProgressionReconciler(t, mock, node)

	_, err := r.reconcileSidecarProgression(context.Background(), node)
	if err != nil {
		t.Fatalf("reconcileSidecarProgression() error = %v", err)
	}

	updated := &seiv1alpha1.SeiNode{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, updated); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if updated.Status.SidecarPhase != phaseTaskRunning {
		t.Errorf("SidecarPhase = %q, want %q", updated.Status.SidecarPhase, phaseTaskRunning)
	}
	if updated.Status.SidecarCurrentTask != taskSnapshotRestore {
		t.Errorf("SidecarCurrentTask = %q, want %q", updated.Status.SidecarCurrentTask, taskSnapshotRestore)
	}
}

// ---------------------------------------------------------------------------
// Tests: bootstrap failure retry with backoff
// ---------------------------------------------------------------------------

func TestHandleTaskFailure_BootstrapRetryWithBackoff(t *testing.T) {
	node := snapshotNode()
	mock := &mockSidecarClient{
		status: &StatusResponse{Phase: phaseTaskComplete, LastTask: taskSnapshotRestore, LastTaskResult: "error"},
	}
	r, _ := newProgressionReconciler(t, mock, node)

	// First failure → 5s backoff.
	result, err := r.reconcileSidecarProgression(context.Background(), node)
	if err != nil {
		t.Fatalf("first failure: error = %v", err)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Errorf("first failure: RequeueAfter = %v, want 5s", result.RequeueAfter)
	}

	// Second failure → 10s backoff.
	result, err = r.reconcileSidecarProgression(context.Background(), node)
	if err != nil {
		t.Fatalf("second failure: error = %v", err)
	}
	if result.RequeueAfter != 10*time.Second {
		t.Errorf("second failure: RequeueAfter = %v, want 10s", result.RequeueAfter)
	}

	// Third failure → 20s backoff.
	result, err = r.reconcileSidecarProgression(context.Background(), node)
	if err != nil {
		t.Fatalf("third failure: error = %v", err)
	}
	if result.RequeueAfter != 20*time.Second {
		t.Errorf("third failure: RequeueAfter = %v, want 20s", result.RequeueAfter)
	}
}

// ---------------------------------------------------------------------------
// Tests: max retries sets Degraded condition
// ---------------------------------------------------------------------------

func TestHandleTaskFailure_MaxRetries_SetsDegradedCondition(t *testing.T) {
	node := snapshotNode()
	mock := &mockSidecarClient{
		status: &StatusResponse{Phase: phaseTaskComplete, LastTask: taskSnapshotRestore, LastTaskResult: "error"},
	}
	r, c := newProgressionReconciler(t, mock, node)

	// Exhaust retries (default 3).
	for range 3 {
		_, _ = r.reconcileSidecarProgression(context.Background(), node)
	}

	// Fourth call should set Degraded and stop retrying.
	result, err := r.reconcileSidecarProgression(context.Background(), node)
	if err != nil {
		t.Fatalf("max retries: error = %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("max retries: RequeueAfter = %v, want 0 (no requeue)", result.RequeueAfter)
	}

	updated := &seiv1alpha1.SeiNode{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, updated); err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	found := false
	for _, cond := range updated.Status.Conditions {
		if cond.Type == ConditionTypeDegraded && cond.Status == metav1.ConditionTrue {
			found = true
			if cond.Reason != reasonBootstrapTaskFailed {
				t.Errorf("Degraded reason = %q, want %q", cond.Reason, reasonBootstrapTaskFailed)
			}
		}
	}
	if !found {
		t.Error("expected Degraded=True condition after max retries")
	}
}

// ---------------------------------------------------------------------------
// Tests: runtime failure requeues at 30s
// ---------------------------------------------------------------------------

func TestHandleTaskFailure_RuntimeFailure_RequeuesAt30s(t *testing.T) {
	node := snapshotNode()

	mock := &mockSidecarClient{
		status: &StatusResponse{Phase: phaseTaskComplete, LastTask: "update-peers", LastTaskResult: "error"},
	}
	r, _ := newProgressionReconciler(t, mock, node)

	result, err := r.reconcileSidecarProgression(context.Background(), node)
	if err != nil {
		t.Fatalf("runtime failure: error = %v", err)
	}
	if result.RequeueAfter != statusPollInterval {
		t.Errorf("runtime failure: RequeueAfter = %v, want %v", result.RequeueAfter, statusPollInterval)
	}
}

// ---------------------------------------------------------------------------
// Tests: retry counter reset on sidecar restart
// ---------------------------------------------------------------------------

func TestRetryCounterReset_OnSidecarRestart(t *testing.T) {
	node := snapshotNode()
	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}

	mock := &mockSidecarClient{
		status: &StatusResponse{Phase: phaseTaskComplete, LastTask: taskSnapshotRestore, LastTaskResult: "error"},
	}
	r, _ := newProgressionReconciler(t, mock, node)

	// Accumulate retries.
	_, _ = r.reconcileSidecarProgression(context.Background(), node)
	_, _ = r.reconcileSidecarProgression(context.Background(), node)

	state := r.getRetryState(key, taskSnapshotRestore)
	if state.Count != 2 {
		t.Fatalf("expected 2 retries, got %d", state.Count)
	}

	// Sidecar restarts.
	mock.status = &StatusResponse{Phase: phaseInitialized}
	_, _ = r.reconcileSidecarProgression(context.Background(), node)

	// All retry counters for this node should be cleared.
	state = r.getRetryState(key, taskSnapshotRestore)
	if state.Count != 0 {
		t.Errorf("retry count after restart = %d, want 0", state.Count)
	}
}

// ---------------------------------------------------------------------------
// Tests: sidecar status error → requeue without error
// ---------------------------------------------------------------------------

func TestReconcileSidecarProgression_StatusError_Requeues(t *testing.T) {
	node := snapshotNode()
	mock := &mockSidecarClient{
		statusErr: fmt.Errorf("connection refused"),
	}
	r, _ := newProgressionReconciler(t, mock, node)

	result, err := r.reconcileSidecarProgression(context.Background(), node)
	if err != nil {
		t.Fatalf("expected no error on status failure, got %v", err)
	}
	if result.RequeueAfter != bootstrapPollInterval {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, bootstrapPollInterval)
	}
}

// ---------------------------------------------------------------------------
// Tests: paramsForTask
// ---------------------------------------------------------------------------

func TestParamsForTask_SnapshotRestore(t *testing.T) {
	node := snapshotNode()
	params := paramsForTask(node, taskSnapshotRestore)
	if params["bucket"] != "my-bucket" {
		t.Errorf("bucket = %v, want %q", params["bucket"], "my-bucket")
	}
	if params["prefix"] != "snapshots/latest.tar" {
		t.Errorf("prefix = %v, want %q", params["prefix"], "snapshots/latest.tar")
	}
	if params["region"] != "us-east-1" {
		t.Errorf("region = %v, want %q", params["region"], "us-east-1")
	}
}

func TestParamsForTask_DiscoverPeers_NoPeerConfig(t *testing.T) {
	node := snapshotNode()
	params := paramsForTask(node, taskDiscoverPeers)
	if params != nil {
		t.Errorf("expected nil params when no PeerConfig, got %v", params)
	}
}

func TestParamsForTask_DiscoverPeers_EC2Tags(t *testing.T) {
	node := snapshotNode()
	node.Spec.Peers = &seiv1alpha1.PeerConfig{
		Sources: []seiv1alpha1.PeerSource{
			{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{
				Region: "eu-central-1",
				Tags:   map[string]string{"ChainIdentifier": "atlantic-2", "Component": "snapshotter"},
			}},
		},
	}
	params := paramsForTask(node, taskDiscoverPeers)

	sources, ok := params["sources"].([]map[string]any)
	if !ok {
		t.Fatalf("sources is not []map[string]any: %T", params["sources"])
	}
	if len(sources) != 1 {
		t.Fatalf("expected 1 source, got %d", len(sources))
	}
	if sources[0]["type"] != "ec2Tags" {
		t.Errorf("type = %v, want ec2Tags", sources[0]["type"])
	}
	if sources[0]["region"] != "eu-central-1" {
		t.Errorf("region = %v, want eu-central-1", sources[0]["region"])
	}
	tags, ok := sources[0]["tags"].(map[string]string)
	if !ok {
		t.Fatalf("tags is not map[string]string: %T", sources[0]["tags"])
	}
	if tags["ChainIdentifier"] != "atlantic-2" {
		t.Errorf("ChainIdentifier = %v, want atlantic-2", tags["ChainIdentifier"])
	}
}

func TestParamsForTask_DiscoverPeers_Static(t *testing.T) {
	node := peerSyncNode()
	node.Spec.Peers = &seiv1alpha1.PeerConfig{
		Sources: []seiv1alpha1.PeerSource{
			{Static: &seiv1alpha1.StaticPeerSource{
				Addresses: []string{"abc@1.2.3.4:26656"},
			}},
		},
	}
	params := paramsForTask(node, taskDiscoverPeers)

	sources, ok := params["sources"].([]map[string]any)
	if !ok {
		t.Fatalf("sources is not []map[string]any: %T", params["sources"])
	}
	if len(sources) != 1 {
		t.Fatalf("expected 1 source, got %d", len(sources))
	}
	if sources[0]["type"] != "static" {
		t.Errorf("type = %v, want static", sources[0]["type"])
	}
	addrs, ok := sources[0]["addresses"].([]string)
	if !ok {
		t.Fatalf("addresses is not []string: %T", sources[0]["addresses"])
	}
	if len(addrs) != 1 || addrs[0] != "abc@1.2.3.4:26656" {
		t.Errorf("addresses = %v, want [abc@1.2.3.4:26656]", addrs)
	}
}

func TestParamsForTask_DiscoverPeers_MultipleSources(t *testing.T) {
	node := snapshotNode()
	node.Spec.Peers = &seiv1alpha1.PeerConfig{
		Sources: []seiv1alpha1.PeerSource{
			{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{
				Region: "eu-central-1",
				Tags:   map[string]string{"ChainIdentifier": "atlantic-2"},
			}},
			{Static: &seiv1alpha1.StaticPeerSource{
				Addresses: []string{"xyz@5.6.7.8:26656"},
			}},
		},
	}
	params := paramsForTask(node, taskDiscoverPeers)

	sources, ok := params["sources"].([]map[string]any)
	if !ok {
		t.Fatalf("sources is not []map[string]any: %T", params["sources"])
	}
	if len(sources) != 2 {
		t.Fatalf("expected 2 sources, got %d", len(sources))
	}
	if sources[0]["type"] != "ec2Tags" {
		t.Errorf("sources[0].type = %v, want ec2Tags", sources[0]["type"])
	}
	if sources[1]["type"] != "static" {
		t.Errorf("sources[1].type = %v, want static", sources[1]["type"])
	}
}

func TestParamsForTask_MarkReady(t *testing.T) {
	node := snapshotNode()
	params := paramsForTask(node, taskMarkReady)
	if params != nil {
		t.Errorf("mark-ready params = %v, want nil", params)
	}
}

// ---------------------------------------------------------------------------
// Tests: TaskRunning phase → requeue at 5s
// ---------------------------------------------------------------------------

func TestReconcileSidecarProgression_TaskRunning_Requeues(t *testing.T) {
	node := snapshotNode()
	mock := &mockSidecarClient{
		status: &StatusResponse{Phase: phaseTaskRunning, CurrentTask: taskSnapshotRestore},
	}
	r, _ := newProgressionReconciler(t, mock, node)

	result, err := r.reconcileSidecarProgression(context.Background(), node)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.RequeueAfter != bootstrapPollInterval {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, bootstrapPollInterval)
	}
	if len(mock.submitted) != 0 {
		t.Errorf("expected no tasks submitted during TaskRunning, got %d", len(mock.submitted))
	}
}

// ---------------------------------------------------------------------------
// Tests: nextPendingUpgrade — lowest-height-first ordering
// ---------------------------------------------------------------------------

func TestNextPendingUpgrade_ReturnsLowestHeight(t *testing.T) {
	node := snapshotNode()
	node.Spec.ScheduledUpgrades = []seiv1alpha1.ScheduledUpgrade{
		{Height: 300, Image: "sei:v3"},
		{Height: 100, Image: "sei:v1"},
		{Height: 200, Image: testUpgradeImageV2},
	}

	got := nextPendingUpgrade(node)
	if got == nil {
		t.Fatal("expected a pending upgrade, got nil")
	}
	if got.Height != 100 {
		t.Errorf("Height = %d, want 100", got.Height)
	}
	if got.Image != "sei:v1" {
		t.Errorf("Image = %q, want %q", got.Image, "sei:v1")
	}
}

func TestNextPendingUpgrade_SkipsAlreadySubmitted(t *testing.T) {
	node := snapshotNode()
	node.Spec.ScheduledUpgrades = []seiv1alpha1.ScheduledUpgrade{
		{Height: 100, Image: "sei:v1"},
		{Height: 200, Image: testUpgradeImageV2},
		{Height: 300, Image: "sei:v3"},
	}
	node.Status.SubmittedUpgradeHeights = []int64{100, 200}

	got := nextPendingUpgrade(node)
	if got == nil {
		t.Fatal("expected a pending upgrade, got nil")
	}
	if got.Height != 300 {
		t.Errorf("Height = %d, want 300", got.Height)
	}
}

func TestNextPendingUpgrade_AllSubmitted_ReturnsNil(t *testing.T) {
	node := snapshotNode()
	node.Spec.ScheduledUpgrades = []seiv1alpha1.ScheduledUpgrade{
		{Height: 100, Image: "sei:v1"},
	}
	node.Status.SubmittedUpgradeHeights = []int64{100}

	got := nextPendingUpgrade(node)
	if got != nil {
		t.Errorf("expected nil, got upgrade at height %d", got.Height)
	}
}

func TestNextPendingUpgrade_NoUpgrades_ReturnsNil(t *testing.T) {
	node := snapshotNode()
	got := nextPendingUpgrade(node)
	if got != nil {
		t.Errorf("expected nil, got upgrade at height %d", got.Height)
	}
}

// ---------------------------------------------------------------------------
// Tests: reconcileRuntimeTasks — schedule-upgrade submission
// ---------------------------------------------------------------------------

func TestReconcileRuntimeTasks_SubmitsScheduleUpgrade(t *testing.T) {
	node := snapshotNode()
	node.Spec.ScheduledUpgrades = []seiv1alpha1.ScheduledUpgrade{
		{Height: 500000, Image: testUpgradeImageV2},
	}

	mock := &mockSidecarClient{
		status: &StatusResponse{Phase: phaseReady},
	}
	r, c := newProgressionReconciler(t, mock, node)

	result, err := r.reconcileRuntimeTasks(context.Background(), node, mock)
	if err != nil {
		t.Fatalf("reconcileRuntimeTasks() error = %v", err)
	}
	if result.RequeueAfter != statusPollInterval {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, statusPollInterval)
	}
	if len(mock.submitted) != 1 {
		t.Fatalf("expected 1 submitted task, got %d", len(mock.submitted))
	}
	if mock.submitted[0].Type != taskScheduleUpgrade {
		t.Errorf("task type = %q, want %q", mock.submitted[0].Type, taskScheduleUpgrade)
	}
	if mock.submitted[0].Params["height"] != int64(500000) {
		t.Errorf("height = %v, want 500000", mock.submitted[0].Params["height"])
	}
	if mock.submitted[0].Params["image"] != testUpgradeImageV2 {
		t.Errorf("image = %v, want %q", mock.submitted[0].Params["image"], testUpgradeImageV2)
	}

	// Verify the height was tracked in status.
	updated := &seiv1alpha1.SeiNode{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, updated); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if len(updated.Status.SubmittedUpgradeHeights) != 1 || updated.Status.SubmittedUpgradeHeights[0] != 500000 {
		t.Errorf("SubmittedUpgradeHeights = %v, want [500000]", updated.Status.SubmittedUpgradeHeights)
	}
}

func TestReconcileRuntimeTasks_DuplicatePrevention(t *testing.T) {
	node := snapshotNode()
	node.Spec.ScheduledUpgrades = []seiv1alpha1.ScheduledUpgrade{
		{Height: 500000, Image: testUpgradeImageV2},
	}
	node.Status.SubmittedUpgradeHeights = []int64{500000}

	mock := &mockSidecarClient{
		status: &StatusResponse{Phase: phaseReady},
	}
	r, _ := newProgressionReconciler(t, mock, node)

	result, err := r.reconcileRuntimeTasks(context.Background(), node, mock)
	if err != nil {
		t.Fatalf("reconcileRuntimeTasks() error = %v", err)
	}
	if result.RequeueAfter != statusPollInterval {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, statusPollInterval)
	}
	if len(mock.submitted) != 0 {
		t.Errorf("expected no tasks submitted (duplicate), got %d", len(mock.submitted))
	}
}

func TestReconcileRuntimeTasks_NoUpgrades_RequeuesAtSteadyState(t *testing.T) {
	node := snapshotNode()
	mock := &mockSidecarClient{
		status: &StatusResponse{Phase: phaseReady},
	}
	r, _ := newProgressionReconciler(t, mock, node)

	result, err := r.reconcileRuntimeTasks(context.Background(), node, mock)
	if err != nil {
		t.Fatalf("reconcileRuntimeTasks() error = %v", err)
	}
	if result.RequeueAfter != statusPollInterval {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, statusPollInterval)
	}
	if len(mock.submitted) != 0 {
		t.Errorf("expected no tasks submitted, got %d", len(mock.submitted))
	}
}

func TestReconcileRuntimeTasks_SubmitError_RequeuesGracefully(t *testing.T) {
	node := snapshotNode()
	node.Spec.ScheduledUpgrades = []seiv1alpha1.ScheduledUpgrade{
		{Height: 500000, Image: testUpgradeImageV2},
	}

	mock := &mockSidecarClient{
		status:    &StatusResponse{Phase: phaseReady},
		submitErr: fmt.Errorf("connection refused"),
	}
	r, _ := newProgressionReconciler(t, mock, node)

	result, err := r.reconcileRuntimeTasks(context.Background(), node, mock)
	if err != nil {
		t.Fatalf("expected no error on submit failure, got %v", err)
	}
	if result.RequeueAfter != statusPollInterval {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, statusPollInterval)
	}
}

// ---------------------------------------------------------------------------
// Tests: handleUpgradeHalt — StatefulSet image patch and prune
// ---------------------------------------------------------------------------

func testStatefulSet(node *seiv1alpha1.SeiNode) *appsv1.StatefulSet {
	one := int32(1)
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.Name,
			Namespace: node.Namespace,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &one,
			ServiceName: node.Name,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": node.Name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": node.Name}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "seid", Image: node.Spec.Image},
					},
				},
			},
		},
	}
}

func TestHandleUpgradeHalt_PatchesImageAndPrunes(t *testing.T) {
	node := snapshotNode()
	node.Spec.ScheduledUpgrades = []seiv1alpha1.ScheduledUpgrade{
		{Height: 196000000, Image: testUpgradeImageV2},
	}
	node.Status.SubmittedUpgradeHeights = []int64{196000000}

	sts := testStatefulSet(node)
	mock := &mockSidecarClient{
		status: &StatusResponse{Phase: phaseUpgradeHalted},
	}
	r, c := newProgressionReconciler(t, mock, node, sts)

	status := &StatusResponse{
		Phase:         phaseUpgradeHalted,
		UpgradeHeight: 196000000,
		UpgradeImage:  testUpgradeImageV2,
	}

	result, err := r.handleUpgradeHalt(context.Background(), node, status)
	if err != nil {
		t.Fatalf("handleUpgradeHalt() error = %v", err)
	}
	// Successful halt returns zero result (pod will restart).
	if result.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0", result.RequeueAfter)
	}

	// Verify StatefulSet image was patched.
	updatedSts := &appsv1.StatefulSet{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, updatedSts); err != nil {
		t.Fatalf("Get StatefulSet error = %v", err)
	}
	seidImage := updatedSts.Spec.Template.Spec.Containers[0].Image
	if seidImage != testUpgradeImageV2 {
		t.Errorf("seid container image = %q, want %q", seidImage, testUpgradeImageV2)
	}

	// Verify the upgrade was pruned from spec.scheduledUpgrades.
	updatedNode := &seiv1alpha1.SeiNode{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, updatedNode); err != nil {
		t.Fatalf("Get SeiNode error = %v", err)
	}
	if len(updatedNode.Spec.ScheduledUpgrades) != 0 {
		t.Errorf("ScheduledUpgrades = %v, want empty", updatedNode.Spec.ScheduledUpgrades)
	}

	// Verify the height was pruned from status.submittedUpgradeHeights.
	if len(updatedNode.Status.SubmittedUpgradeHeights) != 0 {
		t.Errorf("SubmittedUpgradeHeights = %v, want empty", updatedNode.Status.SubmittedUpgradeHeights)
	}
}

func TestHandleUpgradeHalt_EmptyImage_Requeues(t *testing.T) {
	node := snapshotNode()
	mock := &mockSidecarClient{
		status: &StatusResponse{Phase: phaseUpgradeHalted},
	}
	r, _ := newProgressionReconciler(t, mock, node)

	status := &StatusResponse{
		Phase:        phaseUpgradeHalted,
		UpgradeImage: "",
	}

	result, err := r.handleUpgradeHalt(context.Background(), node, status)
	if err != nil {
		t.Fatalf("handleUpgradeHalt() error = %v", err)
	}
	if result.RequeueAfter != bootstrapPollInterval {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, bootstrapPollInterval)
	}
}

// ---------------------------------------------------------------------------
// Tests: full upgrade lifecycle via reconcileSidecarProgression
// ---------------------------------------------------------------------------

func TestUpgradeLifecycle_ScheduleHaltPatchPrune(t *testing.T) {
	node := snapshotNode()
	node.Spec.ScheduledUpgrades = []seiv1alpha1.ScheduledUpgrade{
		{Height: 196000000, Image: testUpgradeImageV2},
	}
	sts := testStatefulSet(node)

	// Phase 1: sidecar is Ready, controller submits schedule-upgrade.
	mock := &mockSidecarClient{
		status: &StatusResponse{Phase: phaseReady},
	}
	r, c := newProgressionReconciler(t, mock, node, sts)

	_, err := r.reconcileSidecarProgression(context.Background(), node)
	if err != nil {
		t.Fatalf("phase 1: error = %v", err)
	}
	if len(mock.submitted) != 1 || mock.submitted[0].Type != taskScheduleUpgrade {
		t.Fatalf("phase 1: expected schedule-upgrade submission, got %v", mock.submitted)
	}

	// Re-fetch node to get updated status (submittedUpgradeHeights).
	if err := c.Get(context.Background(), types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, node); err != nil {
		t.Fatalf("re-fetch node: %v", err)
	}

	// Phase 2: sidecar transitions to UpgradeHalted.
	mock.status = &StatusResponse{
		Phase:         phaseUpgradeHalted,
		UpgradeHeight: 196000000,
		UpgradeImage:  testUpgradeImageV2,
	}
	mock.submitted = nil

	_, err = r.reconcileSidecarProgression(context.Background(), node)
	if err != nil {
		t.Fatalf("phase 2: error = %v", err)
	}

	// Verify StatefulSet image was patched.
	updatedSts := &appsv1.StatefulSet{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, updatedSts); err != nil {
		t.Fatalf("Get StatefulSet: %v", err)
	}
	if updatedSts.Spec.Template.Spec.Containers[0].Image != testUpgradeImageV2 {
		t.Errorf("seid image = %q, want %q", updatedSts.Spec.Template.Spec.Containers[0].Image, testUpgradeImageV2)
	}

	// Verify upgrade was pruned from spec and status.
	updatedNode := &seiv1alpha1.SeiNode{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, updatedNode); err != nil {
		t.Fatalf("Get SeiNode: %v", err)
	}
	if len(updatedNode.Spec.ScheduledUpgrades) != 0 {
		t.Errorf("ScheduledUpgrades not pruned: %v", updatedNode.Spec.ScheduledUpgrades)
	}
	if len(updatedNode.Status.SubmittedUpgradeHeights) != 0 {
		t.Errorf("SubmittedUpgradeHeights not pruned: %v", updatedNode.Status.SubmittedUpgradeHeights)
	}
}

// ---------------------------------------------------------------------------
// Tests: lowest-height-first upgrade ordering via reconcileRuntimeTasks
// ---------------------------------------------------------------------------

func TestReconcileRuntimeTasks_LowestHeightFirst(t *testing.T) {
	node := snapshotNode()
	node.Spec.ScheduledUpgrades = []seiv1alpha1.ScheduledUpgrade{
		{Height: 300, Image: "sei:v3"},
		{Height: 100, Image: "sei:v1"},
		{Height: 200, Image: testUpgradeImageV2},
	}

	mock := &mockSidecarClient{
		status: &StatusResponse{Phase: phaseReady},
	}
	r, _ := newProgressionReconciler(t, mock, node)

	_, err := r.reconcileRuntimeTasks(context.Background(), node, mock)
	if err != nil {
		t.Fatalf("reconcileRuntimeTasks() error = %v", err)
	}
	if len(mock.submitted) != 1 {
		t.Fatalf("expected 1 submitted task, got %d", len(mock.submitted))
	}
	if mock.submitted[0].Params["height"] != int64(100) {
		t.Errorf("submitted height = %v, want 100 (lowest first)", mock.submitted[0].Params["height"])
	}
}

// ---------------------------------------------------------------------------
// Tests: configureGenesisParams
// ---------------------------------------------------------------------------

func TestConfigureGenesisParams_WithS3(t *testing.T) {
	node := peerSyncNode()
	params := configureGenesisParams(node)
	if params == nil {
		t.Fatal("expected non-nil params when Genesis.S3 is set")
	}
	if params["uri"] != "s3://sei-testnet-genesis-config/atlantic-2/genesis.json" {
		t.Errorf("uri = %v, want %q", params["uri"], "s3://sei-testnet-genesis-config/atlantic-2/genesis.json")
	}
	if params["region"] != "us-east-2" {
		t.Errorf("region = %v, want %q", params["region"], "us-east-2")
	}
}

func TestConfigureGenesisParams_WithoutS3(t *testing.T) {
	node := snapshotNode()
	params := configureGenesisParams(node)
	if params != nil {
		t.Errorf("expected nil params when Genesis.S3 is nil, got %v", params)
	}
}

// ---------------------------------------------------------------------------
// Tests: taskProgressionForNode — configure-genesis injection
// ---------------------------------------------------------------------------

func TestTaskProgressionForNode_PeerSyncWithS3_InsertsConfigureGenesis(t *testing.T) {
	node := peerSyncNode()

	got := taskProgressionForNode(node)
	want := []string{taskDiscoverPeers, taskConfigureGenesis, taskConfigureStateSync, taskConfigPatch, taskMarkReady}
	if len(got) != len(want) {
		t.Fatalf("progression = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("progression[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestTaskProgressionForNode_PeerSyncWithStateSync_InsertsConfigureStateSync(t *testing.T) {
	node := peerSyncNode()
	node.Spec.Genesis.S3 = nil // no S3, but still not fresh and no snapshot

	got := taskProgressionForNode(node)
	want := []string{taskDiscoverPeers, taskConfigureStateSync, taskConfigPatch, taskMarkReady}
	if len(got) != len(want) {
		t.Fatalf("progression = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("progression[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// Tests: needsStateSync
// ---------------------------------------------------------------------------

func TestNeedsStateSync_NotFreshNoSnapshot(t *testing.T) {
	node := peerSyncNode()
	if !needsStateSync(node) {
		t.Error("needsStateSync() = false, want true (not fresh, no snapshot)")
	}
}

func TestNeedsStateSync_FreshNoSnapshot(t *testing.T) {
	node := genesisNode()
	if needsStateSync(node) {
		t.Error("needsStateSync() = true, want false (fresh, no snapshot)")
	}
}

func TestNeedsStateSync_NotFreshWithSnapshot(t *testing.T) {
	node := snapshotNode()
	if needsStateSync(node) {
		t.Error("needsStateSync() = true, want false (not fresh, has snapshot)")
	}
}

func TestNeedsStateSync_FreshWithSnapshot(t *testing.T) {
	node := genesisNode()
	node.Spec.Snapshot = &seiv1alpha1.SnapshotSource{
		Region: "us-east-1",
		Bucket: seiv1alpha1.BucketSnapshot{URI: "s3://bucket/snap.tar"},
	}
	if needsStateSync(node) {
		t.Error("needsStateSync() = true, want false (fresh, has snapshot)")
	}
}

// ---------------------------------------------------------------------------
// Tests: configPatchParams returns nil
// ---------------------------------------------------------------------------

func TestConfigPatchParams_ReturnsNil(t *testing.T) {
	for _, node := range []*seiv1alpha1.SeiNode{snapshotNode(), peerSyncNode(), genesisNode()} {
		params := configPatchParams(node)
		if params != nil {
			t.Errorf("configPatchParams() = %v, want nil", params)
		}
	}
}

func TestConfigPatchParams_WithSnapshotGeneration(t *testing.T) {
	node := snapshotNode()
	node.Spec.SnapshotGeneration = &seiv1alpha1.SnapshotGenerationConfig{
		Interval:   2000,
		KeepRecent: 10,
	}

	params := configPatchParams(node)
	if params == nil {
		t.Fatal("expected non-nil params with SnapshotGeneration configured")
	}

	sg, ok := params["snapshotGeneration"].(map[string]any)
	if !ok {
		t.Fatalf("snapshotGeneration is not map[string]any: %T", params["snapshotGeneration"])
	}
	if sg["interval"] != int64(2000) {
		t.Errorf("interval = %v, want 2000", sg["interval"])
	}
	if sg["keepRecent"] != int32(10) {
		t.Errorf("keepRecent = %v, want 10", sg["keepRecent"])
	}
}

func TestConfigPatchParams_SnapshotGenerationDefaultKeepRecent(t *testing.T) {
	node := snapshotNode()
	node.Spec.SnapshotGeneration = &seiv1alpha1.SnapshotGenerationConfig{
		Interval: 1000,
	}

	params := configPatchParams(node)
	if params == nil {
		t.Fatal("expected non-nil params")
	}

	sg := params["snapshotGeneration"].(map[string]any)
	if sg["keepRecent"] != int32(5) {
		t.Errorf("keepRecent = %v, want 5 (default)", sg["keepRecent"])
	}
}

// ---------------------------------------------------------------------------
// Tests: snapshotUploadParams
// ---------------------------------------------------------------------------

func TestSnapshotUploadParams_NoSnapshotGeneration(t *testing.T) {
	node := snapshotNode()
	if params := snapshotUploadParams(node); params != nil {
		t.Errorf("expected nil, got %v", params)
	}
}

func TestSnapshotUploadParams_NoDestination(t *testing.T) {
	node := snapshotNode()
	node.Spec.SnapshotGeneration = &seiv1alpha1.SnapshotGenerationConfig{
		Interval: 2000,
	}
	if params := snapshotUploadParams(node); params != nil {
		t.Errorf("expected nil, got %v", params)
	}
}

func TestSnapshotUploadParams_WithS3Destination(t *testing.T) {
	node := snapshotNode()
	node.Spec.SnapshotGeneration = &seiv1alpha1.SnapshotGenerationConfig{
		Interval: 2000,
		Destination: &seiv1alpha1.SnapshotDestination{
			S3: &seiv1alpha1.S3SnapshotDestination{
				Bucket: "atlantic-2-snapshots",
				Prefix: "state-sync",
				Region: "eu-central-1",
			},
		},
	}

	params := snapshotUploadParams(node)
	if params == nil {
		t.Fatal("expected non-nil params")
	}
	if params["bucket"] != "atlantic-2-snapshots" {
		t.Errorf("bucket = %v, want %q", params["bucket"], "atlantic-2-snapshots")
	}
	if params["prefix"] != "state-sync" {
		t.Errorf("prefix = %v, want %q", params["prefix"], "state-sync")
	}
	if params["region"] != "eu-central-1" {
		t.Errorf("region = %v, want %q", params["region"], "eu-central-1")
	}
}

// ---------------------------------------------------------------------------
// Tests: reconcileRuntimeTasks submits snapshot-upload
// ---------------------------------------------------------------------------

func snapshotterNode() *seiv1alpha1.SeiNode {
	node := snapshotNode()
	node.Spec.SnapshotGeneration = &seiv1alpha1.SnapshotGenerationConfig{
		Interval:   2000,
		KeepRecent: 5,
		Destination: &seiv1alpha1.SnapshotDestination{
			S3: &seiv1alpha1.S3SnapshotDestination{
				Bucket: "atlantic-2-snapshots",
				Prefix: "state-sync",
				Region: "eu-central-1",
			},
		},
	}
	return node
}

func TestReconcileRuntimeTasks_SubmitsSnapshotUpload(t *testing.T) {
	node := snapshotterNode()
	mock := &mockSidecarClient{
		status: &StatusResponse{Phase: phaseReady},
	}
	r, _ := newProgressionReconciler(t, mock, node)

	result, err := r.reconcileRuntimeTasks(context.Background(), node, mock)
	if err != nil {
		t.Fatalf("reconcileRuntimeTasks() error = %v", err)
	}
	if result.RequeueAfter != statusPollInterval {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, statusPollInterval)
	}
	if len(mock.submitted) != 1 {
		t.Fatalf("expected 1 submitted task, got %d", len(mock.submitted))
	}
	if mock.submitted[0].Type != taskSnapshotUpload {
		t.Errorf("task type = %q, want %q", mock.submitted[0].Type, taskSnapshotUpload)
	}
	if mock.submitted[0].Params["bucket"] != "atlantic-2-snapshots" {
		t.Errorf("bucket = %v, want %q", mock.submitted[0].Params["bucket"], "atlantic-2-snapshots")
	}
}

func TestReconcileRuntimeTasks_UpgradesTakePriorityOverUpload(t *testing.T) {
	node := snapshotterNode()
	node.Spec.ScheduledUpgrades = []seiv1alpha1.ScheduledUpgrade{
		{Height: 500000, Image: testUpgradeImageV2},
	}
	mock := &mockSidecarClient{
		status: &StatusResponse{Phase: phaseReady},
	}
	r, _ := newProgressionReconciler(t, mock, node)

	_, err := r.reconcileRuntimeTasks(context.Background(), node, mock)
	if err != nil {
		t.Fatalf("reconcileRuntimeTasks() error = %v", err)
	}
	if len(mock.submitted) != 1 {
		t.Fatalf("expected 1 submitted task, got %d", len(mock.submitted))
	}
	if mock.submitted[0].Type != taskScheduleUpgrade {
		t.Errorf("task type = %q, want %q (upgrades should take priority)", mock.submitted[0].Type, taskScheduleUpgrade)
	}
}

func TestReconcileRuntimeTasks_NoDestination_NoUploadSubmitted(t *testing.T) {
	node := snapshotNode()
	node.Spec.SnapshotGeneration = &seiv1alpha1.SnapshotGenerationConfig{
		Interval: 2000,
	}
	mock := &mockSidecarClient{
		status: &StatusResponse{Phase: phaseReady},
	}
	r, _ := newProgressionReconciler(t, mock, node)

	_, err := r.reconcileRuntimeTasks(context.Background(), node, mock)
	if err != nil {
		t.Fatalf("reconcileRuntimeTasks() error = %v", err)
	}
	if len(mock.submitted) != 0 {
		t.Errorf("expected no tasks submitted, got %d", len(mock.submitted))
	}
}
