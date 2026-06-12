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

// The retired DiscoverPeers kind is no longer in the enum, so a CR with
// kind=DiscoverPeers is rejected at the schema layer (the imperative emitter
// was removed — the controller owns peering via config-apply).
func TestCEL_DiscoverPeers_Removed_Rejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	snt := baseTask(ns, "discover-gone", seiv1alpha1.SeiNodeTaskKind("DiscoverPeers"))
	err := testCli.Create(testCtx, snt)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("Unsupported value"))
}

// RestartSeid with its matching empty payload is accepted.
func TestCEL_RestartSeid_Accepted(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	snt := baseTask(ns, "restart-ok", seiv1alpha1.SeiNodeTaskKindRestartSeid)
	snt.Spec.RestartSeid = &seiv1alpha1.RestartSeidPayload{}
	g.Expect(testCli.Create(testCtx, snt)).To(Succeed())
}

// The removed RestartPod kind is no longer in the enum, so a CR with
// kind=RestartPod is rejected at the schema layer.
func TestCEL_RestartPod_Removed_Rejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	snt := baseTask(ns, "restartpod-gone", seiv1alpha1.SeiNodeTaskKind("RestartPod"))
	err := testCli.Create(testCtx, snt)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("Unsupported value"))
}

// MarkReady with its matching empty payload is accepted.
func TestCEL_MarkReady_Accepted(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	snt := baseTask(ns, "markready-ok", seiv1alpha1.SeiNodeTaskKindMarkReady)
	snt.Spec.MarkReady = &seiv1alpha1.MarkReadyPayload{}
	g.Expect(testCli.Create(testCtx, snt)).To(Succeed())
}

// kind=MarkReady with NO payload is rejected (zero payloads / kind-required rule).
func TestCEL_MarkReady_NoPayload_Rejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	snt := baseTask(ns, "markready-nopayload", seiv1alpha1.SeiNodeTaskKindMarkReady)
	err := testCli.Create(testCtx, snt)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(Or(
		ContainSubstring("exactly one"),
		ContainSubstring("markReady is required"),
	))
}

// kind=MarkReady with a second payload (markReady + restartSeid) is rejected by
// the exactly-one union rule.
func TestCEL_MarkReady_MultiplePayloads_Rejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	snt := baseTask(ns, "markready-two-payloads", seiv1alpha1.SeiNodeTaskKindMarkReady)
	snt.Spec.MarkReady = &seiv1alpha1.MarkReadyPayload{}
	snt.Spec.RestartSeid = &seiv1alpha1.RestartSeidPayload{}
	err := testCli.Create(testCtx, snt)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("exactly one"))
}

// kind=RestartSeid with NO payload is rejected.
func TestCEL_RestartSeid_NoPayload_Rejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	snt := baseTask(ns, "restart-nopayload", seiv1alpha1.SeiNodeTaskKindRestartSeid)
	err := testCli.Create(testCtx, snt)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(Or(
		ContainSubstring("exactly one"),
		ContainSubstring("restartSeid is required"),
	))
}

// kind=RestartSeid with TWO payloads (restartSeid + markReady) is rejected by
// the exactly-one union rule.
func TestCEL_MultiplePayloads_Rejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	snt := baseTask(ns, "two-payloads", seiv1alpha1.SeiNodeTaskKindRestartSeid)
	snt.Spec.RestartSeid = &seiv1alpha1.RestartSeidPayload{}
	snt.Spec.MarkReady = &seiv1alpha1.MarkReadyPayload{}
	err := testCli.Create(testCtx, snt)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("exactly one"))
}

// kind=RestartSeid carrying a mismatched payload (markReady) is rejected by
// the kind/payload agreement rule.
func TestCEL_KindPayloadMismatch_Rejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	snt := baseTask(ns, "kind-mismatch", seiv1alpha1.SeiNodeTaskKindRestartSeid)
	snt.Spec.MarkReady = &seiv1alpha1.MarkReadyPayload{}
	err := testCli.Create(testCtx, snt)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("restartSeid is required"))
}

// spec.kind is immutable — a RestartSeid task cannot be flipped to MarkReady.
func TestCEL_KindImmutable_RestartSeidToMarkReady(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	snt := baseTask(ns, "kind-immutable", seiv1alpha1.SeiNodeTaskKindRestartSeid)
	snt.Spec.RestartSeid = &seiv1alpha1.RestartSeidPayload{}
	g.Expect(testCli.Create(testCtx, snt)).To(Succeed())

	patch := client.MergeFrom(snt.DeepCopy())
	snt.Spec.Kind = seiv1alpha1.SeiNodeTaskKindMarkReady
	snt.Spec.RestartSeid = nil
	snt.Spec.MarkReady = &seiv1alpha1.MarkReadyPayload{}
	err := testCli.Patch(testCtx, snt, patch)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("kind is immutable"))
}
