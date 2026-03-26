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

	plan, err := BuildGroupAssemblyPlan(group, nodes)
	if err != nil {
		t.Fatalf("BuildGroupAssemblyPlan: %v", err)
	}

	if plan.Phase != seiv1alpha1.TaskPlanActive {
		t.Errorf("phase = %q, want Active", plan.Phase)
	}

	if len(plan.Tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(plan.Tasks))
	}

	tsk := plan.Tasks[0]
	if tsk.Type != TaskAssembleGenesis {
		t.Errorf("task type = %q, want %q", tsk.Type, TaskAssembleGenesis)
	}
	if tsk.Status != seiv1alpha1.PlannedTaskPending {
		t.Errorf("task status = %q, want Pending", tsk.Status)
	}
	if tsk.ID == "" {
		t.Error("task ID should not be empty")
	}
	if tsk.Params == nil {
		t.Fatal("task params should not be nil")
	}

	var params task.AssembleAndUploadGenesisParams
	if err := json.Unmarshal(tsk.Params.Raw, &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if params.S3Bucket != "test-bucket" {
		t.Errorf("S3Bucket = %q, want %q", params.S3Bucket, "test-bucket")
	}
	if params.ChainID != "arctic-1" {
		t.Errorf("ChainID = %q, want %q", params.ChainID, "arctic-1")
	}
	if len(params.Nodes) != 3 {
		t.Errorf("expected 3 nodes, got %d", len(params.Nodes))
	}
	if params.Nodes[0].Name != "node-0" {
		t.Errorf("nodes[0].Name = %q, want %q", params.Nodes[0].Name, "node-0")
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

	plan, err := BuildGroupAssemblyPlan(group, nodes)
	if err != nil {
		t.Fatalf("BuildGroupAssemblyPlan: %v", err)
	}

	var params task.AssembleAndUploadGenesisParams
	if err := json.Unmarshal(plan.Tasks[0].Params.Raw, &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if params.S3Bucket != "sei-genesis-artifacts" {
		t.Errorf("S3Bucket = %q, want %q", params.S3Bucket, "sei-genesis-artifacts")
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

	plan1, err := BuildGroupAssemblyPlan(group, nodes)
	if err != nil {
		t.Fatal(err)
	}
	plan2, err := BuildGroupAssemblyPlan(group, nodes)
	if err != nil {
		t.Fatal(err)
	}
	if plan1.Tasks[0].ID != plan2.Tasks[0].ID {
		t.Errorf("IDs not deterministic: %q vs %q", plan1.Tasks[0].ID, plan2.Tasks[0].ID)
	}
}
