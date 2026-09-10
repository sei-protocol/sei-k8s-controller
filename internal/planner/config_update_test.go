package planner

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/uuid"
	. "github.com/onsi/gomega"
	seiconfig "github.com/sei-protocol/sei-config"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
	sidecar "github.com/sei-protocol/sei-k8s-controller/sidecarapi/client"
)

func TestConfigValuesEmptyObservedHashDoesNotRestartFleetOnControllerUpgrade(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Spec.ConfigValues = overlayTestNode().Spec.ConfigValues
	g.Expect(configValuesDrifted(node)).To(BeFalse())
	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	g.Expect(node.Status.Plan).To(BeNil())
	g.Expect(node.Status.CurrentConfigValuesHash).To(BeEmpty())
}

func TestConfigValuesHashCanonicalOrderAndPrecision(t *testing.T) {
	g := NewWithT(t)
	values := overlayTestNode().Spec.ConfigValues
	values[0].Value.Raw = []byte(`{"b": [2, 3], "a": 9007199254740993}`)
	want, err := configValuesHash(values)
	g.Expect(err).NotTo(HaveOccurred())
	values[0].Value.Raw = []byte(`{ "a":9007199254740993,"b":[2,3] }`)
	slices.Reverse(values)
	for range 100 {
		got, err := configValuesHash(values)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(got).To(Equal(want))
	}
	values[len(values)-1].Value.Raw = []byte(`{"a":9007199254740992,"b":[2,3]}`)
	got, err := configValuesHash(values)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).NotTo(Equal(want))
	empty, err := configValuesHash(nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(empty).NotTo(BeEmpty())
}

func TestConfigUpdateAllModesOrderingAndPeerPrecedence(t *testing.T) {
	modes := []struct {
		name      string
		configure func(*seiv1alpha1.SeiNode)
	}{
		{"full", func(n *seiv1alpha1.SeiNode) { n.Spec.FullNode = &seiv1alpha1.FullNodeSpec{} }},
		{"archive", func(n *seiv1alpha1.SeiNode) { n.Spec.Archive = &seiv1alpha1.ArchiveSpec{} }},
		{"validator", func(n *seiv1alpha1.SeiNode) { n.Spec.Validator = &seiv1alpha1.ValidatorSpec{} }},
		{"seed", func(n *seiv1alpha1.SeiNode) { n.Spec.Seed = &seiv1alpha1.SeedSpec{} }},
		{"replayer", func(n *seiv1alpha1.SeiNode) { n.Spec.Replayer = &seiv1alpha1.ReplayerSpec{} }},
	}
	for _, mode := range modes {
		for _, drift := range []string{"config", "image", "both", "sidecar"} {
			t.Run(mode.name+"/"+drift, func(t *testing.T) {
				g := NewWithT(t)
				node := runningFullNode()
				node.Spec.FullNode = nil
				mode.configure(node)
				node.Spec.ConfigValues = []seiv1alpha1.ConfigValue{{
					FileName: "config.toml", Key: "p2p", Value: apiextensionsv1.JSON{Raw: []byte(`{"external-address":"pinned:26656","persistent-peers":"pinned-peer"}`)},
				}}
				node.Status.CurrentConfigValuesHash = "previous"
				if drift == "image" || drift == "sidecar" {
					node.Status.CurrentConfigValuesHash, _ = configValuesHash(node.Spec.ConfigValues)
				}
				if drift == "image" || drift == "both" {
					node.Spec.Image = testImageV2
				}
				resolver := &NodeResolver{}
				if drift == "sidecar" {
					resolver.Platform = platformWithSidecar(testSidecarImageV2)
					node.Status.CurrentSidecarImage = testSidecarImageV1
				}
				planner, err := resolver.plannerForMode(node)
				g.Expect(err).NotTo(HaveOccurred())
				plan, err := planner.BuildPlan(node)
				g.Expect(err).NotTo(HaveOccurred())
				types := planTaskTypes(plan)
				if drift == "config" {
					g.Expect(types).To(Equal([]string{TaskConfigApply, TaskConfigPatch, TaskConfigPatch,
						TaskConfigValidate, sidecar.TaskTypeRestartSeid, TaskMarkReady}))
				} else {
					g.Expect(types).NotTo(ContainElement(sidecar.TaskTypeRestartSeid))
					g.Expect(types).To(ContainElement(task.TaskTypeReplacePod))
				}
				g.Expect(slices.Contains(types, TaskConfigApply)).To(Equal(drift == "config" || drift == "both"))
				validate := slices.Index(types, TaskConfigValidate)
				g.Expect(types[validate-2 : validate]).To(Equal([]string{TaskConfigPatch, TaskConfigPatch}))
				var patch task.ConfigPatchTask
				g.Expect(json.Unmarshal(plan.Tasks[validate-1].Params.Raw, &patch)).To(Succeed())
				g.Expect(patch.Files["config.toml"]["p2p"]).To(Equal(map[string]any{
					"external-address": "pinned:26656", "persistent-peers": "pinned-peer",
				}))
				g.Expect(plan.ConfigValuesHash).NotTo(BeEmpty())
				for i, pt := range plan.Tasks {
					g.Expect(pt.ID).To(Equal(task.DeterministicTaskID(plan.ID, pt.Type, i)))
					if pt.Type == TaskConfigApply {
						var intent seiconfig.ConfigIntent
						g.Expect(json.Unmarshal(pt.Params.Raw, &intent)).To(Succeed())
						g.Expect(&intent).To(Equal(runningConfigIntent(node)))
					}
				}
			})
		}
	}
}

func TestConfigUpdateReconcileTwiceHasNoRestartLoop(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Status.CurrentConfigValuesHash = "old"
	node.Spec.ConfigValues = overlayTestNode().Spec.ConfigValues
	resolver := &NodeResolver{}
	ctx := context.Background()
	g.Expect(resolver.ResolvePlan(ctx, node)).To(Succeed())
	plan := node.Status.Plan
	g.Expect(plan).NotTo(BeNil())
	// Execute the real registry/executor against completed sidecar task results.
	sc := &mockSidecarClient{activeResults: make(map[uuid.UUID]*sidecar.TaskResult)}
	for _, pt := range plan.Tasks {
		sc.activeResults[uuid.MustParse(pt.ID)] = &sidecar.TaskResult{Status: sidecar.TaskStatusCompleted}
	}
	cfg := task.ExecutionConfig{BuildSidecarClient: func() (task.SidecarClient, error) { return sc, nil }}
	_, err := executePlan(ctx, node, plan, cfg)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan.Phase).To(Equal(seiv1alpha1.TaskPlanComplete))
	g.Expect(node.Status.CurrentConfigValuesHash).To(Equal(plan.ConfigValuesHash))
	g.Expect(resolver.ResolvePlan(ctx, node)).To(Succeed())
	g.Expect(node.Status.Plan).To(BeNil())
	g.Expect(configValuesDrifted(node)).To(BeFalse())
}

func TestConfigUpdateCompletionObservesCapturedSpecAndFailureDoesNotObserve(t *testing.T) {
	for _, phase := range []seiv1alpha1.TaskPlanPhase{seiv1alpha1.TaskPlanActive, seiv1alpha1.TaskPlanFailed} {
		t.Run(string(phase), func(t *testing.T) {
			g := NewWithT(t)
			node := runningFullNode()
			node.Status.CurrentConfigValuesHash = "old"
			node.Spec.ConfigValues = overlayTestNode().Spec.ConfigValues
			plan, err := buildConfigUpdatePlan(node)
			g.Expect(err).NotTo(HaveOccurred())
			for i := range plan.Tasks {
				plan.Tasks[i].Status = seiv1alpha1.TaskComplete
			}
			plan.Phase = phase
			node.Spec.ConfigValues = nil
			_, err = executePlan(context.Background(), node, plan, task.ExecutionConfig{})
			g.Expect(err).NotTo(HaveOccurred())
			if phase == seiv1alpha1.TaskPlanFailed {
				g.Expect(node.Status.CurrentConfigValuesHash).To(Equal("old"))
			} else {
				g.Expect(node.Status.CurrentConfigValuesHash).To(Equal(plan.ConfigValuesHash))
			}
			g.Expect(configValuesDrifted(node)).To(BeTrue())
		})
	}
}
