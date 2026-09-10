package planner

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	. "github.com/onsi/gomega"
	seiconfig "github.com/sei-protocol/sei-config"
	"k8s.io/apimachinery/pkg/api/meta"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
	"github.com/sei-protocol/sei-k8s-controller/sidecarapi/tomlpatch"
)

func migrationModeSpecs() map[string]seiv1alpha1.SeiNodeSpec {
	return map[string]seiv1alpha1.SeiNodeSpec{
		overlayTestFull:      {FullNode: &seiv1alpha1.FullNodeSpec{}},
		overlayTestArchive:   {Archive: &seiv1alpha1.ArchiveSpec{}},
		overlayTestValidator: {Validator: &seiv1alpha1.ValidatorSpec{}},
		overlayTestSeed:      {Seed: &seiv1alpha1.SeedSpec{}},
		overlayTestReplayer:  {Replayer: &seiv1alpha1.ReplayerSpec{}},
	}
}

func TestRunningConfigIntentMatchesPlannerMode(t *testing.T) {
	for name, spec := range migrationModeSpecs() {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			node := &seiv1alpha1.SeiNode{Spec: spec}
			mode, err := (&NodeResolver{}).plannerForMode(node)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(runningConfigIntent(node).Mode)).To(Equal(mode.Mode()))
			// Replayer deliberately uses full defaults, just like its init intent;
			// its controller overrides only tune state-commit buffers and retention.
			if name == overlayTestReplayer {
				g.Expect(runningConfigIntent(node).Mode).To(Equal(seiconfig.ModeFull))
			}
		})
	}
}

func TestB1RunningPlanRevertsGigaStoreMigration(t *testing.T) {
	// Known defect B1 (docs/notes/b1-severity.md): pin the REVERT, not a remedy.
	// Only full nodes are workflow-eligible. The other rows measure regeneration
	// defensively; literal expectations catch changes to sibling default flags.
	expected := map[string][]any{
		overlayTestFull:      {true, true, backendPebble, true},
		overlayTestArchive:   {true, nil, backendPebble, true},
		overlayTestValidator: {false, nil, backendPebble, true},
		overlayTestSeed:      {false, nil, backendPebble, true},
		overlayTestReplayer:  {true, true, backendPebble, true},
	}
	for name, spec := range migrationModeSpecs() {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			node := runningFullNode()
			spec.ChainID, spec.Image = node.Spec.ChainID, node.Spec.Image
			node.Spec = spec
			mode, err := (&NodeResolver{}).plannerForMode(node)
			g.Expect(err).NotTo(HaveOccurred())
			home := t.TempDir()
			t.Setenv("HOME", home)
			writeMigrationIntent(t, home, *runningConfigIntent(node))
			migration := gigaStoreConfigPatch(&seiv1alpha1.GigaStoreMigration{Backend: "rocksdb"})
			// Restore migration keys before each trigger to prove the defect recurs,
			// rather than merely observing the previous regeneration's values.
			for _, trigger := range []string{"first-image-roll", "config-edit", "config-removal"} {
				t.Run(trigger, func(t *testing.T) {
					g := NewWithT(t)
					patchMigrationFiles(t, home, migration)
					g.Expect(readMigrationKeys(t, home)).To(Equal([]any{true, true, "rocksdb", true}))
					switch trigger {
					case "first-image-roll":
						node.Status.CurrentConfigValuesHash = ""
						node.Spec.Image = testImageV2
					case "config-edit":
						node.Status.CurrentImage = node.Spec.Image
						node.Status.CurrentConfigValuesHash, err = configValuesHash(nil)
						g.Expect(err).NotTo(HaveOccurred())
						node.Spec.ConfigValues = overlayTestNode().Spec.ConfigValues
					case "config-removal":
						node.Status.CurrentConfigValuesHash, err = configValuesHash(node.Spec.ConfigValues)
						g.Expect(err).NotTo(HaveOccurred())
						node.Spec.ConfigValues = nil
					}
					plan, err := mode.BuildPlan(node)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(plan).NotTo(BeNil())
					raw, err := json.Marshal(plan)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(json.Unmarshal(raw, &plan)).To(Succeed())
					applies := 0
					for _, pt := range plan.Tasks {
						switch pt.Type {
						case TaskConfigApply:
							var intent seiconfig.ConfigIntent
							g.Expect(json.Unmarshal(pt.Params.Raw, &intent)).To(Succeed())
							writeMigrationIntent(t, home, intent)
							applies++
						case TaskConfigPatch:
							g.Expect(applies).To(Equal(1))
							var patch task.ConfigPatchTask
							g.Expect(json.Unmarshal(pt.Params.Raw, &patch)).To(Succeed())
							patchMigrationFiles(t, home, patch.Files)
						}
					}
					g.Expect(applies).To(Equal(1))
					g.Expect(readMigrationKeys(t, home)).To(Equal(expected[name]))
				})
			}
		})
	}
}

func writeMigrationIntent(t *testing.T, home string, intent seiconfig.ConfigIntent) {
	t.Helper()
	g := NewWithT(t)
	resolved, err := seiconfig.ResolveIntent(intent)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(resolved.Valid).To(BeTrue())
	g.Expect(seiconfig.WriteConfigToDir(resolved.Config, home)).To(Succeed())
}

func patchMigrationFiles(t *testing.T, home string, files map[string]map[string]any) {
	t.Helper()
	g := NewWithT(t)
	for file, values := range files {
		path := filepath.Join(home, "config", file)
		base, err := tomlpatch.ReadTOML(path)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(tomlpatch.WriteTOML(path, tomlpatch.Merge(base, values).(map[string]any))).To(Succeed())
	}
}

func readMigrationKeys(t *testing.T, home string) []any {
	t.Helper()
	g := NewWithT(t)
	app, err := tomlpatch.ReadTOML(filepath.Join(home, "config", "app.toml"))
	g.Expect(err).NotTo(HaveOccurred())
	ss := app["state-store"].(map[string]any)
	sc := app["state-commit"].(map[string]any)
	return []any{ss["ss-enable"], ss["evm-ss-split"], ss["ss-backend"], sc["sc-enable"]}
}

func TestUnobservedConfigNonRunningPhasesNeverGetBaselineNotice(t *testing.T) {
	for _, phase := range []seiv1alpha1.SeiNodePhase{
		seiv1alpha1.PhasePending, seiv1alpha1.PhaseInitializing, seiv1alpha1.PhaseFailed,
	} {
		t.Run(string(phase), func(t *testing.T) {
			g := NewWithT(t)
			node := runningFullNode()
			node.Status.CurrentConfigValuesHash = ""
			node.Spec.ConfigValues = overlayTestNode().Spec.ConfigValues
			// Positive control isolates phase as the only changed gate input.
			g.Expect(shouldExplainUnobservedConfig(node)).To(BeTrue())
			node.Status.Phase = phase
			g.Expect(shouldExplainUnobservedConfig(node)).To(BeFalse())
			g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
			condition := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
			if condition != nil {
				g.Expect(condition.Reason).NotTo(Equal("ConfigBaselineUnobserved"))
			}
			// Non-Running nodes take the initialization plan path, not the no-op path.
			g.Expect(node.Status.Plan).NotTo(BeNil())
		})
	}
}
