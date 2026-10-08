//go:build envtest

package envtest_test

import (
	"slices"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest"
)

// Spec 013 Req 1.2 against a real API server: the API server refuses a
// podManagementPolicy change in place, so the switch deletes the StatefulSet
// with orphan propagation, waits while it terminates, and then applies the
// Parallel StatefulSet. envtest runs no garbage collector, so the test plays
// its part and releases the orphan finalizer.
func TestNodeConfig_SwitchRecreatesStatefulSet(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	platform := platformtest.Config()

	node := nodeConfigNode(ns, "nc-live-switch")
	node.Spec.NodeConfig = nil
	g.Expect(testCli.Create(testCtx, node)).To(Succeed())

	sts, err := noderesource.SyncStatefulSet(testCtx, testCli, testCli.Scheme(), node, platform)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(sts.Spec.PodManagementPolicy).To(Equal(appsv1.OrderedReadyPodManagement))
	ref := &seiv1alpha1.StatefulSetRef{Name: sts.Name, UID: sts.UID}

	inPlace := sts.DeepCopy()
	inPlace.Spec.PodManagementPolicy = appsv1.ParallelPodManagement
	err = testCli.Update(testCtx, inPlace)
	g.Expect(apierrors.IsInvalid(err)).To(BeTrue(), "the API server must refuse the change in place; got %v", err)

	key := client.ObjectKeyFromObject(node)
	g.Expect(updateNodeWithRetry(t, key, func(cur *seiv1alpha1.SeiNode) {
		cur.Spec.NodeConfig = &seiv1alpha1.NodeConfig{
			ConfigRef: seiv1alpha1.ConfigFileRef{Name: "rpc-config-v1"},
			AppRef:    seiv1alpha1.ConfigFileRef{Name: "rpc-app-v1"},
		}
	})).To(Succeed())
	g.Expect(testCli.Get(testCtx, key, node)).To(Succeed())
	node.Status.StatefulSet = ref

	sts, err = noderesource.SyncStatefulSet(testCtx, testCli, testCli.Scheme(), node, platform)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(sts).To(BeNil())
	g.Expect(node.Status.StatefulSet).To(BeNil())

	old := &appsv1.StatefulSet{}
	g.Expect(testCli.Get(testCtx, client.ObjectKey{Namespace: ns, Name: ref.Name}, old)).To(Succeed())
	g.Expect(old.DeletionTimestamp).NotTo(BeNil())
	g.Expect(slices.Contains(old.Finalizers, "orphan")).To(BeTrue(), "the delete must orphan the pod; finalizers=%v", old.Finalizers)

	sts, err = noderesource.SyncStatefulSet(testCtx, testCli, testCli.Scheme(), node, platform)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(sts).To(BeNil(), "the sync must wait while the old StatefulSet terminates")

	old.Finalizers = nil
	g.Expect(testCli.Update(testCtx, old)).To(Succeed())

	g.Eventually(func() error {
		sts, err = noderesource.SyncStatefulSet(testCtx, testCli, testCli.Scheme(), node, platform)
		if err != nil {
			return err
		}
		if sts == nil {
			return apierrors.NewNotFound(appsv1.Resource("statefulsets"), ref.Name)
		}
		return nil
	}).Should(Succeed())
	g.Expect(sts.UID).NotTo(Equal(ref.UID))
	g.Expect(sts.Spec.PodManagementPolicy).To(Equal(appsv1.ParallelPodManagement))
	g.Expect(sts.Spec.UpdateStrategy.Type).To(Equal(appsv1.RollingUpdateStatefulSetStrategyType))
}
