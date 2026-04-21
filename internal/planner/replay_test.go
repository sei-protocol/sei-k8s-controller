package planner

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func TestReplayerPlanner_Validate_ResultExport(t *testing.T) {
	cases := []struct {
		name    string
		re      *seiv1alpha1.ResultExportConfig
		wantErr string
	}{
		{
			name: "nil is fine",
			re:   nil,
		},
		{
			name: "shadowResult set is fine",
			re: &seiv1alpha1.ResultExportConfig{
				ShadowResult: &seiv1alpha1.ShadowResultConfig{
					CanonicalRPC: "http://rpc.pacific-1.example.com:26657",
				},
			},
		},
		{
			name:    "empty resultExport is rejected",
			re:      &seiv1alpha1.ResultExportConfig{},
			wantErr: "replayer: resultExport is set but has no sub-struct",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := &seiv1alpha1.SeiNode{
				ObjectMeta: metav1.ObjectMeta{Name: "replayer-0", Namespace: "pacific-1"},
				Spec: seiv1alpha1.SeiNodeSpec{
					ChainID: "pacific-1",
					Image:   "seid:v6.4.1",
					Peers: []seiv1alpha1.PeerSource{
						{Static: &seiv1alpha1.StaticPeerSource{Addresses: []string{"peer1@host:26656"}}},
					},
					Replayer: &seiv1alpha1.ReplayerSpec{
						Snapshot: seiv1alpha1.SnapshotSource{
							S3: &seiv1alpha1.S3SnapshotSource{TargetHeight: 100000000},
						},
						ResultExport: tc.re,
					},
				},
			}
			err := (&replayerPlanner{}).Validate(node)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate: expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate: error = %q, want containing %q", err.Error(), tc.wantErr)
			}
		})
	}
}
