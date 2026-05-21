package planner

import (
	"encoding/json"
	"testing"

	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

func TestBuildGroupAssemblyPlan(t *testing.T) {
	group := &seiv1alpha1.SeiNodeDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "test-group", Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeDeploymentSpec{
			Replicas: 3,
			Genesis: &seiv1alpha1.GenesisCeremonyConfig{
				ChainID: "arctic-1", AccountBalance: testAccountBalance,
			},
		},
		Status: seiv1alpha1.SeiNodeDeploymentStatus{
			IncumbentNodes: []string{"node-0", "node-1", "node-2"},
			// Absence of ConditionGenesisCeremonyComplete=True (the new
			// signal) means the planner treats the SND as needing a
			// genesis plan.

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

	var assembleParams sidecar.AssembleAndUploadGenesisTask
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
	group := &seiv1alpha1.SeiNodeDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "my-group", Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeDeploymentSpec{
			Replicas: 1,
			Genesis: &seiv1alpha1.GenesisCeremonyConfig{
				ChainID: sourceChainID, AccountBalance: testAccountBalance,
			},
		},
		Status: seiv1alpha1.SeiNodeDeploymentStatus{
			IncumbentNodes: []string{"node-0"},
			// Absence of ConditionGenesisCeremonyComplete=True (the new
			// signal) means the planner treats the SND as needing a
			// genesis plan.

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

	var params sidecar.AssembleAndUploadGenesisTask
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

// Locks in plumbing of spec.genesis.overrides into the assemble-and-upload-genesis
// task params; the sidecar consumes "overrides" from the wire request at
// genesis-assembly time. Loss of this propagation is a silent regression that
// would leave SIP-3 test clusters running with cosmos-default params.
func TestBuildGroupAssemblyPlan_PropagatesOverrides(t *testing.T) {
	overrides := map[string]apiextensionsv1.JSON{
		"staking.params.unbonding_time": {Raw: []byte(`"600s"`)},
		"gov.params.voting_period":      {Raw: []byte(`"30s"`)},
	}
	group := &seiv1alpha1.SeiNodeDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "ov-group", Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeDeploymentSpec{
			Replicas: 1,
			Genesis: &seiv1alpha1.GenesisCeremonyConfig{
				ChainID: sourceChainID, AccountBalance: testAccountBalance,
				Overrides: overrides,
			},
		},
		Status: seiv1alpha1.SeiNodeDeploymentStatus{
			IncumbentNodes: []string{testNodeName},
			// Absence of ConditionGenesisCeremonyComplete=True (the new
			// signal) means the planner treats the SND as needing a
			// genesis plan.

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

	var params sidecar.AssembleAndUploadGenesisTask
	if err := json.Unmarshal(plan.Tasks[0].Params.Raw, &params); err != nil {
		t.Fatalf("unmarshal assemble params: %v", err)
	}
	if len(params.Overrides) != 2 {
		t.Fatalf("Overrides: got %d entries, want 2", len(params.Overrides))
	}
	if got := string(params.Overrides["staking.params.unbonding_time"]); got != `"600s"` {
		t.Errorf("staking.params.unbonding_time = %s, want \"600s\"", got)
	}
	if got := string(params.Overrides["gov.params.voting_period"]); got != `"30s"` {
		t.Errorf("gov.params.voting_period = %s, want \"30s\"", got)
	}

	// Wire-shape check: TaskRequest.Params surfaces "overrides" as a flat
	// map keyed by the same dotted paths, matching the sidecar contract.
	req := params.ToTaskRequest()
	if req.Params == nil {
		t.Fatal("TaskRequest.Params should not be nil")
	}
	wire, ok := (*req.Params)["overrides"].(map[string]any)
	if !ok {
		t.Fatalf("TaskRequest.Params[\"overrides\"] not a map: %T", (*req.Params)["overrides"])
	}
	if _, ok := wire["staking.params.unbonding_time"]; !ok {
		t.Errorf("wire overrides missing staking.params.unbonding_time: %#v", wire)
	}
}

// Ensures the assemble-genesis Params shape stays the upstream title-cased one
// when the user omits Overrides — guards against accidental field-rename drift.
func TestBuildGroupAssemblyPlan_OmitsOverridesWhenUnset(t *testing.T) {
	group := &seiv1alpha1.SeiNodeDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "noov-group", Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeDeploymentSpec{
			Replicas: 1,
			Genesis: &seiv1alpha1.GenesisCeremonyConfig{
				ChainID: sourceChainID, AccountBalance: testAccountBalance,
			},
		},
		Status: seiv1alpha1.SeiNodeDeploymentStatus{
			IncumbentNodes: []string{testNodeName},
			// Absence of ConditionGenesisCeremonyComplete=True (the new
			// signal) means the planner treats the SND as needing a
			// genesis plan.

		},
	}

	p, _ := ForGroup(group)
	plan, err := p.BuildPlan(group)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	var params sidecar.AssembleAndUploadGenesisTask
	if err := json.Unmarshal(plan.Tasks[0].Params.Raw, &params); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(params.Overrides) != 0 {
		t.Errorf("Overrides should be empty, got %d entries", len(params.Overrides))
	}
	// Wire request must not advertise an "overrides" key when unset; the
	// sidecar treats an empty map and a missing key identically, but the
	// missing-key shape avoids ambiguity in cross-version compatibility.
	req := params.ToTaskRequest()
	if req.Params != nil {
		if _, present := (*req.Params)["overrides"]; present {
			t.Errorf("TaskRequest.Params should not contain \"overrides\" when unset; got: %#v", *req.Params)
		}
	}
}

func TestBuildGroupAssemblyPlan_UniqueIDsAcrossRebuilds(t *testing.T) {
	group := &seiv1alpha1.SeiNodeDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "det-group", Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeDeploymentSpec{
			Replicas: 2,
			Genesis: &seiv1alpha1.GenesisCeremonyConfig{
				ChainID:        "test-chain",
				AccountBalance: testAccountBalance,
			},
		},
		Status: seiv1alpha1.SeiNodeDeploymentStatus{
			IncumbentNodes: []string{"node-0", "node-1"},
			// Absence of ConditionGenesisCeremonyComplete=True (the new
			// signal) means the planner treats the SND as needing a
			// genesis plan.

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
	if plan1.ID == plan2.ID {
		t.Errorf("plan IDs should differ across rebuilds: both %q", plan1.ID)
	}
	if plan1.Tasks[0].ID == plan2.Tasks[0].ID {
		t.Errorf("assemble task IDs should differ across rebuilds: both %q", plan1.Tasks[0].ID)
	}
	if plan1.Tasks[2].ID == plan2.Tasks[2].ID {
		t.Errorf("await task IDs should differ across rebuilds: both %q", plan1.Tasks[2].ID)
	}
}
