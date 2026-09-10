package planner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	seiconfig "github.com/sei-protocol/sei-config"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
	"github.com/sei-protocol/sei-k8s-controller/sidecarapi/tomlpatch"
)

const (
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
	for _, mode := range []string{"full", overlayTestArchive, "validator", "seed", overlayTestReplayer} {
		t.Run(mode, func(t *testing.T) {
			n := overlayTestNode()
			var build func(*seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error)
			switch mode {
			case "full":
				n.Spec.FullNode = &seiv1alpha1.FullNodeSpec{}
				build = (&fullNodePlanner{}).BuildPlan
			case overlayTestArchive:
				n.Spec.Archive = &seiv1alpha1.ArchiveSpec{}
				build = (&archiveNodePlanner{}).BuildPlan
			case "validator":
				n.Spec.Validator = &seiv1alpha1.ValidatorSpec{}
				build = (&validatorPlanner{}).BuildPlan
			case "seed":
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
