package node

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
)

func stsPod(sts *appsv1.StatefulSet, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: sts.Name + "-0", Namespace: sts.Namespace, Labels: labels,
	}}
}

func TestNodeReconcile_BackfillsNodeIsolation_FromPodLabels(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node, sts := runningFullNode(t, "legacy-shared", "sei")
	pod := stsPod(sts, sts.Spec.Template.Labels)
	r, c := newNodeReconciler(t, node, sts, pod)
	_, err := r.Reconcile(ctx, nodeReqFor(node.Name, node.Namespace))
	g.Expect(err).NotTo(HaveOccurred())
	got := getSeiNode(t, ctx, c, node.Name, node.Namespace)
	g.Expect(got.Status.CurrentNodeIsolation).To(Equal(seiv1alpha1.NodeIsolationShared))
	g.Expect(got.Status.Plan).To(BeNil(), "backfill alone must not roll the pod")

	node, sts = runningFullNode(t, "legacy-dedicated", "sei")
	labels := map[string]string{}
	for k, v := range sts.Spec.Template.Labels {
		labels[k] = v
	}
	labels[noderesource.DedicatedNodeKey] = "true"
	pod = stsPod(sts, labels)
	r, c = newNodeReconciler(t, node, sts, pod)
	_, err = r.Reconcile(ctx, nodeReqFor(node.Name, node.Namespace))
	g.Expect(err).NotTo(HaveOccurred())
	got = getSeiNode(t, ctx, c, node.Name, node.Namespace)
	g.Expect(got.Status.CurrentNodeIsolation).To(Equal(seiv1alpha1.NodeIsolationDedicated))
}

func TestNodeReconcile_BackfillNodeIsolation_SkipsWithoutPodOrWhenObserved(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node, sts := runningFullNode(t, "no-pod", "sei")
	r, c := newNodeReconciler(t, node, sts)
	_, err := r.Reconcile(ctx, nodeReqFor(node.Name, node.Namespace))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getSeiNode(t, ctx, c, node.Name, node.Namespace).Status.CurrentNodeIsolation).To(BeEmpty())

	node, sts = runningFullNode(t, "observed", "sei")
	node.Status.CurrentNodeIsolation = seiv1alpha1.NodeIsolationDedicated
	pod := stsPod(sts, sts.Spec.Template.Labels) // shared-labelled pod must not overwrite
	r, c = newNodeReconciler(t, node, sts, pod)
	_, err = r.Reconcile(ctx, nodeReqFor(node.Name, node.Namespace))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getSeiNode(t, ctx, c, node.Name, node.Namespace).Status.CurrentNodeIsolation).
		To(Equal(seiv1alpha1.NodeIsolationDedicated))
}
