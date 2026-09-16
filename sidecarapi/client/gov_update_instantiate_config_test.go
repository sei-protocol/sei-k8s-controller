package client

import "testing"

func validGovUpdateInstantiateConfigTask() GovUpdateInstantiateConfigTask {
	return GovUpdateInstantiateConfigTask{
		ChainID:     "arctic-1",
		KeyName:     "node_admin",
		Title:       "Disable CosmWasm Contract Instantiation",
		Description: "Set every existing code's instantiate permission to Nobody.",
		Updates: []InstantiateConfigUpdateInput{
			{CodeID: 1, Permission: "nobody"},
			{CodeID: 2, Permission: "everybody"},
			{CodeID: 3, Permission: validSeiAddr1},
		},
		InitialDeposit: "10000000usei",
		Fees:           "30000usei",
		Gas:            1_200_000,
	}
}

func TestGovUpdateInstantiateConfigTaskValidate(t *testing.T) {
	if err := validGovUpdateInstantiateConfigTask().Validate(); err != nil {
		t.Fatalf("valid task: unexpected error: %v", err)
	}

	cases := []struct {
		name string
		mut  func(*GovUpdateInstantiateConfigTask)
	}{
		{"missing chainId", func(task *GovUpdateInstantiateConfigTask) { task.ChainID = "" }},
		{"missing keyName", func(task *GovUpdateInstantiateConfigTask) { task.KeyName = "" }},
		{"missing title", func(task *GovUpdateInstantiateConfigTask) { task.Title = "" }},
		{"missing description", func(task *GovUpdateInstantiateConfigTask) { task.Description = "" }},
		{"empty updates", func(task *GovUpdateInstantiateConfigTask) { task.Updates = nil }},
		{"zero codeId", func(task *GovUpdateInstantiateConfigTask) { task.Updates[0].CodeID = 0 }},
		{"duplicate codeId", func(task *GovUpdateInstantiateConfigTask) {
			task.Updates[1].CodeID = task.Updates[0].CodeID
		}},
		{"missing permission", func(task *GovUpdateInstantiateConfigTask) { task.Updates[0].Permission = "" }},
		{"invalid permission", func(task *GovUpdateInstantiateConfigTask) {
			task.Updates[0].Permission = "somebody"
		}},
		{"missing initialDeposit", func(task *GovUpdateInstantiateConfigTask) { task.InitialDeposit = "" }},
		{"missing fees", func(task *GovUpdateInstantiateConfigTask) { task.Fees = "" }},
		{"zero gas", func(task *GovUpdateInstantiateConfigTask) { task.Gas = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := validGovUpdateInstantiateConfigTask()
			tc.mut(&task)
			if err := task.Validate(); err == nil {
				t.Fatalf("expected validation error for %q", tc.name)
			}
		})
	}
}

func TestGovUpdateInstantiateConfigTaskToTaskRequest(t *testing.T) {
	task := validGovUpdateInstantiateConfigTask()
	req := task.ToTaskRequest()
	if req.Type != TaskTypeGovUpdateInstantiateConfig {
		t.Errorf("Type = %q, want %q", req.Type, TaskTypeGovUpdateInstantiateConfig)
	}
	if req.Params == nil {
		t.Fatal("Params is nil")
	}
	params := *req.Params
	for _, key := range []string{
		"chainId", "keyName", "title", "description", "updates",
		"initialDeposit", "fees", "gas",
	} {
		if _, ok := params[key]; !ok {
			t.Errorf("params missing key %q", key)
		}
	}
	if _, ok := params["memo"]; ok {
		t.Error("memo present but was empty")
	}
}
