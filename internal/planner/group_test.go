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
			},
		},
		Status: seiv1alpha1.SeiNodeGroupStatus{
			IncumbentNodes: []string{"node-0", "node-1", "node-2"},
			Conditions: []metav1.Condition{
				{Type: seiv1alpha1.ConditionGenesisCeremonyNeeded, Status: metav1.ConditionTrue},
			},
		},
	}

	p, err := ForGroup(group)
	if err != nil {
		t.Fatalf("ForGroup: %v", err)
	}
	plan, err := p.BuildPlan(group)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	if plan.Phase != seiv1alpha1.TaskPlanActive {
		t.Errorf("phase = %q, want Active", plan.Phase)
	}

	if len(plan.Tasks) != 3 {
		t.Fatalf("expected 3 tasks, got %d", len(plan.Tasks))
	}

	// Task 0: assemble-and-upload-genesis
	assembleTask := plan.Tasks[0]
	if assembleTask.Type != TaskAssembleGenesis {
		t.Errorf("task[0] type = %q, want %q", assembleTask.Type, TaskAssembleGenesis)
	}
	if assembleTask.Status != seiv1alpha1.TaskPending {
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
	if len(assembleParams.Nodes) != 3 {
		t.Errorf("expected 3 nodes, got %d", len(assembleParams.Nodes))
	}
	if assembleParams.Nodes[0].Name != "node-0" {
		t.Errorf("nodes[0].Name = %q, want %q", assembleParams.Nodes[0].Name, "node-0")
	}
	if assembleParams.Namespace != "default" {
		t.Errorf("Namespace = %q, want %q", assembleParams.Namespace, "default")
	}

	// Task 1: collect-and-set-peers
	peersTask := plan.Tasks[1]
	if peersTask.Type != task.TaskTypeCollectAndSetPeers {
		t.Errorf("task[1] type = %q, want %q", peersTask.Type, task.TaskTypeCollectAndSetPeers)
	}

	// Task 2: await-nodes-running
	awaitTask := plan.Tasks[2]
	if awaitTask.Type != TaskAwaitNodesRunning {
		t.Errorf("task[2] type = %q, want %q", awaitTask.Type, TaskAwaitNodesRunning)
	}
	if awaitTask.Params == nil {
		t.Fatal("task[2] params should not be nil")
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
		Status: seiv1alpha1.SeiNodeGroupStatus{
			IncumbentNodes: []string{"node-0"},
			Conditions: []metav1.Condition{
				{Type: seiv1alpha1.ConditionGenesisCeremonyNeeded, Status: metav1.ConditionTrue},
			},
		},
	}

	p, err := ForGroup(group)
	if err != nil {
		t.Fatalf("ForGroup: %v", err)
	}
	plan, err := p.BuildPlan(group)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	var params task.AssembleAndUploadGenesisParams
	if err := json.Unmarshal(plan.Tasks[0].Params.Raw, &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if params.Namespace != "default" {
		t.Errorf("Namespace = %q, want %q", params.Namespace, "default")
	}
	if len(params.Nodes) != 1 {
		t.Errorf("expected 1 node, got %d", len(params.Nodes))
	}
}

func TestBuildGroupForkPlan(t *testing.T) {
	const sourceChain = "pacific-1"

	group := &seiv1alpha1.SeiNodeGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "fork-group", Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeGroupSpec{
			Replicas: 2,
			Genesis: &seiv1alpha1.GenesisCeremonyConfig{
				ChainID:        "fork-1",
				AccountBalance: "1000usei",
				Fork: &seiv1alpha1.ForkConfig{
					SourceChainID: sourceChain,
					SourceImage:   "sei:v5.0.0",
					ExportHeight:  100000,
				},
			},
		},
		Status: seiv1alpha1.SeiNodeGroupStatus{
			IncumbentNodes: []string{"fork-group-0", "fork-group-1"},
			Conditions: []metav1.Condition{
				{Type: seiv1alpha1.ConditionForkGenesisCeremonyNeeded, Status: metav1.ConditionTrue},
			},
		},
	}

	p, err := ForGroup(group)
	if err != nil {
		t.Fatalf("ForGroup: %v", err)
	}
	plan, err := p.BuildPlan(group)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	if plan.Phase != seiv1alpha1.TaskPlanActive {
		t.Errorf("phase = %q, want Active", plan.Phase)
	}

	// Fork plan: 4 exporter tasks + 3 shared tasks = 7
	if len(plan.Tasks) != 7 {
		t.Fatalf("expected 7 tasks, got %d", len(plan.Tasks))
	}

	expectedTypes := []string{
		task.TaskTypeCreateExporter,
		task.TaskTypeAwaitExporterRunning,
		task.TaskTypeSubmitExportState,
		task.TaskTypeTeardownExporter,
		TaskAssembleGenesisFork,
		task.TaskTypeCollectAndSetPeers,
		TaskAwaitNodesRunning,
	}
	for i, wantType := range expectedTypes {
		if plan.Tasks[i].Type != wantType {
			t.Errorf("task[%d] type = %q, want %q", i, plan.Tasks[i].Type, wantType)
		}
		if plan.Tasks[i].Status != seiv1alpha1.TaskPending {
			t.Errorf("task[%d] status = %q, want Pending", i, plan.Tasks[i].Status)
		}
		if plan.Tasks[i].ID == "" {
			t.Errorf("task[%d] ID should not be empty", i)
		}
	}

	// Verify create-exporter params
	var createParams task.CreateExporterParams
	if err := json.Unmarshal(plan.Tasks[0].Params.Raw, &createParams); err != nil {
		t.Fatalf("unmarshal create-exporter params: %v", err)
	}
	if createParams.GroupName != "fork-group" {
		t.Errorf("GroupName = %q, want %q", createParams.GroupName, "fork-group")
	}
	if createParams.ExporterName != "fork-group-exporter" {
		t.Errorf("ExporterName = %q, want %q", createParams.ExporterName, "fork-group-exporter")
	}
	if createParams.SourceChainID != sourceChain {
		t.Errorf("SourceChainID = %q, want %q", createParams.SourceChainID, sourceChain)
	}
	if createParams.SourceImage != "sei:v5.0.0" {
		t.Errorf("SourceImage = %q, want %q", createParams.SourceImage, "sei:v5.0.0")
	}
	if createParams.ExportHeight != 100000 {
		t.Errorf("ExportHeight = %d, want %d", createParams.ExportHeight, 100000)
	}

	// Verify submit-export-state params
	var submitParams task.SubmitExportStateParams
	if err := json.Unmarshal(plan.Tasks[2].Params.Raw, &submitParams); err != nil {
		t.Fatalf("unmarshal submit-export-state params: %v", err)
	}
	if submitParams.SourceChainID != sourceChain {
		t.Errorf("submit SourceChainID = %q, want %q", submitParams.SourceChainID, sourceChain)
	}

	// Verify assemble-genesis-fork params
	var assembleParams task.AssembleForkGenesisParams
	if err := json.Unmarshal(plan.Tasks[4].Params.Raw, &assembleParams); err != nil {
		t.Fatalf("unmarshal assemble-genesis-fork params: %v", err)
	}
	if assembleParams.SourceChainID != sourceChain {
		t.Errorf("assemble SourceChainID = %q, want %q", assembleParams.SourceChainID, sourceChain)
	}
	if len(assembleParams.Nodes) != 2 {
		t.Errorf("assemble Nodes = %d, want 2", len(assembleParams.Nodes))
	}

	// Verify assemble task has MaxRetries
	if plan.Tasks[4].MaxRetries != groupAssemblyMaxRetries {
		t.Errorf("assemble MaxRetries = %d, want %d", plan.Tasks[4].MaxRetries, groupAssemblyMaxRetries)
	}
}

func TestBuildGroupForkPlan_NilForkSpecNoExporterTasks(t *testing.T) {
	// ForkGenesisCeremonyNeeded condition is set but Fork spec is nil.
	// The planner should fall back to the standard genesis plan.
	group := &seiv1alpha1.SeiNodeGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-group", Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeGroupSpec{
			Replicas: 1,
			Genesis: &seiv1alpha1.GenesisCeremonyConfig{
				ChainID: "test-1",
			},
		},
		Status: seiv1alpha1.SeiNodeGroupStatus{
			IncumbentNodes: []string{"edge-group-0"},
			Conditions: []metav1.Condition{
				{Type: seiv1alpha1.ConditionForkGenesisCeremonyNeeded, Status: metav1.ConditionTrue},
			},
		},
	}

	p, err := ForGroup(group)
	if err != nil {
		t.Fatalf("ForGroup: %v", err)
	}
	plan, err := p.BuildPlan(group)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	// Should fall back to standard genesis (3 tasks, no exporter)
	if len(plan.Tasks) != 3 {
		t.Fatalf("expected 3 tasks (standard genesis fallback), got %d", len(plan.Tasks))
	}
	if plan.Tasks[0].Type != TaskAssembleGenesis {
		t.Errorf("task[0] type = %q, want %q", plan.Tasks[0].Type, TaskAssembleGenesis)
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
		Status: seiv1alpha1.SeiNodeGroupStatus{
			IncumbentNodes: []string{"node-0", "node-1"},
			Conditions: []metav1.Condition{
				{Type: seiv1alpha1.ConditionGenesisCeremonyNeeded, Status: metav1.ConditionTrue},
			},
		},
	}

	p, err := ForGroup(group)
	if err != nil {
		t.Fatal(err)
	}
	plan1, err := p.BuildPlan(group)
	if err != nil {
		t.Fatal(err)
	}
	plan2, err := p.BuildPlan(group)
	if err != nil {
		t.Fatal(err)
	}
	if plan1.Tasks[0].ID != plan2.Tasks[0].ID {
		t.Errorf("assemble IDs not deterministic: %q vs %q", plan1.Tasks[0].ID, plan2.Tasks[0].ID)
	}
	if plan1.Tasks[2].ID != plan2.Tasks[2].ID {
		t.Errorf("await IDs not deterministic: %q vs %q", plan1.Tasks[2].ID, plan2.Tasks[2].ID)
	}
}
