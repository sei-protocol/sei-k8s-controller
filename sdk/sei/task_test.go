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
