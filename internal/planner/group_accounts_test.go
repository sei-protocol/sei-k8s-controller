package planner

import (
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const validSeiAddr = "sei1zg69v7y6hn00qy352euf40x77qfrg4nclsjzp9"

func groupWithAccounts(fork bool, accounts []seiv1alpha1.GenesisAccount) *seiv1alpha1.SeiNodeDeployment {
	cond := seiv1alpha1.ConditionGenesisCeremonyNeeded
	if fork {
		cond = seiv1alpha1.ConditionForkGenesisCeremonyNeeded
	}
	g := &seiv1alpha1.SeiNodeDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "test-group", Namespace: "nightly"},
		Spec: seiv1alpha1.SeiNodeDeploymentSpec{
			Replicas: 1,
			Genesis: &seiv1alpha1.GenesisCeremonyConfig{
				ChainID:        "arctic-1",
				AccountBalance: "1000000usei",
				Accounts:       accounts,
			},
		},
		Status: seiv1alpha1.SeiNodeDeploymentStatus{
			IncumbentNodes: []string{"node-0"},
			Conditions:     []metav1.Condition{{Type: cond, Status: metav1.ConditionTrue}},
		},
	}
	if fork {
		g.Spec.Genesis.Fork = &seiv1alpha1.ForkConfig{
			SourceChainID: "pacific-1",
			SourceImage:   "ecr/img@sha256:abc",
			ExportHeight:  100,
		}
	}
	return g
}

func TestBuildPlan_NonFork_PropagatesAccounts(t *testing.T) {
	group := groupWithAccounts(false, []seiv1alpha1.GenesisAccount{
		{Address: validSeiAddr, Balance: "1000usei"},
	})
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
		t.Fatalf("unmarshal: %v", err)
	}
	if len(params.Accounts) != 1 {
		t.Fatalf("Accounts: got %d, want 1", len(params.Accounts))
	}
	if params.Accounts[0].Address != validSeiAddr || params.Accounts[0].Balance != "1000usei" {
		t.Errorf("Accounts[0] = %+v", params.Accounts[0])
	}
}

func TestBuildPlan_Fork_PropagatesAccounts(t *testing.T) {
	group := groupWithAccounts(true, []seiv1alpha1.GenesisAccount{
		{Address: validSeiAddr, Balance: "5000usei"},
	})
	p, err := ForGroup(group)
	if err != nil {
		t.Fatalf("ForGroup: %v", err)
	}
	plan, err := p.BuildPlan(group)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	// Fork plan prepends 4 exporter tasks; assemble is at index 4.
	var params task.AssembleForkGenesisParams
	if err := json.Unmarshal(plan.Tasks[4].Params.Raw, &params); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(params.Accounts) != 1 {
		t.Fatalf("Accounts: got %d, want 1", len(params.Accounts))
	}
}

func TestBuildPlan_NilAccountsOmitsField(t *testing.T) {
	group := groupWithAccounts(false, nil)
	p, _ := ForGroup(group)
	plan, err := p.BuildPlan(group)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if strings.Contains(string(plan.Tasks[0].Params.Raw), `"accounts"`) {
		t.Errorf("nil accounts should omit field; got: %s", string(plan.Tasks[0].Params.Raw))
	}
}

func TestBuildPlan_RejectsBadBech32_PlannerTime(t *testing.T) {
	// Validate() runs at planner time so a bad address surfaces in
	// kubectl describe rather than burning a sidecar Job pod.
	group := groupWithAccounts(false, []seiv1alpha1.GenesisAccount{
		{Address: "cosmos1zg69v7y6hn00qy352euf40x77qfrg4ncjur58y", Balance: "1usei"},
	})
	p, _ := ForGroup(group)
	_, err := p.BuildPlan(group)
	if err == nil {
		t.Fatal("expected planner-time error for non-sei address")
	}
	if !strings.Contains(err.Error(), "sei") {
		t.Errorf("error: got %q", err.Error())
	}
}
