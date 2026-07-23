package planner

import (
	"encoding/json"
	"strings"
	"testing"

	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	validSeiAddr  = "sei1zg69v7y6hn00qy352euf40x77qfrg4nclsjzp9"
	validSeiAddr2 = "sei140x77qfrg4ncn27dauqjx3t83x4ummcpmrsjjl"

	testAccountBalance  = "1000000usei"
	testBalance1000usei = "1000usei"
	testGroupName       = "test-group"
	testNodeName        = "node-0"
	sourceChainID       = "pacific-1"
)

func groupWithAccounts(accounts []seiv1alpha1.GenesisAccount) *seiv1alpha1.SeiNetwork {
	return &seiv1alpha1.SeiNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: testGroupName, Namespace: "nightly"},
		Spec: seiv1alpha1.SeiNetworkSpec{
			Replicas: 1,
			Genesis: seiv1alpha1.GenesisCeremonyConfig{
				ChainID:        "arctic-1",
				AccountBalance: testAccountBalance,
				Accounts:       accounts,
			},
		},
		Status: seiv1alpha1.SeiNetworkStatus{
			IncumbentNodes: []string{testNodeName},
		},
	}
}

func TestBuildPlan_PropagatesAccounts(t *testing.T) {
	group := groupWithAccounts([]seiv1alpha1.GenesisAccount{
		{Address: validSeiAddr, Balance: testBalance1000usei},
	})
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
		t.Fatalf("unmarshal: %v", err)
	}
	if len(params.Accounts) != 1 {
		t.Fatalf("Accounts: got %d, want 1", len(params.Accounts))
	}
	if params.Accounts[0].Address != validSeiAddr || params.Accounts[0].Balance != testBalance1000usei {
		t.Errorf("Accounts[0] = %+v", params.Accounts[0])
	}
}

func TestBuildPlan_NilAccountsOmitsField(t *testing.T) {
	group := groupWithAccounts(nil)
	p, _ := ForGroup(group)
	plan, err := p.BuildPlan(group)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if strings.Contains(string(plan.Tasks[0].Params.Raw), `"accounts"`) {
		t.Errorf("nil accounts should omit field; got: %s", string(plan.Tasks[0].Params.Raw))
	}
}

func TestBuildPlan_PropagatesVesting(t *testing.T) {
	group := groupWithAccounts([]seiv1alpha1.GenesisAccount{
		{
			Address: validSeiAddr,
			Balance: "2000000usei",
			Vesting: &seiv1alpha1.GenesisAccountVesting{
				Amount:  testAccountBalance,
				EndTime: 1893456000,
				Delayed: true,
			},
		},
		{Address: validSeiAddr2, Balance: testBalance1000usei}, // no Vesting: must stay nil, not zero-valued
	})
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
		t.Fatalf("unmarshal: %v", err)
	}
	if len(params.Accounts) != 2 {
		t.Fatalf("Accounts: got %d, want 2", len(params.Accounts))
	}
	v := params.Accounts[0].Vesting
	if v == nil {
		t.Fatalf("Accounts[0].Vesting: got nil, want set")
	}
	if v.Amount != testAccountBalance || v.EndTime != 1893456000 || !v.Delayed {
		t.Errorf("Accounts[0].Vesting = %+v", v)
	}
	if params.Accounts[1].Vesting != nil {
		t.Errorf("Accounts[1].Vesting: got %+v, want nil", params.Accounts[1].Vesting)
	}
}

func TestBuildPlan_RejectsBadBech32_PlannerTime(t *testing.T) {
	// Validate() runs at planner time so a bad address surfaces in
	// kubectl describe rather than burning a sidecar Job pod.
	group := groupWithAccounts([]seiv1alpha1.GenesisAccount{
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

func TestBuildPlan_PropagatesMultipleAccounts(t *testing.T) {
	// Locks in the slice copy at group.go:37-40 — if a future
	// refactor switches the idiom and silently drops entries past
	// index 0, this test catches it.
	group := groupWithAccounts([]seiv1alpha1.GenesisAccount{
		{Address: validSeiAddr, Balance: testBalance1000usei},
		{Address: validSeiAddr2, Balance: "2000usei,500uatom"},
	})
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
		t.Fatalf("unmarshal: %v", err)
	}
	if len(params.Accounts) != 2 {
		t.Fatalf("Accounts: got %d, want 2", len(params.Accounts))
	}
	if params.Accounts[0].Address != validSeiAddr || params.Accounts[1].Address != validSeiAddr2 {
		t.Errorf("addresses: got [%q, %q]", params.Accounts[0].Address, params.Accounts[1].Address)
	}
	if params.Accounts[1].Balance != "2000usei,500uatom" {
		t.Errorf("multi-denom balance: got %q", params.Accounts[1].Balance)
	}
}
