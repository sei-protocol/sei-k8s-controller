package node

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
)

func evmServingCondition(node *seiv1alpha1.SeiNode) *metav1.Condition {
	return apimeta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionEvmServing)
}

func evmServingNode(engine *seiv1alpha1.ExecutionEngineSpec) *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "evm-val-0", Namespace: testNamespace, Generation: 4},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:         testChainID,
			Image:           testImage,
			Validator:       &seiv1alpha1.ValidatorSpec{},
			Consensus:       &seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineAutobahn},
			ExecutionEngine: engine,
		},
		Status: seiv1alpha1.SeiNodeStatus{
			StatefulSet: &seiv1alpha1.StatefulSetRef{Name: "evm-val-0"},
		},
	}
}

func seidPod(node *seiv1alpha1.SeiNode, ready bool) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.Status.StatefulSet.Name + "-0",
			Namespace: node.Namespace,
			Labels:    noderesource.SelectorLabels(node),
		},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}},
	}
}

func TestEvmServing_DefaultEngine_NotApplicable(t *testing.T) {
	g := NewWithT(t)
	node := evmServingNode(nil)
	r, _ := newNodeReconciler(t, node)

	r.reconcileEvmServing(context.Background(), node)

	cond := evmServingCondition(node)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonEvmNotApplicable))
	g.Expect(cond.ObservedGeneration).To(Equal(int64(4)))
}

func TestEvmServing_HttpDisabled(t *testing.T) {
	g := NewWithT(t)
	off := false
	node := evmServingNode(&seiv1alpha1.ExecutionEngineSpec{
		Mode:    seiv1alpha1.ExecutionEngineEvmOnly,
		EvmOnly: &seiv1alpha1.EvmOnlyExecutionSpec{HttpEnabled: &off},
	})
	r, _ := newNodeReconciler(t, node, seidPod(node, true))

	r.reconcileEvmServing(context.Background(), node)

	cond := evmServingCondition(node)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonEvmHttpDisabled), "a ready pod does not make a closed listener serve")
	g.Expect(composeNodeEndpoints(node)).To(BeNil())
}

func TestEvmServing_PodReadyIsServing(t *testing.T) {
	g := NewWithT(t)
	node := evmServingNode(&seiv1alpha1.ExecutionEngineSpec{Mode: seiv1alpha1.ExecutionEngineEvmOnly})
	r, _ := newNodeReconciler(t, node, seidPod(node, true))

	r.reconcileEvmServing(context.Background(), node)

	cond := evmServingCondition(node)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonEvmServing))
	g.Expect(cond.ObservedGeneration).To(Equal(int64(4)))
	g.Expect(composeNodeEndpoints(node)).To(Equal(&seiv1alpha1.NodeEndpointStatus{
		EvmJsonRpc: "http://evm-val-0." + testNamespace + ".svc:8545",
	}))
}

func TestEvmServing_ListenerRefused(t *testing.T) {
	g := NewWithT(t)
	node := evmServingNode(&seiv1alpha1.ExecutionEngineSpec{Mode: seiv1alpha1.ExecutionEngineEvmOnly})

	t.Run("pod not ready", func(t *testing.T) {
		n := node.DeepCopy()
		r, _ := newNodeReconciler(t, n, seidPod(n, false))
		r.reconcileEvmServing(context.Background(), n)
		cond := evmServingCondition(n)
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonEvmListenerRefused))
		g.Expect(composeNodeEndpoints(n)).To(BeNil())
	})

	t.Run("pod missing", func(t *testing.T) {
		n := node.DeepCopy()
		r, _ := newNodeReconciler(t, n)
		r.reconcileEvmServing(context.Background(), n)
		g.Expect(evmServingCondition(n).Reason).To(Equal(seiv1alpha1.ReasonEvmListenerRefused))
	})

	t.Run("no statefulset yet", func(t *testing.T) {
		n := node.DeepCopy()
		n.Status.StatefulSet = nil
		r, _ := newNodeReconciler(t, n)
		r.reconcileEvmServing(context.Background(), n)
		g.Expect(evmServingCondition(n).Reason).To(Equal(seiv1alpha1.ReasonEvmListenerRefused))
	})
}

// Serving -> refused clears the published endpoint on the same reconcile that
// flips the condition: the network must never hold a URL the child no longer
// stands behind.
func TestEvmServing_LossClearsEndpoint(t *testing.T) {
	g := NewWithT(t)
	node := evmServingNode(&seiv1alpha1.ExecutionEngineSpec{Mode: seiv1alpha1.ExecutionEngineEvmOnly})
	pod := seidPod(node, true)
	r, c := newNodeReconciler(t, node, pod)

	r.reconcileEvmServing(context.Background(), node)
	node.Status.Endpoint = composeNodeEndpoints(node)
	g.Expect(node.Status.Endpoint).NotTo(BeNil())

	pod.Status.Conditions[0].Status = corev1.ConditionFalse
	g.Expect(c.Status().Update(context.Background(), pod)).To(Succeed())

	r.reconcileEvmServing(context.Background(), node)
	node.Status.Endpoint = composeNodeEndpoints(node)
	g.Expect(evmServingCondition(node).Reason).To(Equal(seiv1alpha1.ReasonEvmListenerRefused))
	g.Expect(node.Status.Endpoint).To(BeNil())
}

func TestPodReadyChanged_AdmitsOnlyReadyFlips(t *testing.T) {
	g := NewWithT(t)
	node := evmServingNode(nil)
	ready := seidPod(node, true)
	unready := seidPod(node, false)

	g.Expect(podReadyChanged.Update(event.UpdateEvent{ObjectOld: unready, ObjectNew: ready})).To(BeTrue())
	g.Expect(podReadyChanged.Update(event.UpdateEvent{ObjectOld: ready, ObjectNew: ready.DeepCopy()})).To(BeFalse())
	g.Expect(podReadyChanged.Create(event.CreateEvent{Object: unready})).To(BeTrue())
	g.Expect(podReadyChanged.Delete(event.DeleteEvent{Object: ready})).To(BeTrue())
}

func TestPodToSeiNode_MapsByNodeLabel(t *testing.T) {
	g := NewWithT(t)
	node := evmServingNode(nil)

	reqs := podToSeiNode(context.Background(), seidPod(node, true))
	g.Expect(reqs).To(HaveLen(1))
	g.Expect(reqs[0].Name).To(Equal(node.Name))
	g.Expect(reqs[0].Namespace).To(Equal(node.Namespace))

	var unlabeled client.Object = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "stray", Namespace: testNamespace}}
	g.Expect(podToSeiNode(context.Background(), unlabeled)).To(BeEmpty())
}
