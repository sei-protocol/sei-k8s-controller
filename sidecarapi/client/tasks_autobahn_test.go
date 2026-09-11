package client

import (
	"strings"
	"testing"
)

func TestAssembleAndUploadGenesisTask_ValidateAutobahnConfig(t *testing.T) {
	one, zero, overCap := int64(1), int64(0), int64(AutobahnMaxTxsPerBlockCeiling+1)
	cases := []struct {
		name     string
		autobahn bool
		cfg      *AutobahnConfigParams
		wantErr  string
	}{
		{name: "tendermint without config", autobahn: false},
		{name: "autobahn without config", autobahn: true},
		{name: "autobahn empty config", autobahn: true, cfg: &AutobahnConfigParams{}},
		{name: "autobahn full config", autobahn: true, cfg: &AutobahnConfigParams{BlockInterval: "250ms", MaxTxsPerBlock: &one}},
		{name: "config without autobahn", autobahn: false, cfg: &AutobahnConfigParams{}, wantErr: "AutobahnConfig requires Autobahn"},
		{name: "unparseable interval", autobahn: true, cfg: &AutobahnConfigParams{BlockInterval: "fast"}, wantErr: `BlockInterval "fast"`},
		{name: "zero interval", autobahn: true, cfg: &AutobahnConfigParams{BlockInterval: "0s"}, wantErr: "must be positive"},
		{name: "negative interval", autobahn: true, cfg: &AutobahnConfigParams{BlockInterval: "-1s"}, wantErr: "must be positive"},
		{name: "zero max txs", autobahn: true, cfg: &AutobahnConfigParams{MaxTxsPerBlock: &zero}, wantErr: "MaxTxsPerBlock 0"},
		{name: "over-cap max txs", autobahn: true, cfg: &AutobahnConfigParams{MaxTxsPerBlock: &overCap}, wantErr: "MaxTxsPerBlock 2001"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := validNonForkTask(nil)
			task.Autobahn = tc.autobahn
			task.AutobahnConfig = tc.cfg
			err := task.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("expected nil error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error: got %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestAssembleAndUploadGenesisTask_ToTaskRequest_AutobahnConfigOnlyUnderAutobahn(t *testing.T) {
	task := validNonForkTask(nil)
	task.AutobahnConfig = &AutobahnConfigParams{BlockInterval: "1s"}
	if _, present := (*task.ToTaskRequest().Params)["autobahnConfig"]; present {
		t.Fatal("autobahnConfig must not be serialized when Autobahn is false")
	}
	task.Autobahn = true
	if _, present := (*task.ToTaskRequest().Params)["autobahnConfig"]; !present {
		t.Fatal("autobahnConfig missing under Autobahn")
	}
}
