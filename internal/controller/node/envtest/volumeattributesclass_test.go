//go:build envtest

package envtest_test

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// The VolumeAttributesClass pre-flight, driven by a real reconciler against a
// real apiserver — the path the RBAC read verbs exist for. The unit tests in
// internal/task cover the branch matrix; these prove the whole chain: a
// cluster-scoped read through the manager's cached client, the condition on
// status, and what does or does not land on the provisioned claim.
//
// The 1.34 control plane serves storage.k8s.io/v1 VolumeAttributesClasses (the
// GA version DR-001 depends on); a cluster that did not would fail these with a
// no-matching-kind error rather than not-found.

// ensureVolumeAttributesClass creates a cluster-scoped VolumeAttributesClass and
// removes it when the test ends. Driver and parameters are opaque placeholders —
// the controller references the class by name and never reads its contents.
func ensureVolumeAttributesClass(t *testing.T, name string) {
	t.Helper()
	g := NewWithT(t)
	vac := &storagev1.VolumeAttributesClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		DriverName: "csi.example.com",
		Parameters: map[string]string{"placeholder": "opaque-to-the-controller"},
	}
	g.Expect(testCli.Create(testCtx, vac)).To(Succeed())
	t.Cleanup(func() {
		if err := testCli.Delete(testCtx, vac); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("deleting VolumeAttributesClass %q: %v", name, err)
		}
	})
}

// vacSelectingNode returns a full node in the default namespace selecting vac.
// An empty vac leaves the selection unset.
func vacSelectingNode(name, vac string) *seiv1alpha1.SeiNode {
	node := lifecycleNode(name)
	if vac != "" {
		node.Spec.DataVolume = &seiv1alpha1.DataVolumeSpec{
			Storage: &seiv1alpha1.DataVolumeStorage{VolumeAttributesClassName: &vac},
		}
	}
	return node
}

func vacCondition(g Gomega, cli client.Client, name string) *metav1.Condition {
	node := getNode(g, cli, name)
	cond := apimeta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionVolumeAttributesClassReady)
	g.Expect(cond).NotTo(BeNil(),
		"VolumeAttributesClassReady must be present once ensure-data-pvc has run")
	return cond
}

func dataPVC(cli client.Client, node string) (*corev1.PersistentVolumeClaim, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	err := cli.Get(testCtx, types.NamespacedName{Namespace: "default", Name: "data-" + node}, pvc)
	return pvc, err
}

// A selected class that is not in the cluster holds the provision and names
// itself, instead of binding a dangling reference into a create-once PVC and
// leaving a silently-Pending pod. Adding the class — a platform/GitOps act —
// releases the hold on the next poll.
func TestVolumeAttributesClass_MissingHoldsProvision_ThenAppearsAndStamps(t *testing.T) {
	g := NewWithT(t)
	cli := startNodeManager(t, newFakeSidecar())

	const nodeName = "vac-preflight"
	const vacName = "vac-preflight-class"
	g.Expect(testCli.Create(testCtx, vacSelectingNode(nodeName, vacName))).To(Succeed())

	g.Eventually(func(g Gomega) {
		cond := vacCondition(g, cli, nodeName)
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonVolumeAttributesClassNotFound))
		g.Expect(cond.Message).To(ContainSubstring(vacName),
			"the message must name the class the platform has to add")
	}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

	_, err := dataPVC(cli, nodeName)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"no claim may be created while the selected class is missing — the claim binds the name once")

	ensureVolumeAttributesClass(t, vacName)

	g.Eventually(func(g Gomega) {
		cond := vacCondition(g, cli, nodeName)
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonVolumeAttributesClassFound))

		pvc, err := dataPVC(cli, nodeName)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(pvc.Spec.VolumeAttributesClassName).NotTo(BeNil())
		g.Expect(*pvc.Spec.VolumeAttributesClassName).To(Equal(vacName))
	}).WithTimeout(60 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
}

// DR-001: with no selection at all the condition is still present, and True.
// Absence would force a consumer to read "not configured" out of a missing
// condition — the naive implementation of the pre-flight.
func TestVolumeAttributesClass_NoSelectionStillReportsTrue(t *testing.T) {
	g := NewWithT(t)
	cli := startNodeManager(t, newFakeSidecar())

	const nodeName = "vac-none"
	g.Expect(testCli.Create(testCtx, vacSelectingNode(nodeName, ""))).To(Succeed())

	g.Eventually(func(g Gomega) {
		cond := vacCondition(g, cli, nodeName)
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonNoVolumeAttributesClass))

		pvc, err := dataPVC(cli, nodeName)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(pvc.Spec.VolumeAttributesClassName).To(BeNil(),
			"with no selection the claim carries no volumeAttributesClassName at all")
	}).WithTimeout(60 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
}
