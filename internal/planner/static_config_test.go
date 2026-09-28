package planner

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const (
	staticConfigMapName = "rpc-config-v1"
	staticTestNamespace = "default"
	staticTestChainID   = "atlantic-2"
	staticTestImage     = "sei:v1.0.0"
)

func withNodeConfig(node *seiv1alpha1.SeiNode) *seiv1alpha1.SeiNode {
	node.Spec.NodeConfig = &seiv1alpha1.NodeConfig{
		ConfigRef: seiv1alpha1.ConfigFileRef{Name: staticConfigMapName},
		AppRef:    seiv1alpha1.ConfigFileRef{Name: staticConfigMapName},
	}
	return node
}

// pendingNode is an un-provisioned node in the given mode, ready for an init plan.
func pendingNode(configure func(*seiv1alpha1.SeiNode)) *seiv1alpha1.SeiNode {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testNodeName, Namespace: staticTestNamespace, Generation: 1},
		Spec:       seiv1alpha1.SeiNodeSpec{ChainID: staticTestChainID, Image: staticTestImage},
		Status:     seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhasePending},
	}
	configure(node)
	return node
}

var staticModes = []struct {
	name      string
	configure func(*seiv1alpha1.SeiNode)
}{
	{"full", func(n *seiv1alpha1.SeiNode) { n.Spec.FullNode = &seiv1alpha1.FullNodeSpec{} }},
	{overlayTestArchive, func(n *seiv1alpha1.SeiNode) { n.Spec.Archive = &seiv1alpha1.ArchiveSpec{} }},
	{overlayTestValidator, func(n *seiv1alpha1.SeiNode) { n.Spec.Validator = &seiv1alpha1.ValidatorSpec{} }},
}

// TestStaticInitPlanCarriesNoConfigWriter is the assertion the whole feature
// rests on. A task that writes config.toml on the production pod renames it,
// and a rename onto a mounted path from another container detaches the mount.
func TestStaticInitPlanCarriesNoConfigWriter(t *testing.T) {
	for _, mode := range staticModes {
		t.Run(mode.name, func(t *testing.T) {
			g := NewWithT(t)
			node := withNodeConfig(pendingNode(mode.configure))

			g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
			g.Expect(node.Status.Plan).NotTo(BeNil())

			types := planTaskTypes(node.Status.Plan)
			for _, writer := range mountedConfigWriters {
				g.Expect(types).NotTo(ContainElement(writer))
			}
			g.Expect(types).NotTo(ContainElement(TaskConfigValidate),
				"the controller does not validate a file the operator owns")
			g.Expect(types).To(ContainElement(TaskConfigureGenesis))
			g.Expect(types[len(types)-1]).To(Equal(TaskMarkReady))
		})
	}
}

// TestInitPlanKeepsConfigWriterWithoutConfigSource pins the other half: the
// filter is inert for a node the controller configures.
func TestInitPlanKeepsConfigWriterWithoutConfigSource(t *testing.T) {
	for _, mode := range staticModes {
		t.Run(mode.name, func(t *testing.T) {
			g := NewWithT(t)
			node := pendingNode(mode.configure)

			g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
			g.Expect(planTaskTypes(node.Status.Plan)).To(ContainElement(TaskConfigApply))
		})
	}
}

// TestStaticRunningPlanFollowsTheRoll covers the Running arm. The StatefulSet
// is RollingUpdate for these nodes, so the plan only applies the template and
// waits for the StatefulSet controller's roll; it never deletes the pod itself
// and never writes config.
func TestStaticRunningPlanFollowsTheRoll(t *testing.T) {
	g := NewWithT(t)
	node := withNodeConfig(runningFullNode())
	node.Spec.Image = "sei:v2.0.0"

	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	g.Expect(node.Status.Plan).NotTo(BeNil())

	types := planTaskTypes(node.Status.Plan)
	g.Expect(types).To(Equal([]string{
		task.TaskTypeApplyStatefulSet,
		task.TaskTypeApplyService,
		task.TaskTypeObserveImage,
		TaskMarkReady,
	}))
	g.Expect(node.Status.Plan.ConfigValuesHash).To(BeEmpty())

	cond := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Message).To(ContainSubstring("sei:v2.0.0"))
}

// TestStaticRunningPlanNoDrift pins the steady state: a ConfigMap reference is
// not an observed input, so a node with no image drift plans nothing.
func TestStaticRunningPlanNoDrift(t *testing.T) {
	g := NewWithT(t)
	node := withNodeConfig(runningFullNode())

	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	g.Expect(node.Status.Plan).To(BeNil())
}

// TestStaticValidateRunsTheModesOwnChecks pins the wrapping. Replacing the mode
// planner instead of wrapping it would drop every per-mode Validate.
func TestStaticValidateRunsTheModesOwnChecks(t *testing.T) {
	g := NewWithT(t)
	// A seed without a node-key Secret is refused by seedPlanner.Validate.
	node := withNodeConfig(pendingNode(func(n *seiv1alpha1.SeiNode) {
		n.Spec.Seed = &seiv1alpha1.SeedSpec{}
	}))

	err := (&NodeResolver{}).ResolvePlan(context.Background(), node)
	g.Expect(err).To(HaveOccurred())
	g.Expect(node.Status.Plan).To(BeNil())
}

// TestStaticWorkflowRefused pins the planner half of the lockstep pair with
// SeiNodeReconciler's adoption-time refusal. Every recipe writes config.toml.
func TestStaticWorkflowRefused(t *testing.T) {
	g := NewWithT(t)
	node := withNodeConfig(runningFullNode())
	wf := &seiv1alpha1.SeiNodeTaskWorkflow{
		Spec: seiv1alpha1.SeiNodeTaskWorkflowSpec{
			StateSync: &seiv1alpha1.StateSyncWorkflow{},
		},
	}

	err := (&stateSyncWorkflowPlanner{}).Validate(node, wf)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("static-config"))
}

// TestMountedConfigWriterInPlan covers the single gate that holds the
// invariant. The progression filter removes the writers and
// staticConfigPlanner owns the Running arms; this checks the finished plan
// regardless of which builder produced it.
func TestMountedConfigWriterInPlan(t *testing.T) {
	plan := func(types ...string) *seiv1alpha1.TaskPlan {
		tasks := make([]seiv1alpha1.PlannedTask, len(types))
		for i, tt := range types {
			tasks[i] = seiv1alpha1.PlannedTask{Type: tt}
		}
		return &seiv1alpha1.TaskPlan{Tasks: tasks}
	}

	cases := []struct {
		name string
		plan *seiv1alpha1.TaskPlan
		want string
	}{
		{"clean plan", plan(TaskConfigureGenesis, TaskConfigValidate, TaskMarkReady), ""},
		{"config-apply", plan(TaskConfigureGenesis, TaskConfigApply, TaskMarkReady), TaskConfigApply},
		{"config-patch", plan(task.TaskTypeApplyStatefulSet, TaskConfigPatch), TaskConfigPatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			node := withNodeConfig(runningFullNode())
			g.Expect(mountedConfigWriterInPlan(node, tc.plan)).To(Equal(tc.want))
		})
	}

	t.Run("inert for a node that mounts nothing", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(mountedConfigWriterInPlan(runningFullNode(), plan(TaskConfigApply))).To(BeEmpty())
	})

}
