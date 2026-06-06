package task

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fetchStatefulSet signals a missing StatefulSet as (node, nil, nil) so callers
// treat it as a transient wait rather than an error.
func TestPodCycle_FetchStatefulSet_MissingIsTransient(t *testing.T) {
	g := NewWithT(t)
	node := restartPodNode()
	cfg := restartPodCfg(t, node)

	pc := podCycle{cfg: cfg}
	gotNode, sts, err := pc.fetchStatefulSet(context.Background())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(sts).To(BeNil())
	g.Expect(gotNode.Name).To(Equal(node.Name))
}

func TestPodCycle_GuardSelectorAndReplicas(t *testing.T) {
	node := restartPodNode()

	t.Run("nil selector is terminal", func(t *testing.T) {
		g := NewWithT(t)
		sts := stsForRestart()
		sts.Spec.Selector = nil
		err := guardSelectorAndReplicas(node, sts, TaskTypeRestartPod)
		var termErr *TerminalError
		g.Expect(err).To(BeAssignableToTypeOf(termErr))
		g.Expect(err.Error()).To(ContainSubstring("no selector"))
	})

	t.Run("empty selector is terminal", func(t *testing.T) {
		g := NewWithT(t)
		sts := stsForRestart()
		sts.Spec.Selector = &metav1.LabelSelector{}
		err := guardSelectorAndReplicas(node, sts, TaskTypeRestartPod)
		var termErr *TerminalError
		g.Expect(err).To(BeAssignableToTypeOf(termErr))
	})

	t.Run("multi-replica is terminal and names the task", func(t *testing.T) {
		g := NewWithT(t)
		sts := stsForRestart()
		three := int32(3)
		sts.Spec.Replicas = &three
		err := guardSelectorAndReplicas(node, sts, TaskTypeReplacePod)
		var termErr *TerminalError
		g.Expect(err).To(BeAssignableToTypeOf(termErr))
		g.Expect(err.Error()).To(ContainSubstring("multi-replica"))
		g.Expect(err.Error()).To(ContainSubstring(TaskTypeReplacePod))
	})

	t.Run("single-replica with selector passes", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(guardSelectorAndReplicas(node, stsForRestart(), TaskTypeRestartPod)).To(Succeed())
	})
}

// ownedPods returns only pods that carry an ownerReference to the StatefulSet,
// even when a label-matching but unowned pod shares the selector.
func TestPodCycle_OwnedPods_FiltersByOwnership(t *testing.T) {
	g := NewWithT(t)
	node := restartPodNode()
	sts := stsForRestart()

	owned := podForRestart(restartedUID, someTime, false, false)
	unowned := podForRestart(replacementUID, someTime, false, false)
	unowned.Name = restartSTS + "-imposter"
	unowned.OwnerReferences = nil // matches selector labels but not owned

	cfg := restartPodCfg(t, node, sts, owned, unowned)
	pc := podCycle{cfg: cfg}

	pods, err := pc.ownedPods(context.Background(), node, sts)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pods).To(HaveLen(1))
	g.Expect(pods[0].UID).To(Equal(restartedUID))
}

// deletePod tolerates an already-absent pod (NotFound) so the task is
// idempotent across reconciles.
func TestPodCycle_DeletePod_NotFoundTolerant(t *testing.T) {
	g := NewWithT(t)
	node := restartPodNode()
	cfg := restartPodCfg(t, node)
	pc := podCycle{cfg: cfg}

	ghost := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "gone", Namespace: restartNS}}
	g.Expect(pc.deletePod(context.Background(), ghost)).To(Succeed())
}
