package planner

import (
	"math"
	"testing"

	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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
