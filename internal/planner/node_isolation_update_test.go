package planner

import (
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const legacyDedicatedValue = "true"

func dedicatedAnnotation() map[string]string {
	return map[string]string{noderesource.DedicatedNodeKey: legacyDedicatedValue}
}

// observedSharedNode is a Running node whose pod was last rolled Shared.
func observedSharedNode() *seiv1alpha1.SeiNode {
	node := runningFullNode()
	node.Status.CurrentNodeIsolation = seiv1alpha1.NodeIsolationShared
	return node
}

// Shared->Dedicated on a running node rolls the pod through the same
// progression an image bump uses; nothing else moves a pod under OnDelete.
func TestFullPlanner_NodeIsolationDrift_UpdateProgression(t *testing.T) {
	g := NewWithT(t)
	node := observedSharedNode()
	node.Spec.Scheduling = &seiv1alpha1.SchedulingConfig{NodeIsolation: seiv1alpha1.NodeIsolationDedicated}

	plan, err := (&fullNodePlanner{}).BuildPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).NotTo(BeNil(), "node isolation drift should trigger update plan")
	g.Expect(planTaskTypes(plan)).To(Equal([]string{
		task.TaskTypeApplyStatefulSet,
		task.TaskTypeApplyService,
		TaskConfigPatch,
		TaskConfigValidate,
		task.TaskTypeReplacePod,
		task.TaskTypeObserveImage,
		TaskMarkReady,
	}))

	cond := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal("UpdateStarted"))
	g.Expect(cond.Message).To(And(
		ContainSubstring("node isolation drift detected"),
		ContainSubstring("nodeIsolation spec=Dedicated current=Shared"),
		Not(ContainSubstring("image drift"))))
}

// Dedicated->Shared is a roll too: the pod must leave the single-tenant pool.
func TestValidatorPlanner_NodeIsolationDrift_DedicatedToShared(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Spec.FullNode = nil
	node.Spec.Validator = &seiv1alpha1.ValidatorSpec{
		SigningKey: &seiv1alpha1.SigningKeySource{
			Secret: &seiv1alpha1.SecretSigningKeySource{SecretName: testSigningKeySecret},
		},
		NodeKey: &seiv1alpha1.NodeKeySource{
			Secret: &seiv1alpha1.SecretNodeKeySource{SecretName: testNodeKeySecret},
		},
	}
	node.Status.CurrentNodeIsolation = seiv1alpha1.NodeIsolationDedicated
	node.Spec.Scheduling = &seiv1alpha1.SchedulingConfig{NodeIsolation: seiv1alpha1.NodeIsolationShared}

	plan, err := (&validatorPlanner{}).BuildPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).NotTo(BeNil())
	g.Expect(planTaskTypes(plan)).To(ContainElement(task.TaskTypeReplacePod))
}

// The legacy annotation feeds the same effective value, so annotating a
// Shared-observed node is drift, and clearing the field on an annotated node
// that was observed Dedicated is not.
func TestFullPlanner_NodeIsolationDrift_ResolvesThroughAnnotation(t *testing.T) {
	g := NewWithT(t)
	annotated := observedSharedNode()
	annotated.Annotations = dedicatedAnnotation()
	plan, err := (&fullNodePlanner{}).BuildPlan(annotated)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).NotTo(BeNil())

	steady := runningFullNode()
	steady.Annotations = dedicatedAnnotation()
	steady.Status.CurrentNodeIsolation = seiv1alpha1.NodeIsolationDedicated
	plan, err = (&fullNodePlanner{}).BuildPlan(steady)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).To(BeNil())
}

// An unobserved node (controller upgrade) must not fleet-roll, and a node
// whose observed value matches the effective one sits in steady state.
func TestFullPlanner_NodeIsolation_NoDriftCases(t *testing.T) {
	g := NewWithT(t)

	unobserved := runningFullNode()
	unobserved.Spec.Scheduling = &seiv1alpha1.SchedulingConfig{NodeIsolation: seiv1alpha1.NodeIsolationDedicated}
	plan, err := (&fullNodePlanner{}).BuildPlan(unobserved)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).To(BeNil(), "empty CurrentNodeIsolation must NOT trigger drift")

	steady := observedSharedNode()
	steady.Spec.Scheduling = &seiv1alpha1.SchedulingConfig{NodeIsolation: seiv1alpha1.NodeIsolationShared}
	plan, err = (&fullNodePlanner{}).BuildPlan(steady)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).To(BeNil())

	unset := observedSharedNode()
	plan, err = (&fullNodePlanner{}).BuildPlan(unset)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).To(BeNil(), "unset field resolves to Shared, matching the observation")
}

// Image and isolation drifting together produce one plan naming both.
func TestPodTemplateDriftMessage_ImageAndIsolation(t *testing.T) {
	g := NewWithT(t)
	node := observedSharedNode()
	node.Spec.Image = testImageV2
	node.Spec.Scheduling = &seiv1alpha1.SchedulingConfig{NodeIsolation: seiv1alpha1.NodeIsolationDedicated}
	g.Expect(podTemplateDriftMessage(node, platformWithSidecar(""))).To(And(
		ContainSubstring("image and node isolation drift detected"),
		ContainSubstring("seid spec="),
		ContainSubstring("nodeIsolation spec=")))
}
