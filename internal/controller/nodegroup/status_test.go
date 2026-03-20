package nodegroup

import (
	"testing"

	. "github.com/onsi/gomega"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func TestComputeGroupPhase_NoNodes(t *testing.T) {
	g := NewWithT(t)
	phase := computeGroupPhase(0, 3, nil)
	g.Expect(phase).To(Equal(seiv1alpha1.GroupPhasePending))
}

func TestComputeGroupPhase_AllReady(t *testing.T) {
	g := NewWithT(t)
	nodes := makeNodes(3, seiv1alpha1.PhaseRunning)
	phase := computeGroupPhase(3, 3, nodes)
	g.Expect(phase).To(Equal(seiv1alpha1.GroupPhaseReady))
}

func TestComputeGroupPhase_Initializing(t *testing.T) {
	g := NewWithT(t)
	nodes := []seiv1alpha1.SeiNode{
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseRunning}},
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseInitializing}},
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhasePending}},
	}
	phase := computeGroupPhase(1, 3, nodes)
	g.Expect(phase).To(Equal(seiv1alpha1.GroupPhaseInitializing))
}

func TestComputeGroupPhase_Degraded(t *testing.T) {
	g := NewWithT(t)
	nodes := []seiv1alpha1.SeiNode{
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseRunning}},
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseRunning}},
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseFailed}},
	}
	phase := computeGroupPhase(2, 3, nodes)
	g.Expect(phase).To(Equal(seiv1alpha1.GroupPhaseDegraded))
}

func TestComputeGroupPhase_SomeFailedSomeInitializing(t *testing.T) {
	g := NewWithT(t)
	nodes := []seiv1alpha1.SeiNode{
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseFailed}},
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseInitializing}},
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhasePending}},
	}
	phase := computeGroupPhase(0, 3, nodes)
	g.Expect(phase).To(Equal(seiv1alpha1.GroupPhaseInitializing),
		"should be Initializing when some nodes are still progressing, not Failed")
}

func TestComputeGroupPhase_AllFailed(t *testing.T) {
	g := NewWithT(t)
	nodes := makeNodes(3, seiv1alpha1.PhaseFailed)
	phase := computeGroupPhase(0, 3, nodes)
	g.Expect(phase).To(Equal(seiv1alpha1.GroupPhaseFailed))
}

func makeNodes(n int, phase seiv1alpha1.SeiNodePhase) []seiv1alpha1.SeiNode {
	nodes := make([]seiv1alpha1.SeiNode, n)
	for i := range n {
		nodes[i].Status.Phase = phase
	}
	return nodes
}
