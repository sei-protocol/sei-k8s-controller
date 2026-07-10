package sei

import "testing"

const unsupportedKind = "Frobnicate"

func TestValidateWorkflowSpec(t *testing.T) {
	good := WorkflowSpec{
		Name:      "resync",
		Node:      "rpc-0",
		Kind:      WorkflowStateSync,
		StateSync: &StateSyncWorkflow{},
	}
	cases := []struct {
		name    string
		mut     func(*WorkflowSpec)
		wantErr bool
	}{
		{"ok", func(*WorkflowSpec) {}, false},
		{"no name", func(s *WorkflowSpec) { s.Name = "" }, true},
		{"no node", func(s *WorkflowSpec) { s.Node = "" }, true},
		{"unknown kind", func(s *WorkflowSpec) { s.Kind = unsupportedKind }, true},
		{"no payload", func(s *WorkflowSpec) { s.StateSync = nil }, true},
		{
			"kind/payload mismatch",
			func(s *WorkflowSpec) { s.Kind = unsupportedKind; s.StateSync = &StateSyncWorkflow{} },
			true,
		},
		{
			"statesync with config patch + witnesses",
			func(s *WorkflowSpec) {
				s.StateSync = &StateSyncWorkflow{
					ConfigPatch: map[string]map[string]any{"app.toml": {"state-store.evm-ss-split": true}},
					RpcServers:  []string{"a:26657", "b:26657"},
				}
			},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := good
			tc.mut(&s)
			if err := validateWorkflowSpec(s); (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}
