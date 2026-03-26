package planner

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

func TestBuildGroupAssemblyPlan(t *testing.T) {
	group := &seiv1alpha1.SeiNodeGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "test-group", Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeGroupSpec{
			Replicas: 3,
			Genesis: &seiv1alpha1.GenesisCeremonyConfig{
				ChainID: "arctic-1",
				GenesisS3: &seiv1alpha1.GenesisS3Destination{
					Bucket: "test-bucket",
					Prefix: "arctic-1/test-group/",
					Region: "us-east-2",
				},
			},
		},
	}

	nodes := []seiv1alpha1.SeiNode{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-2"}},
	}

	p, err := ForGroup(group)
	if err != nil {
		t.Fatalf("ForGroup: %v", err)
	}
	plan, err := p.BuildPlan(group, nodes)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	if plan.Phase != seiv1alpha1.TaskPlanActive {
		t.Errorf("phase = %q, want Active", plan.Phase)
	}

	if len(plan.Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(plan.Tasks))
	}

	// Task 0: assemble-and-upload-genesis
	assembleTask := plan.Tasks[0]
	if assembleTask.Type != TaskAssembleGenesis {
		t.Errorf("task[0] type = %q, want %q", assembleTask.Type, TaskAssembleGenesis)
	}
	if assembleTask.Status != seiv1alpha1.PlannedTaskPending {
		t.Errorf("task[0] status = %q, want Pending", assembleTask.Status)
	}
	if assembleTask.ID == "" {
		t.Error("task[0] ID should not be empty")
	}
	if assembleTask.MaxRetries != groupAssemblyMaxRetries {
		t.Errorf("task[0] MaxRetries = %d, want %d", assembleTask.MaxRetries, groupAssemblyMaxRetries)
	}
	if assembleTask.Params == nil {
		t.Fatal("task[0] params should not be nil")
	}

	var assembleParams task.AssembleAndUploadGenesisParams
	if err := json.Unmarshal(assembleTask.Params.Raw, &assembleParams); err != nil {
		t.Fatalf("unmarshal assemble params: %v", err)
	}
	if assembleParams.S3Bucket != "test-bucket" {
		t.Errorf("S3Bucket = %q, want %q", assembleParams.S3Bucket, "test-bucket")
	}
	if assembleParams.ChainID != "arctic-1" {
		t.Errorf("ChainID = %q, want %q", assembleParams.ChainID, "arctic-1")
	}
	if len(assembleParams.Nodes) != 3 {
		t.Errorf("expected 3 nodes, got %d", len(assembleParams.Nodes))
	}
	if assembleParams.Nodes[0].Name != "node-0" {
		t.Errorf("nodes[0].Name = %q, want %q", assembleParams.Nodes[0].Name, "node-0")
	}

	// Task 1: await-nodes-running
	awaitTask := plan.Tasks[1]
	if awaitTask.Type != TaskAwaitNodesRunning {
		t.Errorf("task[1] type = %q, want %q", awaitTask.Type, TaskAwaitNodesRunning)
	}
	if awaitTask.Params == nil {
		t.Fatal("task[1] params should not be nil")
	}

	var awaitParams task.AwaitNodesRunningParams
	if err := json.Unmarshal(awaitTask.Params.Raw, &awaitParams); err != nil {
		t.Fatalf("unmarshal await params: %v", err)
	}
	if awaitParams.GroupName != "test-group" {
		t.Errorf("GroupName = %q, want %q", awaitParams.GroupName, "test-group")
	}
	if awaitParams.Expected != 3 {
		t.Errorf("Expected = %d, want 3", awaitParams.Expected)
	}
}

func TestBuildGroupAssemblyPlan_DefaultS3(t *testing.T) {
	group := &seiv1alpha1.SeiNodeGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "my-group", Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeGroupSpec{
			Replicas: 1,
			Genesis: &seiv1alpha1.GenesisCeremonyConfig{
				ChainID: "pacific-1",
			},
		},
	}

	nodes := []seiv1alpha1.SeiNode{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0"}},
	}

	p, err := ForGroup(group)
	if err != nil {
		t.Fatalf("ForGroup: %v", err)
	}
	plan, err := p.BuildPlan(group, nodes)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	var params task.AssembleAndUploadGenesisParams
	if err := json.Unmarshal(plan.Tasks[0].Params.Raw, &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if params.S3Bucket != "sei-genesis-ceremony-artifacts" {
		t.Errorf("S3Bucket = %q, want %q", params.S3Bucket, "sei-genesis-ceremony-artifacts")
	}
	if params.S3Prefix != "pacific-1/my-group/" {
		t.Errorf("S3Prefix = %q, want %q", params.S3Prefix, "pacific-1/my-group/")
	}
}

func TestBuildGroupAssemblyPlan_DeterministicIDs(t *testing.T) {
	group := &seiv1alpha1.SeiNodeGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "det-group", Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeGroupSpec{
			Replicas: 2,
			Genesis: &seiv1alpha1.GenesisCeremonyConfig{
				ChainID: "test-chain",
			},
		},
	}
	nodes := []seiv1alpha1.SeiNode{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
	}

	p, err := ForGroup(group)
	if err != nil {
		t.Fatal(err)
	}
	plan1, err := p.BuildPlan(group, nodes)
	if err != nil {
		t.Fatal(err)
	}
	plan2, err := p.BuildPlan(group, nodes)
	if err != nil {
		t.Fatal(err)
	}
	if plan1.Tasks[0].ID != plan2.Tasks[0].ID {
		t.Errorf("assemble IDs not deterministic: %q vs %q", plan1.Tasks[0].ID, plan2.Tasks[0].ID)
	}
	if plan1.Tasks[1].ID != plan2.Tasks[1].ID {
		t.Errorf("await IDs not deterministic: %q vs %q", plan1.Tasks[1].ID, plan2.Tasks[1].ID)
	}
}
