package sei

import "testing"

const (
	unsupportedKind = "Frobnicate"
	backendPebble   = "pebbledb"
	backendRocks    = "rocksdb"
)

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
			"statesync with gigastore migration + witnesses",
			func(s *WorkflowSpec) {
				s.StateSync = &StateSyncWorkflow{
					Migration: &ConfigMigration{
						Kind:      ConfigMigrationGigaStore,
						GigaStore: &GigaStoreMigration{Backend: backendRocks},
					},
					RpcServers: []string{witnessA, witnessB},
				}
			},
			false,
		},
		{
			"gigastore backend pebbledb ok",
			func(s *WorkflowSpec) {
				s.StateSync = &StateSyncWorkflow{
					Migration:  &ConfigMigration{Kind: ConfigMigrationGigaStore, GigaStore: &GigaStoreMigration{Backend: backendPebble}},
					RpcServers: []string{witnessA, witnessB},
				}
			},
			false,
		},
		{
			"gigastore backend empty defaults ok",
			func(s *WorkflowSpec) {
				s.StateSync = &StateSyncWorkflow{
					Migration:  &ConfigMigration{Kind: ConfigMigrationGigaStore, GigaStore: &GigaStoreMigration{}},
					RpcServers: []string{witnessA, witnessB},
				}
			},
			false,
		},
		{
			"gigastore backend invalid",
			func(s *WorkflowSpec) {
				s.StateSync = &StateSyncWorkflow{
					Migration:  &ConfigMigration{Kind: ConfigMigrationGigaStore, GigaStore: &GigaStoreMigration{Backend: "badger"}},
					RpcServers: []string{witnessA, witnessB},
				}
			},
			true,
		},
		{
			"statesync migration missing payload",
			func(s *WorkflowSpec) {
				s.StateSync = &StateSyncWorkflow{
					Migration:  &ConfigMigration{Kind: ConfigMigrationGigaStore},
					RpcServers: []string{witnessA, witnessB},
				}
			},
			true,
		},
		{
			"statesync migration unknown kind",
			func(s *WorkflowSpec) {
				s.StateSync = &StateSyncWorkflow{
					Migration:  &ConfigMigration{Kind: "Bogus", GigaStore: &GigaStoreMigration{}},
					RpcServers: []string{witnessA, witnessB},
				}
			},
			true,
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
