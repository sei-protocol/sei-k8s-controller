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
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
	sidecar "github.com/sei-protocol/sei-k8s-controller/sidecarapi/client"
	"github.com/sei-protocol/sei-k8s-controller/sidecarapi/wire"
)

const (
	configUpdateSidecar = "sidecar"
	configUpdateImage   = "image"
	configUpdateOnly    = "config"
	configUpdateBoth    = "both"
	configUpdateFile    = "config.toml"
	configUpdateOldHash = "old"
)

func TestConfigValuesEmptyObservedHashDoesNotRestartFleetOnControllerUpgrade(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Status.CurrentConfigValuesHash = ""
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
		for _, drift := range []string{configUpdateOnly, configUpdateImage, configUpdateBoth, configUpdateSidecar} {
			t.Run(mode.name+"/"+drift, func(t *testing.T) {
				g := NewWithT(t)
				node := runningFullNode()
				node.Spec.FullNode = nil
				mode.configure(node)
				node.Spec.ConfigValues = []seiv1alpha1.ConfigValue{{
					FileName: configUpdateFile, Key: "p2p", Value: apiextensionsv1.JSON{Raw: []byte(`{"external-address":"pinned:26656","persistent-peers":"pinned-peer"}`)},
				}}
				node.Status.CurrentConfigValuesHash = "previous"
				if drift == configUpdateImage || drift == configUpdateSidecar {
					node.Status.CurrentConfigValuesHash, _ = configValuesHash(node.Spec.ConfigValues)
				}
				if drift == configUpdateImage || drift == configUpdateBoth {
					node.Spec.Image = testImageV2
				}
				resolver := &NodeResolver{}
				if drift == configUpdateSidecar {
					resolver.Platform = platformWithSidecar(testSidecarImageV2)
					node.Status.CurrentSidecarImage = testSidecarImageV1
				}
				planner, err := resolver.plannerForMode(node)
				g.Expect(err).NotTo(HaveOccurred())
				plan, err := planner.BuildPlan(node)
				g.Expect(err).NotTo(HaveOccurred())
				types := planTaskTypes(plan)
				if drift == configUpdateOnly {
					g.Expect(types).To(Equal([]string{TaskConfigApply, TaskConfigPatch, TaskConfigPatch,
						TaskConfigValidate, TaskMarkReady, sidecar.TaskTypeRestartSeid}))
				} else {
					g.Expect(types).NotTo(ContainElement(sidecar.TaskTypeRestartSeid))
					g.Expect(types).To(ContainElement(task.TaskTypeReplacePod))
				}
				g.Expect(slices.Contains(types, TaskConfigApply)).To(Equal(drift == configUpdateOnly || drift == configUpdateBoth))
				validate := slices.Index(types, TaskConfigValidate)
				g.Expect(types[validate-2 : validate]).To(Equal([]string{TaskConfigPatch, TaskConfigPatch}))
				var patch task.ConfigPatchTask
				g.Expect(json.Unmarshal(plan.Tasks[validate-1].Params.Raw, &patch)).To(Succeed())
				g.Expect(patch.Files[configUpdateFile]["p2p"]).To(Equal(map[string]any{
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
	node.Status.CurrentConfigValuesHash = configUpdateOldHash
	node.Spec.ConfigValues = overlayTestNode().Spec.ConfigValues
	resolver := &NodeResolver{}
	ctx := context.Background()
	g.Expect(resolver.ResolvePlan(ctx, node)).To(Succeed())
	plan := node.Status.Plan
	g.Expect(plan).NotTo(BeNil())
	// Execute the real registry/executor against completed sidecar task results.
	sc := &mockSidecarClient{activeResults: make(map[uuid.UUID]*sidecar.TaskResult)}
	for _, pt := range plan.Tasks {
		sc.activeResults[uuid.MustParse(pt.ID)] = &sidecar.TaskResult{Status: sidecar.Completed}
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
			node.Status.CurrentConfigValuesHash = configUpdateOldHash
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
				g.Expect(node.Status.CurrentConfigValuesHash).To(Equal(configUpdateOldHash))
			} else {
				g.Expect(node.Status.CurrentConfigValuesHash).To(Equal(plan.ConfigValuesHash))
			}
			g.Expect(configValuesDrifted(node)).To(BeTrue())
		})
	}
}

func TestConfigValuesHashCanonicalNumericJSON(t *testing.T) {
	for _, variants := range [][]string{
		{"1", "1.00", "10e-1", "0.1e+1"},
		{"0", "-0.0", "0e999999999"},
		{"9007199254740993", "9007199254740993.000", "90071992547409930e-1"},
		{"-0.0123", "-123e-4", "-1.2300E-2"},
	} {
		t.Run(variants[0], func(t *testing.T) {
			g := NewWithT(t)
			var want string
			for _, raw := range variants {
				values := []seiv1alpha1.ConfigValue{{FileName: configUpdateFile, Key: "custom",
					Value: apiextensionsv1.JSON{Raw: []byte(`{"nested":[` + raw + `]}`)},
				}}
				got, err := configValuesHash(values)
				g.Expect(err).NotTo(HaveOccurred())
				if want == "" {
					want = got
				}
				g.Expect(got).To(Equal(want), raw)
			}
		})
	}
}

func TestConfigValuesRemovalToEmptyStillRegeneratesBaseAndRestarts(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	var err error
	node.Status.CurrentConfigValuesHash, err = configValuesHash(overlayTestNode().Spec.ConfigValues)
	g.Expect(err).NotTo(HaveOccurred())
	plan, err := (&fullNodePlanner{}).BuildPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(planTaskTypes(plan)).To(Equal([]string{
		TaskConfigApply, TaskConfigPatch, TaskConfigValidate, TaskMarkReady, sidecar.TaskTypeRestartSeid,
	}))
	emptyHash, err := configValuesHash(nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan.ConfigValuesHash).To(Equal(emptyHash))
}

func TestConfigUpdateWaitsForRestartBeforeObserving(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	node := runningFullNode()
	node.Status.CurrentConfigValuesHash = configUpdateOldHash
	plan, err := buildConfigUpdatePlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	var restartID uuid.UUID
	for i := range plan.Tasks {
		if plan.Tasks[i].Type == sidecar.TaskTypeRestartSeid {
			restartID = uuid.MustParse(plan.Tasks[i].ID)
			plan.Tasks[i].Status = seiv1alpha1.TaskRunning
			break
		}
		plan.Tasks[i].Status = seiv1alpha1.TaskComplete
	}
	sc := &mockSidecarClient{activeResults: map[uuid.UUID]*sidecar.TaskResult{
		restartID: {Status: sidecar.Running},
	}}
	cfg := task.ExecutionConfig{BuildSidecarClient: func() (task.SidecarClient, error) { return sc, nil }}
	_, err = executePlan(ctx, node, plan, cfg)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan.Phase).To(Equal(seiv1alpha1.TaskPlanActive))
	g.Expect(node.Status.CurrentConfigValuesHash).To(Equal(configUpdateOldHash))
	sc.activeResults[restartID].Status = sidecar.Completed
	_, err = executePlan(ctx, node, plan, cfg)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan.Phase).To(Equal(seiv1alpha1.TaskPlanComplete))
	g.Expect(node.Status.CurrentConfigValuesHash).To(Equal(plan.ConfigValuesHash))
}

// The restart-seid request names the node's up-check so the sidecar waits on
// the listener the readiness probe reads, not on a hard-coded /status.
func TestConfigUpdateRestartCarriesUpCheck(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Status.CurrentConfigValuesHash = configUpdateOldHash
	plan, err := buildConfigUpdatePlan(node)
	g.Expect(err).NotTo(HaveOccurred())

	var params struct {
		UpCheck wire.UpCheck `json:"upCheck"`
	}
	g.Expect(json.Unmarshal(taskParams(t, plan, sidecar.TaskTypeRestartSeid), &params)).To(Succeed())
	g.Expect(params.UpCheck).To(Equal(noderesource.UpCheckForNode(node)))
	g.Expect(params.UpCheck).To(Equal(wire.UpCheck{Scheme: wire.UpCheckHTTP, Port: seiconfig.PortRPC, Path: "/status"}))
}
