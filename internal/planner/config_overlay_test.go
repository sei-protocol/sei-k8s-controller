package planner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	seiconfig "github.com/sei-protocol/sei-config"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
	"github.com/sei-protocol/sei-k8s-controller/sidecarapi/tomlpatch"
)

const (
	overlayTestFull      = "full"
	overlayTestValidator = "validator"
	overlayTestSeed      = "seed"
	overlayTestAppFile   = "app.toml"
	overlayTestBase      = "base"
	overlayTestStateSync = "state-sync"
	overlayTestArchive   = "archive"
	overlayTestReplayer  = "replayer"
)

func overlayTestNode() *seiv1alpha1.SeiNode {
	n := &seiv1alpha1.SeiNode{}
	for _, v := range []struct{ file, key, raw string }{
		{overlayTestAppFile, "arbitrary.deep.enabled", "true"},
		{overlayTestAppFile, "arbitrary.deep.count", "42"},
		{overlayTestAppFile, "arbitrary.deep.ratio", "1.25"},
		{"config.toml", "custom.label", `"true"`},
	} {
		n.Spec.ConfigValues = append(n.Spec.ConfigValues, seiv1alpha1.ConfigValue{FileName: v.file, Key: v.key, Value: apiextensionsv1.JSON{Raw: []byte(v.raw)}})
	}
	return n
}

// Exercise persisted plan JSON, the patch wire payload, merge, and an actual
// TOML file roundtrip; checking only the pre-serialization map misses SC-008.
func TestConfigValuesTypedTOMLFile(t *testing.T) {
	plan, err := buildBasePlan(overlayTestNode(), nil, &seiconfig.ConfigIntent{})
	if err != nil {
		t.Fatal(err)
	}
	var patch task.ConfigPatchTask
	for _, p := range plan.Tasks {
		if p.Type == TaskConfigPatch {
			if err := json.Unmarshal(p.Params.Raw, &patch); err != nil {
				t.Fatal(err)
			}
		}
	}
	wire, err := json.Marshal(patch.ToTaskRequest().Params)
	if err != nil {
		t.Fatal(err)
	}
	var received task.ConfigPatchTask
	if err := json.Unmarshal(wire, &received); err != nil {
		t.Fatal(err)
	}
	if len(received.Files) != 2 {
		t.Fatalf("files: %#v", received.Files)
	}
	dir := t.TempDir()
	for file, overlay := range received.Files {
		base := map[string]any{"untouched": overlayTestBase}
		merged := tomlpatch.Merge(base, overlay).(map[string]any)
		path := filepath.Join(dir, file)
		if err := tomlpatch.WriteTOML(path, merged); err != nil {
			t.Fatal(err)
		}
		doc, err := tomlpatch.ReadTOML(path)
		if err != nil {
			t.Fatal(err)
		}
		if doc["untouched"] != overlayTestBase {
			t.Fatal("base erased")
		}
		if file == overlayTestAppFile {
			deep := doc["arbitrary"].(map[string]any)["deep"].(map[string]any)
			if deep["enabled"] != true || deep["count"] != int64(42) || deep["ratio"] != float64(1.25) {
				t.Fatalf("wrong TOML types: %#v", deep)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), "enabled = true") {
				t.Fatalf("boolean not unquoted: %s", raw)
			}
			t.Logf("SC-008 actual TOML file:\n%s", raw)
		} else if doc["custom"].(map[string]any)["label"] != "true" {
			t.Fatalf("string type lost: %#v", doc)
		}
	}
}

func TestConfigValuesInitOrdering(t *testing.T) {
	n := overlayTestNode()
	intent := &seiconfig.ConfigIntent{Overrides: map[string]string{"moniker": "legacy"}}
	cases := []struct {
		name   string
		build  func() (*seiv1alpha1.TaskPlan, error)
		stages int
	}{
		{overlayTestBase, func() (*seiv1alpha1.TaskPlan, error) { return buildBasePlan(n, nil, intent) }, 1},
		{overlayTestStateSync, func() (*seiv1alpha1.TaskPlan, error) { return buildBasePlan(n, &seiv1alpha1.SnapshotSource{}, intent) }, 1},
		{"bootstrap", func() (*seiv1alpha1.TaskPlan, error) {
			return buildBootstrapPlan(n, &seiv1alpha1.SnapshotSource{}, intent)
		}, 2},
		{"ceremony", func() (*seiv1alpha1.TaskPlan, error) { return buildGenesisPlan(n) }, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := tc.build()
			if err != nil {
				t.Fatal(err)
			}
			applied, patches := false, 0
			for i, p := range plan.Tasks {
				if p.ID != task.DeterministicTaskID(plan.ID, p.Type, i) {
					t.Fatal("incorrect task index")
				}
				switch p.Type {
				case TaskConfigApply:
					applied = true
				case TaskConfigPatch:
					if !applied || i+1 == len(plan.Tasks) || plan.Tasks[i+1].Type != TaskConfigValidate {
						t.Fatal("overlay not after apply/before validate")
					}
					patches++
					applied = false
				}
			}
			if patches != tc.stages {
				t.Fatalf("patches %d want %d", patches, tc.stages)
			}
		})
	}
	if intent.Overrides["moniker"] != "legacy" || len(intent.Overrides) != 1 {
		t.Fatal("legacy overrides changed")
	}
	n.Spec.ConfigValues = nil
	plan, err := buildBasePlan(n, nil, intent)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range plan.Tasks {
		if p.Type == TaskConfigPatch {
			t.Fatal("empty overlay emitted")
		}
	}
}

func TestConfigValuesInvalidJSON(t *testing.T) {
	n := overlayTestNode()
	n.Spec.ConfigValues[0].Value.Raw = []byte("invalid")
	if _, err := buildBasePlan(n, nil, &seiconfig.ConfigIntent{}); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}

func TestConfigValuesAllModePlanners(t *testing.T) {
	for _, mode := range []string{overlayTestFull, overlayTestArchive, overlayTestValidator, overlayTestSeed, overlayTestReplayer} {
		t.Run(mode, func(t *testing.T) {
			n := overlayTestNode()
			var build func(*seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error)
			switch mode {
			case overlayTestFull:
				n.Spec.FullNode = &seiv1alpha1.FullNodeSpec{}
				build = (&fullNodePlanner{}).BuildPlan
			case overlayTestArchive:
				n.Spec.Archive = &seiv1alpha1.ArchiveSpec{}
				build = (&archiveNodePlanner{}).BuildPlan
			case overlayTestValidator:
				n.Spec.Validator = &seiv1alpha1.ValidatorSpec{}
				build = (&validatorPlanner{}).BuildPlan
			case overlayTestSeed:
				n.Spec.Seed = &seiv1alpha1.SeedSpec{}
				build = (&seedPlanner{}).BuildPlan
			case overlayTestReplayer:
				n.Spec.Replayer = &seiv1alpha1.ReplayerSpec{}
				build = (&replayerPlanner{}).BuildPlan
			}
			plan, err := build(n)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for i, p := range plan.Tasks {
				if p.Type == TaskConfigPatch {
					found = true
					if i == 0 || plan.Tasks[i+1].Type != TaskConfigValidate {
						t.Fatal("incorrect ordering")
					}
				}
			}
			if !found {
				t.Fatal("mode omitted overlay")
			}
		})
	}
}

func TestConfigValuesRejectUnsafePatches(t *testing.T) {
	const (
		parentKey    = "a.b"
		childKey     = "a.b.c"
		peerTable    = "p2p"
		peerCountKey = "p2p.max_num_peers"
		overlapError = "overlapping keys"
	)
	for _, tc := range []struct {
		name string
		keys []string
		raw  []string
		want string
	}{
		{"prefix", []string{parentKey, childKey}, []string{"1", "2"}, overlapError},
		{"reverse-prefix", []string{childKey, parentKey}, []string{"2", "1"}, overlapError},
		{"array-prefix", []string{parentKey, childKey}, []string{"[1,2]", "2"}, overlapError},
		{"colliding-member", []string{peerTable, peerCountKey}, []string{`{"max_num_peers":10}`, "50"}, overlapError},
		{"disjoint-member", []string{peerTable, peerCountKey}, []string{`{"seeds":"a"}`, "50"}, ""},
		{"nested-collision", []string{peerTable, "p2p.options"}, []string{`{"options":{"count":10}}`, `{"count":50}`}, overlapError},
		{"nested-disjoint", []string{peerTable, "p2p.options"}, []string{`{"options":{"seeds":"a"}}`, `{"count":50}`}, ""},
		{"empty-table", []string{peerTable, peerCountKey}, []string{`{}`, "50"}, ""},
		{"nested-null", []string{parentKey}, []string{`{"child":{"value":null}}`}, "null values"},
		{"array-null", []string{parentKey}, []string{`[{"child":null}]`}, "null values"},
		{"number-overflow", []string{parentKey}, []string{`1e400`}, "int64 or float64"},
		{"nested-number-overflow", []string{parentKey}, []string{`{"child":[1e400]}`}, "int64 or float64"},
		{"integer-float-fallback", []string{parentKey}, []string{`9223372036854775808`}, ""},
		{"empty-json", []string{parentKey}, []string{""}, "EOF"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var first map[string]map[string]any
			for _, reverse := range []bool{false, true} {
				n := &seiv1alpha1.SeiNode{}
				for j := range tc.keys {
					i := j
					if reverse {
						i = len(tc.keys) - 1 - j
					}
					n.Spec.ConfigValues = append(n.Spec.ConfigValues, seiv1alpha1.ConfigValue{
						FileName: overlayTestAppFile, Key: tc.keys[i], Value: apiextensionsv1.JSON{Raw: []byte(tc.raw[i])},
					})
				}
				plan, err := buildBasePlan(n, nil, &seiconfig.ConfigIntent{})
				if tc.want == "" {
					if err != nil || plan == nil {
						t.Fatalf("valid overlay rejected: %v", err)
					}
					patch, err := configValuesOverlay(n.Spec.ConfigValues)
					if err != nil {
						t.Fatal(err)
					}
					if reverse && !reflect.DeepEqual(first, patch.Files) {
						t.Fatalf("order-dependent: %v versus %v", first, patch.Files)
					}
					first = patch.Files
					if tc.name == "disjoint-member" {
						want := map[string]any{"seeds": "a", "max_num_peers": json.Number("50")}
						if !reflect.DeepEqual(patch.Files[overlayTestAppFile][peerTable], want) {
							t.Fatalf("lost disjoint member: %v", patch.Files)
						}
					}
					continue
				}
				if err == nil || plan != nil {
					t.Fatalf("unsafe overlay produced plan: %v, %v", plan, err)
				}
				for _, want := range append(tc.keys, overlayTestAppFile, tc.want) {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q missing %q", err, want)
					}
				}
			}
		})
	}
}

func TestConfigValuesFileNameValidation(t *testing.T) {
	for _, name := range []string{"../app.toml", "/app.toml", "sub/app.toml", `sub\app.toml`, "app.json", "app.toml\n", ""} {
		t.Run(name, func(t *testing.T) {
			n := overlayTestNode()
			n.Spec.ConfigValues[0].FileName = name
			if _, err := configValuesOverlay(n.Spec.ConfigValues); err == nil || !strings.Contains(err.Error(), "fileName must match") || !strings.Contains(err.Error(), n.Spec.ConfigValues[0].Key) {
				t.Fatalf("invalid file name accepted or missing context: %v", err)
			}
		})
	}
}

func TestConfigValuesMissingValidationTask(t *testing.T) {
	plan := &seiv1alpha1.TaskPlan{Tasks: []seiv1alpha1.PlannedTask{{Type: TaskConfigApply}, {Type: TaskConfigPatch}}}
	if got, err := withConfigValues(plan, overlayTestNode()); err == nil || got != nil || !strings.Contains(err.Error(), "no config-validate") {
		t.Fatalf("missing validation accepted: %v, %v", got, err)
	}
	if len(plan.Tasks) != 2 {
		t.Fatal("failed splice mutated plan")
	}
	n := overlayTestNode()
	n.Spec.ConfigValues = nil
	if got, err := withConfigValues(plan, n); err != nil || got != plan {
		t.Fatalf("empty overlay rejected: %v", err)
	}
}

func TestConfigValuesOverridesPrecedenceOnDisk(t *testing.T) {
	n := &seiv1alpha1.SeiNode{}
	n.Spec.FullNode = &seiv1alpha1.FullNodeSpec{}
	n.Spec.Overrides = map[string]string{"evm.http_port": "9545"}
	n.Spec.ConfigValues = []seiv1alpha1.ConfigValue{{
		FileName: overlayTestAppFile, Key: "evm.http_port",
		Value: apiextensionsv1.JSON{Raw: []byte("10545")},
	}}
	plan, err := (&fullNodePlanner{}).BuildPlan(n)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	path := filepath.Join(home, "config", overlayTestAppFile)
	applied := false
	for _, planned := range plan.Tasks {
		switch planned.Type {
		case TaskConfigApply:
			var intent seiconfig.ConfigIntent
			if err := json.Unmarshal(planned.Params.Raw, &intent); err != nil {
				t.Fatal(err)
			}
			result, err := seiconfig.ResolveIntent(intent)
			if err != nil {
				t.Fatal(err)
			}
			if !result.Valid {
				t.Fatalf("invalid intent: %v", result.Diagnostics)
			}
			if err := seiconfig.WriteConfigToDir(result.Config, home); err != nil {
				t.Fatal(err)
			}
			doc, err := tomlpatch.ReadTOML(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := doc["evm"].(map[string]any)["http_port"]; got != int64(9545) {
				t.Fatalf("Overrides not applied: %v", got)
			}
		case TaskConfigPatch:
			var patch task.ConfigPatchTask
			if err := json.Unmarshal(planned.Params.Raw, &patch); err != nil {
				t.Fatal(err)
			}
			doc, err := tomlpatch.ReadTOML(path)
			if err != nil {
				t.Fatal(err)
			}
			merged := tomlpatch.Merge(doc, patch.Files[overlayTestAppFile]).(map[string]any)
			if err := tomlpatch.WriteTOML(path, merged); err != nil {
				t.Fatal(err)
			}
			doc, err = tomlpatch.ReadTOML(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := doc["evm"].(map[string]any)["http_port"]; got != int64(10545) {
				t.Fatalf("configValues did not win on disk: %v", got)
			}
			applied = true
		}
	}
	if !applied {
		t.Fatal("overlay was not applied")
	}
}
