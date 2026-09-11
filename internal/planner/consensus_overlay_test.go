package planner

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	seiconfig "github.com/sei-protocol/sei-config"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

func consensusTestNode(engine seiv1alpha1.ConsensusEngine, evmOnly bool) *seiv1alpha1.SeiNode {
	n := &seiv1alpha1.SeiNode{}
	n.Spec.FullNode = &seiv1alpha1.FullNodeSpec{}
	if engine != "" || evmOnly {
		n.Spec.Consensus = &seiv1alpha1.ConsensusSpec{Engine: engine, EvmOnly: evmOnly}
	}
	return n
}

func overlayPatches(t *testing.T, plan *seiv1alpha1.TaskPlan) []task.ConfigPatchTask {
	t.Helper()
	var patches []task.ConfigPatchTask
	for _, p := range plan.Tasks {
		if p.Type != TaskConfigPatch {
			continue
		}
		var patch task.ConfigPatchTask
		if err := json.Unmarshal(p.Params.Raw, &patch); err != nil {
			t.Fatal(err)
		}
		patches = append(patches, patch)
	}
	return patches
}

func TestConsensusOverlay(t *testing.T) {
	tests := []struct {
		name   string
		node   *seiv1alpha1.SeiNode
		expect map[string]map[string]any
	}{
		{name: "absent is Tendermint", node: consensusTestNode("", false)},
		{name: "explicit Tendermint", node: consensusTestNode(seiv1alpha1.ConsensusEngineTendermint, false)},
		{
			name: "Autobahn points config.toml at the ceremony artifact",
			node: consensusTestNode(seiv1alpha1.ConsensusEngineAutobahn, false),
			expect: map[string]map[string]any{
				consensusConfigFile: {keyAutobahnConfigFile: AutobahnConfigPath},
			},
		},
		{
			name: "EVM-only also closes CometBFT RPC, REST, gRPC and gRPC-web",
			node: consensusTestNode(seiv1alpha1.ConsensusEngineAutobahn, true),
			expect: map[string]map[string]any{
				consensusConfigFile: {
					keyAutobahnConfigFile: AutobahnConfigPath,
					"evm-only":            true,
					"rpc":                 map[string]any{"laddr": ""},
				},
				consensusAppFile: {
					"api":      map[string]any{keyEnable: false},
					"grpc":     map[string]any{keyEnable: false},
					"grpc-web": map[string]any{keyEnable: false},
				},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := consensusOverlay(tc.node)
			if tc.expect == nil {
				if got != nil {
					t.Fatalf("expected no overlay, got %v", got.Files)
				}
				return
			}
			if got == nil || !reflect.DeepEqual(got.Files, tc.expect) {
				t.Fatalf("overlay mismatch\n got: %#v\nwant: %#v", got, tc.expect)
			}
		})
	}
}

// The engine keys ride the same splice as configValues, after base
// regeneration and before every config-validate, with the controller winning.
func TestConsensusOverlaySplicedWithConfigValues(t *testing.T) {
	node := consensusTestNode(seiv1alpha1.ConsensusEngineAutobahn, true)
	node.Spec.ConfigValues = []seiv1alpha1.ConfigValue{
		{FileName: consensusAppFile, Key: "api.address", Value: apiextensionsv1.JSON{Raw: []byte(`"tcp://0.0.0.0:1317"`)}},
		{FileName: consensusConfigFile, Key: "rpc.max_open_connections", Value: apiextensionsv1.JSON{Raw: []byte(`900`)}},
	}
	plan, err := buildBasePlan(node, nil, &seiconfig.ConfigIntent{})
	if err != nil {
		t.Fatal(err)
	}
	patches := overlayPatches(t, plan)
	if len(patches) == 0 {
		t.Fatal("expected a config-patch task carrying the overlay")
	}
	for _, patch := range patches {
		api, _ := patch.Files[consensusAppFile]["api"].(map[string]any)
		if api[keyEnable] != false || api["address"] != "tcp://0.0.0.0:1317" {
			t.Fatalf("app.toml api table not merged: %#v", api)
		}
		rpc, _ := patch.Files[consensusConfigFile]["rpc"].(map[string]any)
		if rpc["laddr"] != "" || rpc["max_open_connections"] == nil {
			t.Fatalf("config.toml rpc table not merged: %#v", rpc)
		}
		if patch.Files[consensusConfigFile]["evm-only"] != true {
			t.Fatalf("evm-only missing: %#v", patch.Files[consensusConfigFile])
		}
	}
}

func TestConsensusOverlayWithoutConfigValuesStillSpliced(t *testing.T) {
	node := consensusTestNode(seiv1alpha1.ConsensusEngineAutobahn, false)
	plan, err := buildBasePlan(node, nil, &seiconfig.ConfigIntent{})
	if err != nil {
		t.Fatal(err)
	}
	patches := overlayPatches(t, plan)
	if len(patches) == 0 {
		t.Fatal("Autobahn without configValues must still splice the engine overlay")
	}
	if patches[0].Files[consensusConfigFile][keyAutobahnConfigFile] != AutobahnConfigPath {
		t.Fatalf("unexpected patch: %#v", patches[0].Files)
	}

	tendermint, err := buildBasePlan(consensusTestNode("", false), nil, &seiconfig.ConfigIntent{})
	if err != nil {
		t.Fatal(err)
	}
	if got := overlayPatches(t, tendermint); len(got) != 0 {
		t.Fatalf("Tendermint without configValues must not splice a patch: %v", got)
	}
}

func TestConsensusOverlayRejectsUserValueOnControllerKey(t *testing.T) {
	for name, entry := range map[string]seiv1alpha1.ConfigValue{
		"top-level": {FileName: consensusConfigFile, Key: keyAutobahnConfigFile, Value: apiextensionsv1.JSON{Raw: []byte(`"/tmp/x.json"`)}},
		"nested":    {FileName: consensusAppFile, Key: "grpc.enable", Value: apiextensionsv1.JSON{Raw: []byte(`true`)}},
		"table":     {FileName: consensusConfigFile, Key: "rpc", Value: apiextensionsv1.JSON{Raw: []byte(`{"laddr":"tcp://0.0.0.0:26657"}`)}},
	} {
		t.Run(name, func(t *testing.T) {
			node := consensusTestNode(seiv1alpha1.ConsensusEngineAutobahn, true)
			node.Spec.ConfigValues = []seiv1alpha1.ConfigValue{entry}
			_, err := buildBasePlan(node, nil, &seiconfig.ConfigIntent{})
			if err == nil || !strings.Contains(err.Error(), "spec.consensus") {
				t.Fatalf("want a spec.consensus ownership error, got %v", err)
			}
		})
	}
}
