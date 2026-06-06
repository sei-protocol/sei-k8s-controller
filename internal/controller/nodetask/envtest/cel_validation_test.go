//go:build envtest

package envtest_test

import (
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// makeNamespace creates a unique namespace per test for isolation.
func makeNamespace(t *testing.T) string {
	t.Helper()
	g := NewWithT(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "snt-" + rand.String(8)}}
	g.Expect(testCli.Create(testCtx, ns)).To(Succeed())
	return ns.Name
}

func baseTask(ns, name string, kind seiv1alpha1.SeiNodeTaskKind) *seiv1alpha1.SeiNodeTask {
	return &seiv1alpha1.SeiNodeTask{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: seiv1alpha1.SeiNodeTaskSpec{
			Kind: kind,
			Target: seiv1alpha1.SeiNodeTaskTarget{
				NodeRef: seiv1alpha1.SeiNodeTaskNodeRef{Name: "target-node"},
			},
		},
	}
}

// DiscoverPeers with its matching empty payload is accepted.
func TestCEL_DiscoverPeers_Accepted(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	snt := baseTask(ns, "discover-ok", seiv1alpha1.SeiNodeTaskKindDiscoverPeers)
	snt.Spec.DiscoverPeers = &seiv1alpha1.DiscoverPeersPayload{}
	g.Expect(testCli.Create(testCtx, snt)).To(Succeed())
}

// RestartPod with a payload carrying a non-empty podUID is accepted.
func TestCEL_RestartPod_Accepted(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	snt := baseTask(ns, "restart-ok", seiv1alpha1.SeiNodeTaskKindRestartPod)
	snt.Spec.RestartPod = &seiv1alpha1.RestartPodPayload{PodUID: "pod-uid-1"}
	g.Expect(testCli.Create(testCtx, snt)).To(Succeed())
}

// kind=DiscoverPeers with NO payload is rejected (zero payloads).
func TestCEL_DiscoverPeers_NoPayload_Rejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	snt := baseTask(ns, "discover-nopayload", seiv1alpha1.SeiNodeTaskKindDiscoverPeers)
	err := testCli.Create(testCtx, snt)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(Or(
		ContainSubstring("exactly one"),
		ContainSubstring("discoverPeers is required"),
	))
}

// kind=RestartPod with NO payload is rejected.
func TestCEL_RestartPod_NoPayload_Rejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	snt := baseTask(ns, "restart-nopayload", seiv1alpha1.SeiNodeTaskKindRestartPod)
	err := testCli.Create(testCtx, snt)
	g.Expect(err).To(HaveOccurred())
}

// kind=RestartPod with a payload but empty podUID is rejected — the controller
// content-addresses the pod to delete, so an empty UID is not a valid restart
// target.
func TestCEL_RestartPod_EmptyPodUID_Rejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	snt := baseTask(ns, "restart-emptyuid", seiv1alpha1.SeiNodeTaskKindRestartPod)
	snt.Spec.RestartPod = &seiv1alpha1.RestartPodPayload{}
	err := testCli.Create(testCtx, snt)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(Or(
		ContainSubstring("podUID is required"),
		ContainSubstring("should be at least 1 chars long"),
	))
}

// kind=DiscoverPeers with TWO payloads (discoverPeers + restartPod) is
// rejected by the exactly-one union rule.
func TestCEL_MultiplePayloads_Rejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	snt := baseTask(ns, "two-payloads", seiv1alpha1.SeiNodeTaskKindDiscoverPeers)
	snt.Spec.DiscoverPeers = &seiv1alpha1.DiscoverPeersPayload{}
	snt.Spec.RestartPod = &seiv1alpha1.RestartPodPayload{}
	err := testCli.Create(testCtx, snt)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("exactly one"))
}

// kind=DiscoverPeers carrying a mismatched payload (restartPod) is rejected by
// the kind/payload agreement rule.
func TestCEL_KindPayloadMismatch_Rejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	snt := baseTask(ns, "kind-mismatch", seiv1alpha1.SeiNodeTaskKindDiscoverPeers)
	snt.Spec.RestartPod = &seiv1alpha1.RestartPodPayload{}
	err := testCli.Create(testCtx, snt)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("discoverPeers is required"))
}

// spec.kind is immutable — a DiscoverPeers task cannot be flipped to RestartPod.
func TestCEL_KindImmutable_DiscoverPeersToRestartPod(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	snt := baseTask(ns, "kind-immutable", seiv1alpha1.SeiNodeTaskKindDiscoverPeers)
	snt.Spec.DiscoverPeers = &seiv1alpha1.DiscoverPeersPayload{}
	g.Expect(testCli.Create(testCtx, snt)).To(Succeed())

	patch := client.MergeFrom(snt.DeepCopy())
	snt.Spec.Kind = seiv1alpha1.SeiNodeTaskKindRestartPod
	snt.Spec.DiscoverPeers = nil
	snt.Spec.RestartPod = &seiv1alpha1.RestartPodPayload{PodUID: "pod-uid-1"}
	err := testCli.Patch(testCtx, snt, patch)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("kind is immutable"))
}
