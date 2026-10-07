//go:build envtest

package envtest_test

import (
	"testing"

	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// Admission coverage of spec.dataResetGeneration (spec 009 Requirement 1,
// SC-001): the counter can only increase, and needs spec.nodeConfig. These
// cases need no controller.

func updateResetCounter(t *testing.T, node *seiv1alpha1.SeiNode, value int64) error {
	t.Helper()
	cur := &seiv1alpha1.SeiNode{}
	if err := testCli.Get(testCtx, client.ObjectKeyFromObject(node), cur); err != nil {
		t.Fatalf("get: %v", err)
	}
	cur.Spec.DataResetGeneration = value
	return testCli.Update(testCtx, cur)
}

// 009 Req 1.1, 1.2: increase is accepted; lowering or removing is rejected.
func TestDataResetGeneration_OnlyIncreases(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	node := nodeConfigNode(ns, "reset-mono")
	node.Spec.DataResetGeneration = 2
	g.Expect(testCli.Create(testCtx, node)).To(Succeed())

	g.Expect(updateResetCounter(t, node, 3)).To(Succeed(), "an increase asks for a reset")
	g.Expect(updateResetCounter(t, node, 3)).To(Succeed(), "an unchanged counter is a no-op")

	err := updateResetCounter(t, node, 1)
	g.Expect(err).To(HaveOccurred(), "a git revert must not lower the counter")
	g.Expect(err.Error()).To(ContainSubstring("can only increase"))

	// 0 is omitted on the wire, so setting 0 removes the field.
	g.Expect(updateResetCounter(t, node, 0)).To(HaveOccurred(), "removing a set counter is rejected")
}

// 009 Req 1.3: the counter needs spec.nodeConfig.
func TestDataResetGeneration_RequiresNodeConfig(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	node := nodeConfigNode(ns, "reset-no-nc")
	node.Spec.NodeConfig = nil
	node.Spec.DataResetGeneration = 1

	err := testCli.Create(testCtx, node)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("needs spec.nodeConfig"))

	node.Spec.DataResetGeneration = 0
	g.Expect(testCli.Create(testCtx, node)).To(Succeed(), "an unset counter is fine without nodeConfig")
}

// 009 Req 1.1: the minimum is 0.
func TestDataResetGeneration_Negative_Rejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	node := nodeConfigNode(ns, "reset-neg")
	node.Spec.DataResetGeneration = -1
	g.Expect(testCli.Create(testCtx, node)).To(HaveOccurred())
}
