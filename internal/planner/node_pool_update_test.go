package planner

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const benchPool = "sei-validator-c8g"

// Requesting a pool on a running node rolls the pod through the same
// progression an image bump uses, and the condition names the pool change.
func TestFullPlanner_NodePoolDrift_UpdateProgression(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Spec.Scheduling = &seiv1alpha1.SchedulingConfig{NodePool: benchPool}

	plan, err := (&fullNodePlanner{}).BuildPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).NotTo(BeNil(), "a nodePool change should trigger an update plan")
	g.Expect(planTaskTypes(plan)).To(ContainElements(task.TaskTypeApplyStatefulSet, task.TaskTypeReplacePod, task.TaskTypeObserveImage))

	cond := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Message).To(And(
		ContainSubstring("node pool drift detected"),
		ContainSubstring(`nodePool spec="sei-validator-c8g" current=""`),
		Not(ContainSubstring("isolation"))))
}

// Leaving a requested pool is a roll too: the pod must return to its mode's pool.
func TestFullPlanner_NodePoolDrift_BackToModePool(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Status.CurrentNodePool = benchPool

	plan, err := (&fullNodePlanner{}).BuildPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).NotTo(BeNil())
	g.Expect(planTaskTypes(plan)).To(ContainElement(task.TaskTypeReplacePod))
}

// A node whose pod already runs on the requested pool, and a node with no
// observed rollout yet, are not drifted.
func TestFullPlanner_NodePool_NoDriftCases(t *testing.T) {
	g := NewWithT(t)

	steady := runningFullNode()
	steady.Spec.Scheduling = &seiv1alpha1.SchedulingConfig{NodePool: benchPool}
	steady.Status.CurrentNodePool = benchPool
	plan, err := (&fullNodePlanner{}).BuildPlan(steady)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).To(BeNil())

	unobserved := runningFullNode()
	unobserved.Status.CurrentImage = ""
	unobserved.Spec.Scheduling = &seiv1alpha1.SchedulingConfig{NodePool: benchPool}
	g.Expect(nodePoolDrifted(unobserved)).To(BeFalse())
}

// A RollingUpdate (spec.nodeConfig) node renders its template on every
// reconcile, so while the drift budget defers its roll the rendered copy keeps
// the pool its pod runs on rather than moving it outside the budget.
func TestDriftRenderNode_DeferredNodePoolChangeKeepsTheRunningPool(t *testing.T) {
	g := NewWithT(t)
	nodes := eightDrifted()
	updating(nodes[0])
	updating(nodes[1])
	n := withNodeConfig(runningFullNode())
	n.Name = "node-y"
	n.Spec.Scheduling = &seiv1alpha1.SchedulingConfig{NodePool: benchPool}

	r := budgetResolver(t, 25, append(nodes, n)...)
	got, err := r.DriftRenderNode(context.Background(), n)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).NotTo(BeIdenticalTo(n))
	g.Expect(got.Spec.Scheduling.NodePool).To(BeEmpty(), "the pinned copy renders the running pool")
	g.Expect(n.Spec.Scheduling.NodePool).To(Equal(benchPool), "the node itself keeps its spec")
	cond := meta.FindStatusCondition(n.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal(ReasonUpdateDeferred))
}
