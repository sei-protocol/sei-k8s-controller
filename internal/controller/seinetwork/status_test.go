package seinetwork

import (
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func runningChild(engine *seiv1alpha1.ExecutionEngineSpec, evmServing *metav1.ConditionStatus) *seiv1alpha1.SeiNode {
	node := &seiv1alpha1.SeiNode{
		Spec: seiv1alpha1.SeiNodeSpec{
			Validator:       &seiv1alpha1.ValidatorSpec{},
			Consensus:       &seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineAutobahn},
			ExecutionEngine: engine,
		},
		Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseRunning},
	}
	if evmServing != nil {
		apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
			Type: seiv1alpha1.ConditionEvmServing, Status: *evmServing, Reason: "test",
		})
	}
	return node
}

// Running is the whole story for a Default-engine child; an EVM-only child
// with its listener enabled must also be EvmServing, since that is the surface
// the network publishes for it.
func TestChildReady(t *testing.T) {
	g := NewWithT(t)
	off := false
	evmOnly := &seiv1alpha1.ExecutionEngineSpec{Mode: seiv1alpha1.ExecutionEngineEvmOnly}
	evmOnlyHTTPOff := &seiv1alpha1.ExecutionEngineSpec{
		Mode:    seiv1alpha1.ExecutionEngineEvmOnly,
		EvmOnly: &seiv1alpha1.EvmOnlyExecutionSpec{HttpEnabled: &off},
	}

	g.Expect(childReady(runningChild(nil, nil))).To(BeTrue(), "default engine: Running suffices")
	g.Expect(childReady(runningChild(evmOnly, nil))).To(BeFalse(), "evm-only: no EvmServing yet")
	g.Expect(childReady(runningChild(evmOnly, new(metav1.ConditionFalse)))).To(BeFalse(), "evm-only: listener refused")
	g.Expect(childReady(runningChild(evmOnly, new(metav1.ConditionTrue)))).To(BeTrue(), "evm-only: serving")
	g.Expect(childReady(runningChild(evmOnlyHTTPOff, new(metav1.ConditionFalse)))).To(BeTrue(), "evm-only, http off: nothing to publish, Running suffices")

	legacy := runningChild(nil, nil)
	legacy.Spec.Consensus.EvmOnly = true //nolint:staticcheck // deliberately exercising the deprecated field's compatibility path
	g.Expect(childReady(legacy)).To(BeFalse(), "deprecated bool resolves to the same gate")

	pending := runningChild(nil, nil)
	pending.Status.Phase = seiv1alpha1.PhaseInitializing
	g.Expect(childReady(pending)).To(BeFalse())
}

// A consumer reading status.nodes[].ready must see false written out, not a
// missing key it has to interpret.
func TestGroupNodeStatus_ReadyFalseSerializes(t *testing.T) {
	g := NewWithT(t)
	raw, err := json.Marshal(seiv1alpha1.GroupNodeStatus{Name: "n-0", Ready: false})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(raw)).To(ContainSubstring(`"ready":false`))
}
