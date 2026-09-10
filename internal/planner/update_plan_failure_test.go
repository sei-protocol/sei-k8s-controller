package planner

import (
	"context"
	"math"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func TestAssembleUpdatePlanFailureCondition(t *testing.T) {
	for _, tc := range []struct {
		name        string
		progression []string
		raw         string
		patch       map[string]map[string]any
		cause       string
	}{
		{name: "marshal", progression: []string{TaskConfigPatch}, patch: map[string]map[string]any{configUpdateFile: {"value": math.NaN()}}, cause: "unsupported value: NaN"},
		{name: "insertBefore", progression: []string{TaskMarkReady}, cause: "insertBefore: target"},
		{name: "hash", progression: []string{TaskConfigPatch}, raw: "{", cause: "configValues config.toml:custom: unexpected EOF"},
		{name: "overlay", progression: []string{TaskConfigPatch, TaskConfigValidate}, raw: `{"nested":null}`, cause: "null values are not supported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			node := runningFullNode()
			node.Status.CurrentConfigValuesHash = ""
			if tc.raw != "" {
				node.Spec.ConfigValues = []seiv1alpha1.ConfigValue{{FileName: configUpdateFile, Key: "custom", Value: apiextensionsv1.JSON{Raw: []byte(tc.raw)}}}
			}
			setNodeUpdateCondition(node, metav1.ConditionTrue, "UpdateStarted", "image drift detected")
			plan, err := assembleUpdatePlan(node, tc.progression, tc.patch)
			g.Expect(err).To(MatchError(ContainSubstring(tc.cause)))
			g.Expect(plan).To(BeNil())
			condition := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
			g.Expect(condition).NotTo(BeNil())
			g.Expect(condition.Status).To(Equal(metav1.ConditionFalse), "assembly failure must clear the provisional update stamp")
			g.Expect(condition.Reason).To(Equal("UpdatePlanBuildFailed"))
			g.Expect(condition.Message).To(Equal(err.Error()), "condition must report the actual assembly failure")
			g.Expect(condition.ObservedGeneration).To(Equal(node.Generation))
		})
	}
}

func TestBuildFailureSurvivesImageRevert(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Status.CurrentConfigValuesHash = ""
	node.Spec.Image = "sei:next"
	node.Spec.ConfigValues = []seiv1alpha1.ConfigValue{{FileName: configUpdateFile, Key: "custom", Value: apiextensionsv1.JSON{Raw: []byte("{")}}}
	resolver := &NodeResolver{}
	g.Expect(resolver.ResolvePlan(context.Background(), node)).To(HaveOccurred())
	before := *meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
	g.Expect(before.Reason).To(Equal("UpdatePlanBuildFailed"))
	node.Spec.Image = node.Status.CurrentImage
	for range 2 {
		g.Expect(resolver.ResolvePlan(context.Background(), node)).To(Succeed())
		g.Expect(node.Status.Plan).To(BeNil())
		g.Expect(*meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)).To(Equal(before))
	}
}

func TestBuildFailureLastTransitionTimeAcrossReconciles(t *testing.T) {
	for _, mode := range []string{"full", "archive", "replayer", "seed", "validator"} {
		t.Run(mode, func(t *testing.T) {
			g := NewWithT(t)
			node := runningFullNode()
			if mode != "full" {
				node.Spec.FullNode = nil
			}
			switch mode {
			case "archive":
				node.Spec.Archive = &seiv1alpha1.ArchiveSpec{}
			case "replayer":
				node.Spec.Replayer = &seiv1alpha1.ReplayerSpec{Snapshot: seiv1alpha1.SnapshotSource{S3: &seiv1alpha1.S3SnapshotSource{TargetHeight: 100}}}
				node.Spec.Peers = []seiv1alpha1.PeerSource{{Static: &seiv1alpha1.StaticPeerSource{Addresses: []string{"peer@host:26656"}}}}
			case "seed":
				node.Spec.Seed = seedNode().Spec.Seed
			case "validator":
				node.Spec.Validator = &seiv1alpha1.ValidatorSpec{}
			}
			node.Spec.Image = "sei:next"
			node.Spec.ConfigValues = []seiv1alpha1.ConfigValue{{FileName: configUpdateFile, Key: "custom", Value: apiextensionsv1.JSON{Raw: []byte("{")}}}
			resolver := &NodeResolver{}
			g.Expect(resolver.ResolvePlan(context.Background(), node)).To(HaveOccurred())
			condition := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
			g.Expect(condition).NotTo(BeNil())
			g.Expect(condition.Reason).To(Equal("UpdatePlanBuildFailed"))
			g.Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			// Model a persisted failure from an earlier reconcile without a clock-dependent sleep.
			condition.LastTransitionTime = metav1.NewTime(time.Unix(100, 0).UTC())
			persisted := node.DeepCopy()
			node = persisted.DeepCopy()
			g.Expect(resolver.ResolvePlan(context.Background(), node)).To(HaveOccurred())
			condition = meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
			g.Expect(condition.LastTransitionTime).To(Equal(persisted.Status.Conditions[0].LastTransitionTime))
			g.Expect(apiequality.Semantic.DeepEqual(persisted.Status, node.Status)).To(BeTrue(), "unchanged failure must pass the controller's status no-op guard")
		})
	}
}
