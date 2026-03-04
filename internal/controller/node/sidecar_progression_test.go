package node

import (
	"context"
	"fmt"
	"testing"
	"time"

	sidecar "github.com/sei-protocol/sei-sidecar-client-go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func strPtr(s string) *string { return &s }

type mockSidecarClient struct {
	status    *sidecar.StatusResponse
	statusErr error
	submitted []sidecar.TaskBuilder
	submitErr error
}

func (m *mockSidecarClient) Status(_ context.Context) (*sidecar.StatusResponse, error) {
	return m.status, m.statusErr
}

func (m *mockSidecarClient) SubmitTask(_ context.Context, task sidecar.TaskBuilder) error {
	m.submitted = append(m.submitted, task)
	return m.submitErr
}

func newProgressionReconciler(t *testing.T, mock *mockSidecarClient, objs ...client.Object) (*SeiNodeReconciler, client.Client) {
	t.Helper()
	s := k8sruntime.NewScheme()
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

func TestSnapshotMode_TaskOrdering(t *testing.T) {
	mock := &mockSidecarClient{status: &sidecar.StatusResponse{Status: sidecar.Initializing}}
	r, _ := newProgressionReconciler(t, mock, snapshotNode())

	_, err := r.reconcileSidecarProgression(context.Background(), snapshotNode())
	if err != nil {
		t.Fatalf("reconcileSidecarProgression() error = %v", err)
	}
	if len(mock.submitted) != 1 {
		t.Fatalf("expected 1 submitted task, got %d", len(mock.submitted))
	}
	if mock.submitted[0].TaskType() != taskSnapshotRestore {
		t.Errorf("first task = %q, want %q", mock.submitted[0].TaskType(), taskSnapshotRestore)
	}
}

func TestPeerSyncMode_TaskOrdering(t *testing.T) {
	mock := &mockSidecarClient{status: &sidecar.StatusResponse{Status: sidecar.Initializing}}
	r, _ := newProgressionReconciler(t, mock, peerSyncNode())

	_, err := r.reconcileSidecarProgression(context.Background(), peerSyncNode())
	if err != nil {
		t.Fatalf("reconcileSidecarProgression() error = %v", err)
	}
	if len(mock.submitted) != 1 {
		t.Fatalf("expected 1 submitted task, got %d", len(mock.submitted))
	}
	if mock.submitted[0].TaskType() != taskDiscoverPeers {
		t.Errorf("first task = %q, want %q", mock.submitted[0].TaskType(), taskDiscoverPeers)
	}
}

func TestGenesisMode_TaskOrdering(t *testing.T) {
	mock := &mockSidecarClient{status: &sidecar.StatusResponse{Status: sidecar.Initializing}}
	r, _ := newProgressionReconciler(t, mock, genesisNode())

	_, err := r.reconcileSidecarProgression(context.Background(), genesisNode())
	if err != nil {
		t.Fatalf("reconcileSidecarProgression() error = %v", err)
	}
	if len(mock.submitted) != 1 {
		t.Fatalf("expected 1 submitted task, got %d", len(mock.submitted))
	}
	if mock.submitted[0].TaskType() != taskConfigPatch {
		t.Errorf("first task = %q, want %q", mock.submitted[0].TaskType(), taskConfigPatch)
	}
}

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

	mock := &mockSidecarClient{status: &sidecar.StatusResponse{Status: sidecar.Initializing}}
	r, _ := newProgressionReconciler(t, mock, node)

	_, err := r.reconcileSidecarProgression(context.Background(), node)
	if err != nil {
		t.Fatalf("reconcileSidecarProgression() error = %v", err)
	}
	if len(mock.submitted) != 1 {
		t.Fatalf("expected 1 submitted task, got %d", len(mock.submitted))
	}
	if mock.submitted[0].TaskType() != taskDiscoverPeers {
		t.Errorf("first task = %q, want %q", mock.submitted[0].TaskType(), taskDiscoverPeers)
	}
}

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
		if mock.submitted[0].TaskType() != expected[i] {
			t.Errorf("after %q: next task = %q, want %q", completedTask, mock.submitted[0].TaskType(), expected[i])
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

func TestPhaseRegression_ResetsRetryState(t *testing.T) {
	node := snapshotNode()
	mock := &mockSidecarClient{
		status: &sidecar.StatusResponse{Status: sidecar.Initializing, LastTask: &sidecar.TaskResult{Type: taskSnapshotRestore, Error: strPtr("failed")}},
	}
	r, _ := newProgressionReconciler(t, mock, node)

	// Simulate a failure to increment retry count.
	_, _ = r.reconcileSidecarProgression(context.Background(), node)

	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}
	state := r.getRetryState(key, taskSnapshotRestore)
	if state.Count == 0 {
		t.Fatal("expected retry count > 0 after failure")
	}

	// Sidecar restarts → Initializing with no lastTask.
	mock.status = &sidecar.StatusResponse{Status: sidecar.Initializing}
	_, _ = r.reconcileSidecarProgression(context.Background(), node)

	// Retry state should be reset.
	state = r.getRetryState(key, taskSnapshotRestore)
	if state.Count != 0 {
		t.Errorf("retry count after phase regression = %d, want 0", state.Count)
	}
}

func TestReconcileSidecarProgression_WritesStatusToNode(t *testing.T) {
	node := snapshotNode()
	mock := &mockSidecarClient{
		status: &sidecar.StatusResponse{Status: sidecar.Running},
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
	if updated.Status.SidecarPhase != sidecarRunning {
		t.Errorf("SidecarPhase = %q, want %q", updated.Status.SidecarPhase, sidecarRunning)
	}
}

func TestHandleTaskFailure_BootstrapRetryWithBackoff(t *testing.T) {
	node := snapshotNode()
	mock := &mockSidecarClient{
		status: &sidecar.StatusResponse{Status: sidecar.Initializing, LastTask: &sidecar.TaskResult{Type: taskSnapshotRestore, Error: strPtr("failed")}},
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

func TestHandleTaskFailure_MaxRetries_SetsDegradedCondition(t *testing.T) {
	node := snapshotNode()
	mock := &mockSidecarClient{
		status: &sidecar.StatusResponse{Status: sidecar.Initializing, LastTask: &sidecar.TaskResult{Type: taskSnapshotRestore, Error: strPtr("failed")}},
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

func TestHandleTaskFailure_RuntimeFailure_RequeuesAt30s(t *testing.T) {
	node := snapshotNode()

	mock := &mockSidecarClient{
		status: &sidecar.StatusResponse{Status: sidecar.Initializing, LastTask: &sidecar.TaskResult{Type: "update-peers", Error: strPtr("failed")}},
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

func TestRetryCounterReset_OnSidecarRestart(t *testing.T) {
	node := snapshotNode()
	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}

	mock := &mockSidecarClient{
		status: &sidecar.StatusResponse{Status: sidecar.Initializing, LastTask: &sidecar.TaskResult{Type: taskSnapshotRestore, Error: strPtr("failed")}},
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
	mock.status = &sidecar.StatusResponse{Status: sidecar.Initializing}
	_, _ = r.reconcileSidecarProgression(context.Background(), node)

	// All retry counters for this node should be cleared.
	state = r.getRetryState(key, taskSnapshotRestore)
	if state.Count != 0 {
		t.Errorf("retry count after restart = %d, want 0", state.Count)
	}
}

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

func TestTaskBuilderForNode_SnapshotRestore(t *testing.T) {
	node := snapshotNode()
	b := taskBuilderForNode(node, taskSnapshotRestore)
	task, ok := b.(sidecar.SnapshotRestoreTask)
	if !ok {
		t.Fatalf("expected SnapshotRestoreTask, got %T", b)
	}
	if task.Bucket != "my-bucket" {
		t.Errorf("Bucket = %q, want %q", task.Bucket, "my-bucket")
	}
	if task.Prefix != "snapshots/latest.tar" {
		t.Errorf("Prefix = %q, want %q", task.Prefix, "snapshots/latest.tar")
	}
	if task.Region != "us-east-1" {
		t.Errorf("Region = %q, want %q", task.Region, "us-east-1")
	}
}

func TestTaskBuilderForNode_DiscoverPeers_NoPeerConfig(t *testing.T) {
	node := snapshotNode()
	b := taskBuilderForNode(node, taskDiscoverPeers)
	task, ok := b.(sidecar.DiscoverPeersTask)
	if !ok {
		t.Fatalf("expected DiscoverPeersTask, got %T", b)
	}
	if len(task.Sources) != 0 {
		t.Errorf("expected empty sources when no PeerConfig, got %d", len(task.Sources))
	}
}

func TestTaskBuilderForNode_DiscoverPeers_EC2Tags(t *testing.T) {
	node := snapshotNode()
	node.Spec.Peers = &seiv1alpha1.PeerConfig{
		Sources: []seiv1alpha1.PeerSource{
			{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{
				Region: "eu-central-1",
				Tags:   map[string]string{"ChainIdentifier": "atlantic-2", "Component": "snapshotter"},
			}},
		},
	}
	b := taskBuilderForNode(node, taskDiscoverPeers)
	task, ok := b.(sidecar.DiscoverPeersTask)
	if !ok {
		t.Fatalf("expected DiscoverPeersTask, got %T", b)
	}
	if len(task.Sources) != 1 {
		t.Fatalf("expected 1 source, got %d", len(task.Sources))
	}
	if task.Sources[0].Type != sidecar.PeerSourceEC2Tags {
		t.Errorf("type = %v, want ec2Tags", task.Sources[0].Type)
	}
	if task.Sources[0].Region != "eu-central-1" {
		t.Errorf("region = %v, want eu-central-1", task.Sources[0].Region)
	}
	if task.Sources[0].Tags["ChainIdentifier"] != "atlantic-2" {
		t.Errorf("ChainIdentifier = %v, want atlantic-2", task.Sources[0].Tags["ChainIdentifier"])
	}
}

func TestTaskBuilderForNode_DiscoverPeers_Static(t *testing.T) {
	node := peerSyncNode()
	node.Spec.Peers = &seiv1alpha1.PeerConfig{
		Sources: []seiv1alpha1.PeerSource{
			{Static: &seiv1alpha1.StaticPeerSource{
				Addresses: []string{"abc@1.2.3.4:26656"},
			}},
		},
	}
	b := taskBuilderForNode(node, taskDiscoverPeers)
	task, ok := b.(sidecar.DiscoverPeersTask)
	if !ok {
		t.Fatalf("expected DiscoverPeersTask, got %T", b)
	}
	if len(task.Sources) != 1 {
		t.Fatalf("expected 1 source, got %d", len(task.Sources))
	}
	if task.Sources[0].Type != sidecar.PeerSourceStatic {
		t.Errorf("type = %v, want static", task.Sources[0].Type)
	}
	if len(task.Sources[0].Addresses) != 1 || task.Sources[0].Addresses[0] != "abc@1.2.3.4:26656" {
		t.Errorf("addresses = %v, want [abc@1.2.3.4:26656]", task.Sources[0].Addresses)
	}
}

func TestTaskBuilderForNode_DiscoverPeers_MultipleSources(t *testing.T) {
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
	b := taskBuilderForNode(node, taskDiscoverPeers)
	task, ok := b.(sidecar.DiscoverPeersTask)
	if !ok {
		t.Fatalf("expected DiscoverPeersTask, got %T", b)
	}
	if len(task.Sources) != 2 {
		t.Fatalf("expected 2 sources, got %d", len(task.Sources))
	}
	if task.Sources[0].Type != sidecar.PeerSourceEC2Tags {
		t.Errorf("sources[0].type = %v, want ec2Tags", task.Sources[0].Type)
	}
	if task.Sources[1].Type != sidecar.PeerSourceStatic {
		t.Errorf("sources[1].type = %v, want static", task.Sources[1].Type)
	}
}

func TestTaskBuilderForNode_MarkReady(t *testing.T) {
	node := snapshotNode()
	b := taskBuilderForNode(node, taskMarkReady)
	if _, ok := b.(sidecar.MarkReadyTask); !ok {
		t.Errorf("expected MarkReadyTask, got %T", b)
	}
	req := b.ToTaskRequest()
	if req.Params != nil {
		t.Errorf("mark-ready Params = %v, want nil", req.Params)
	}
}

func TestReconcileSidecarProgression_TaskRunning_Requeues(t *testing.T) {
	node := snapshotNode()
	mock := &mockSidecarClient{
		status: &sidecar.StatusResponse{Status: sidecar.Running},
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

func TestReconcileRuntimeTasks_NoTasks_RequeuesAtSteadyState(t *testing.T) {
	node := snapshotNode()
	mock := &mockSidecarClient{
		status: &sidecar.StatusResponse{Status: sidecar.Ready},
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
	node := snapshotterNode()

	mock := &mockSidecarClient{
		status:    &sidecar.StatusResponse{Status: sidecar.Ready},
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

func TestConfigureGenesisBuilder_WithS3(t *testing.T) {
	node := peerSyncNode()
	b := configureGenesisBuilder(node)
	task, ok := b.(sidecar.ConfigureGenesisTask)
	if !ok {
		t.Fatalf("expected ConfigureGenesisTask, got %T", b)
	}
	if task.URI != "s3://sei-testnet-genesis-config/atlantic-2/genesis.json" {
		t.Errorf("URI = %q, want %q", task.URI, "s3://sei-testnet-genesis-config/atlantic-2/genesis.json")
	}
	if task.Region != "us-east-2" {
		t.Errorf("Region = %q, want %q", task.Region, "us-east-2")
	}
}

func TestConfigureGenesisBuilder_WithoutS3(t *testing.T) {
	node := snapshotNode()
	b := configureGenesisBuilder(node)
	task, ok := b.(sidecar.ConfigureGenesisTask)
	if !ok {
		t.Fatalf("expected ConfigureGenesisTask, got %T", b)
	}
	if task.URI != "" || task.Region != "" {
		t.Errorf("expected empty fields when Genesis.S3 is nil, got URI=%q Region=%q", task.URI, task.Region)
	}
}

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

func TestConfigPatchBuilder_NoSnapshotGeneration(t *testing.T) {
	for _, node := range []*seiv1alpha1.SeiNode{snapshotNode(), peerSyncNode(), genesisNode()} {
		b := configPatchBuilder(node)
		task, ok := b.(sidecar.ConfigPatchTask)
		if !ok {
			t.Fatalf("expected ConfigPatchTask, got %T", b)
		}
		if task.SnapshotGeneration != nil {
			t.Errorf("expected nil SnapshotGeneration, got %+v", task.SnapshotGeneration)
		}
	}
}

func TestConfigPatchBuilder_WithSnapshotGeneration(t *testing.T) {
	node := snapshotNode()
	node.Spec.SnapshotGeneration = &seiv1alpha1.SnapshotGenerationConfig{
		Interval:   2000,
		KeepRecent: 10,
	}

	b := configPatchBuilder(node)
	task, ok := b.(sidecar.ConfigPatchTask)
	if !ok {
		t.Fatalf("expected ConfigPatchTask, got %T", b)
	}
	if task.SnapshotGeneration == nil {
		t.Fatal("expected non-nil SnapshotGeneration")
	}
	if task.SnapshotGeneration.Interval != 2000 {
		t.Errorf("Interval = %v, want 2000", task.SnapshotGeneration.Interval)
	}
	if task.SnapshotGeneration.KeepRecent != 10 {
		t.Errorf("KeepRecent = %v, want 10", task.SnapshotGeneration.KeepRecent)
	}
}

func TestConfigPatchBuilder_SnapshotGenerationDefaultKeepRecent(t *testing.T) {
	node := snapshotNode()
	node.Spec.SnapshotGeneration = &seiv1alpha1.SnapshotGenerationConfig{
		Interval: 1000,
	}

	b := configPatchBuilder(node)
	task := b.(sidecar.ConfigPatchTask)
	if task.SnapshotGeneration.KeepRecent != 5 {
		t.Errorf("KeepRecent = %v, want 5 (default)", task.SnapshotGeneration.KeepRecent)
	}
}

func TestSnapshotUploadTask_NoSnapshotGeneration(t *testing.T) {
	node := snapshotNode()
	if task := snapshotUploadTask(node); task != nil {
		t.Errorf("expected nil, got %v", task)
	}
}

func TestSnapshotUploadTask_NoDestination(t *testing.T) {
	node := snapshotNode()
	node.Spec.SnapshotGeneration = &seiv1alpha1.SnapshotGenerationConfig{
		Interval: 2000,
	}
	if task := snapshotUploadTask(node); task != nil {
		t.Errorf("expected nil, got %v", task)
	}
}

func TestSnapshotUploadTask_WithS3Destination(t *testing.T) {
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

	b := snapshotUploadTask(node)
	if b == nil {
		t.Fatal("expected non-nil task")
	}
	task, ok := b.(sidecar.SnapshotUploadTask)
	if !ok {
		t.Fatalf("expected SnapshotUploadTask, got %T", b)
	}
	if task.Bucket != "atlantic-2-snapshots" {
		t.Errorf("Bucket = %q, want %q", task.Bucket, "atlantic-2-snapshots")
	}
	if task.Prefix != "state-sync" {
		t.Errorf("Prefix = %q, want %q", task.Prefix, "state-sync")
	}
	if task.Region != "eu-central-1" {
		t.Errorf("Region = %q, want %q", task.Region, "eu-central-1")
	}
}

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
		status: &sidecar.StatusResponse{Status: sidecar.Ready},
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
	if mock.submitted[0].TaskType() != taskSnapshotUpload {
		t.Errorf("task type = %q, want %q", mock.submitted[0].TaskType(), taskSnapshotUpload)
	}
	req := mock.submitted[0].ToTaskRequest()
	if req.Params == nil || (*req.Params)["bucket"] != "atlantic-2-snapshots" {
		t.Errorf("bucket = %v, want %q", (*req.Params)["bucket"], "atlantic-2-snapshots")
	}
}

func TestReconcileRuntimeTasks_NoDestination_NoUploadSubmitted(t *testing.T) {
	node := snapshotNode()
	node.Spec.SnapshotGeneration = &seiv1alpha1.SnapshotGenerationConfig{
		Interval: 2000,
	}
	mock := &mockSidecarClient{
		status: &sidecar.StatusResponse{Status: sidecar.Ready},
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
