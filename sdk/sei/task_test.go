package sei

import (
	"testing"
	"time"
)

func TestValidateTaskSpec(t *testing.T) {
	good := TaskSpec{
		Name:               "upgrade",
		Node:               "net-0",
		Kind:               TaskGovSoftwareUpgrade,
		GovSoftwareUpgrade: &GovSoftwareUpgrade{},
	}
	cases := []struct {
		name    string
		mut     func(*TaskSpec)
		wantErr bool
	}{
		{"ok", func(*TaskSpec) {}, false},
		{"no name", func(s *TaskSpec) { s.Name = "" }, true},
		{"no node", func(s *TaskSpec) { s.Node = "" }, true},
		{"unknown kind", func(s *TaskSpec) { s.Kind = "Frobnicate" }, true},
		{"no payload", func(s *TaskSpec) { s.GovSoftwareUpgrade = nil }, true},
		{
			"two payloads",
			func(s *TaskSpec) { s.GovVote = &GovVote{} },
			true,
		},
		{
			"payload mismatches kind",
			func(s *TaskSpec) {
				s.GovSoftwareUpgrade = nil
				s.GovVote = &GovVote{}
			},
			true,
		},
		{
			"vote valid",
			func(s *TaskSpec) {
				s.Kind = TaskGovVote
				s.GovSoftwareUpgrade = nil
				s.GovVote = &GovVote{ProposalID: 1, Option: VoteYes}
			},
			false,
		},
		{
			"vote bad option",
			func(s *TaskSpec) {
				s.Kind = TaskGovVote
				s.GovSoftwareUpgrade = nil
				s.GovVote = &GovVote{ProposalID: 1, Option: "maybe"}
			},
			true,
		},
		{
			"sub-second timeout",
			func(s *TaskSpec) { s.Timeout = 500 * time.Millisecond },
			true,
		},
		{
			"await valid",
			func(s *TaskSpec) {
				s.Kind = TaskAwaitNodesAtHeight
				s.GovSoftwareUpgrade = nil
				s.AwaitNodesAtHeight = &AwaitNodesAtHeight{TargetHeight: 100}
			},
			false,
		},
		{
			"update-image valid",
			func(s *TaskSpec) {
				s.Kind = TaskUpdateNodeImage
				s.GovSoftwareUpgrade = nil
				s.UpdateNodeImage = &UpdateNodeImage{Image: "img:v2"}
			},
			false,
		},
		{
			"config-patch valid",
			func(s *TaskSpec) {
				s.Kind = TaskConfigPatch
				s.GovSoftwareUpgrade = nil
				s.ConfigPatch = &ConfigPatch{Overrides: map[string]string{"giga_executor.enabled": "false"}}
			},
			false,
		},
		{
			"config-patch empty overrides",
			func(s *TaskSpec) {
				s.Kind = TaskConfigPatch
				s.GovSoftwareUpgrade = nil
				s.ConfigPatch = &ConfigPatch{Overrides: map[string]string{}}
			},
			true,
		},
		{
			"config-patch disallowed key",
			func(s *TaskSpec) {
				s.Kind = TaskConfigPatch
				s.GovSoftwareUpgrade = nil
				s.ConfigPatch = &ConfigPatch{Overrides: map[string]string{"evm.http_port": "8545"}}
			},
			true,
		},
		{
			"config-patch missing payload",
			func(s *TaskSpec) {
				s.Kind = TaskConfigPatch
				s.GovSoftwareUpgrade = nil
			},
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := good
			tc.mut(&s)
			if err := validateTaskSpec(s); (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}
