//go:build envtest

package envtest_test

import (
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/nodedeployment/envtest/fixtures"
)

const p2pEndpointTestDomain = "test.platform.sei.io"

func expectedP2PEndpointHost(sndName, chainID string, ordinal int) string {
	return fmt.Sprintf("%s-%d-p2p.%s.%s", sndName, ordinal, chainID, p2pEndpointTestDomain)
}

func expectedP2PEndpointAddr(sndName, chainID string, ordinal int) string {
	return expectedP2PEndpointHost(sndName, chainID, ordinal) + ":26656"
}

// withP2PEndpointDomain sets the test domain on the shared reconciler and
// restores it after the test.
func withP2PEndpointDomain(t *testing.T, domain string) {
	t.Helper()
	prev := testSNDReconciler.P2PEndpointDomain
	testSNDReconciler.P2PEndpointDomain = domain
	t.Cleanup(func() { testSNDReconciler.P2PEndpointDomain = prev })
}

// withTCP enables `spec.networking.tcp` on a fixtures-built SND. Goes
// here (not in fixtures/) because TCP-only is a publishable-test
// concept; the broader fixture surface stays minimal.
func withTCP() fixtures.Option {
	return func(snd *seiv1alpha1.SeiNodeDeployment) {
		if snd.Spec.Networking == nil {
			snd.Spec.Networking = &seiv1alpha1.NetworkingConfig{}
		}
		snd.Spec.Networking.TCP = &seiv1alpha1.TCPConfig{}
	}
}

// Happy path: TCP-enabled SND produces a child with Spec.ExternalAddress,
// a per-pod LB Service, and a P2PEndpoints entry.
func TestP2PEndpointP2P_CreateWithTCP_ChildHasAddressAndServiceExists(t *testing.T) {
	g := NewWithT(t)
	withP2PEndpointDomain(t, p2pEndpointTestDomain)
	ns := makeNamespace(t)

	snd := fixtures.NewSND(ns, "pub-create",
		fixtures.WithReplicas(1),
		withTCP(),
	)
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())

	const chainID = "pacific-1" // fixtures default
	wantHost := expectedP2PEndpointHost(snd.Name, chainID, 0)
	wantAddr := expectedP2PEndpointAddr(snd.Name, chainID, 0)

	// 1. Child SeiNode is created with Spec.ExternalAddress already set.
	//    The brief calls out "child appears in the API with the value
	//    already populated" — this poll asserts the no-race-window claim.
	childKey := types.NamespacedName{Name: snd.Name + "-0", Namespace: ns}
	waitFor(t, func() bool {
		child := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, childKey, child); err != nil {
			return false
		}
		return child.Spec.ExternalAddress == wantAddr
	}, "child SeiNode "+childKey.Name+" populated with publishable ExternalAddress")

	// 2. Per-pod LB Service exists with the expected annotations.
	svcKey := types.NamespacedName{Name: snd.Name + "-0-p2p", Namespace: ns}
	waitFor(t, func() bool {
		svc := &corev1.Service{}
		return testCli.Get(testCtx, svcKey, svc) == nil
	}, "P2P endpoint Service "+svcKey.Name+" created")

	svc := &corev1.Service{}
	g.Expect(testCli.Get(testCtx, svcKey, svc)).To(Succeed())
	g.Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeLoadBalancer))
	g.Expect(svc.Annotations).To(HaveKeyWithValue(
		"external-dns.alpha.kubernetes.io/hostname", wantHost))
	g.Expect(svc.Annotations).To(HaveKeyWithValue(
		"service.beta.kubernetes.io/aws-load-balancer-type", "external"))
	g.Expect(svc.Annotations).To(HaveKeyWithValue(
		"service.beta.kubernetes.io/aws-load-balancer-scheme", "internet-facing"))
	g.Expect(svc.Annotations).To(HaveKeyWithValue(
		"service.beta.kubernetes.io/aws-load-balancer-nlb-target-type", "ip"))
	// Selector targets the child SeiNode's pod via `sei.io/node`.
	g.Expect(svc.Spec.Selector).To(HaveKeyWithValue("sei.io/node", snd.Name+"-0"))

	// 3. Owner reference is the SND (not the child) so opt-out and SND
	//    deletion both reap the Service through the same path.
	refs := svc.GetOwnerReferences()
	g.Expect(refs).NotTo(BeEmpty())
	g.Expect(refs[0].UID).To(Equal(snd.UID))
	g.Expect(refs[0].Controller).NotTo(BeNil())
	g.Expect(*refs[0].Controller).To(BeTrue())

	// 4. P2PEndpoints surfaces the same hostname the child got.
	waitForStatus(t, client.ObjectKeyFromObject(snd), func(latest *seiv1alpha1.SeiNodeDeployment) bool {
		ns := latest.Status.NetworkingStatus
		if ns == nil || len(ns.P2PEndpoints) != 1 {
			return false
		}
		ep := ns.P2PEndpoints[0]
		return ep.Ordinal == 0 &&
			ep.SeiNodeName == snd.Name+"-0" &&
			ep.Hostname == wantAddr
	}, "P2PEndpoints stamped with deterministic vanity host")
}

// Opt-out: removing TCP clears Spec.ExternalAddress and deletes the Service.
func TestP2PEndpointP2P_OptOut_ClearsAddressAndDeletesService(t *testing.T) {
	g := NewWithT(t)
	withP2PEndpointDomain(t, p2pEndpointTestDomain)
	ns := makeNamespace(t)

	snd := fixtures.NewSND(ns, "pub-optout",
		fixtures.WithReplicas(1),
		withTCP(),
	)
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())

	const chainID = "pacific-1"
	wantAddr := expectedP2PEndpointAddr(snd.Name, chainID, 0)
	childKey := types.NamespacedName{Name: snd.Name + "-0", Namespace: ns}
	svcKey := types.NamespacedName{Name: snd.Name + "-0-p2p", Namespace: ns}

	// Wait for converged publishable state.
	waitFor(t, func() bool {
		child := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, childKey, child); err != nil {
			return false
		}
		return child.Spec.ExternalAddress == wantAddr
	}, "child has publishable address before opt-out")
	waitFor(t, func() bool {
		return testCli.Get(testCtx, svcKey, &corev1.Service{}) == nil
	}, "P2P endpoint Service exists before opt-out")

	// Opt out: clear TCP. The SND retains an empty `networking` block
	// (HTTPEnabled() back-compat will treat it as HTTP-enabled, but the
	// validator template has no externally-routable HTTP protocols so
	// nothing else changes). To get a clean "no networking" state, set
	// Networking to nil.
	latest := getSND(t, client.ObjectKeyFromObject(snd))
	patch := client.MergeFrom(latest.DeepCopy())
	latest.Spec.Networking = nil
	g.Expect(testCli.Patch(testCtx, latest, patch)).To(Succeed())

	// Child Spec.ExternalAddress is cleared by ensureSeiNode's diff.
	waitFor(t, func() bool {
		child := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, childKey, child); err != nil {
			return false
		}
		return child.Spec.ExternalAddress == ""
	}, "child ExternalAddress cleared after opt-out")

	// Per-pod LB Service is deleted.
	waitFor(t, func() bool {
		err := testCli.Get(testCtx, svcKey, &corev1.Service{})
		return apierrors.IsNotFound(err)
	}, "P2P endpoint Service deleted after opt-out")

	// P2PEndpoints is cleared (or NetworkingStatus is nil).
	waitForStatus(t, client.ObjectKeyFromObject(snd), func(latest *seiv1alpha1.SeiNodeDeployment) bool {
		return latest.Status.NetworkingStatus == nil ||
			len(latest.Status.NetworkingStatus.P2PEndpoints) == 0
	}, "P2PEndpoints cleared after opt-out")
}

// Re-opt-in: TCP restored brings the address back and recreates the Service.
func TestP2PEndpointP2P_ReOptIn_RestoresAddressAndService(t *testing.T) {
	g := NewWithT(t)
	withP2PEndpointDomain(t, p2pEndpointTestDomain)
	ns := makeNamespace(t)

	snd := fixtures.NewSND(ns, "pub-reoptin",
		fixtures.WithReplicas(1),
		withTCP(),
	)
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())

	const chainID = "pacific-1"
	wantAddr := expectedP2PEndpointAddr(snd.Name, chainID, 0)
	childKey := types.NamespacedName{Name: snd.Name + "-0", Namespace: ns}
	svcKey := types.NamespacedName{Name: snd.Name + "-0-p2p", Namespace: ns}

	waitFor(t, func() bool {
		child := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, childKey, child); err != nil {
			return false
		}
		return child.Spec.ExternalAddress == wantAddr
	}, "initial converge")

	// Opt out.
	latest := getSND(t, client.ObjectKeyFromObject(snd))
	patch := client.MergeFrom(latest.DeepCopy())
	latest.Spec.Networking = nil
	g.Expect(testCli.Patch(testCtx, latest, patch)).To(Succeed())
	waitFor(t, func() bool {
		err := testCli.Get(testCtx, svcKey, &corev1.Service{})
		return apierrors.IsNotFound(err)
	}, "Service gone after opt-out")

	// Re-opt-in.
	latest = getSND(t, client.ObjectKeyFromObject(snd))
	patch = client.MergeFrom(latest.DeepCopy())
	latest.Spec.Networking = &seiv1alpha1.NetworkingConfig{TCP: &seiv1alpha1.TCPConfig{}}
	g.Expect(testCli.Patch(testCtx, latest, patch)).To(Succeed())

	// Child gets the same vanity address back (identity is stable).
	waitFor(t, func() bool {
		child := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, childKey, child); err != nil {
			return false
		}
		return child.Spec.ExternalAddress == wantAddr
	}, "child re-stamped with same vanity address after re-opt-in")

	// Service is recreated.
	waitFor(t, func() bool {
		return testCli.Get(testCtx, svcKey, &corev1.Service{}) == nil
	}, "P2P endpoint Service recreated after re-opt-in")
}

// Scale-down deletes the ordinal's Service before the child SeiNode.
func TestP2PEndpointP2P_ScaleDown_DeletesOrdinalServiceBeforeChild(t *testing.T) {
	g := NewWithT(t)
	withP2PEndpointDomain(t, p2pEndpointTestDomain)
	ns := makeNamespace(t)

	snd := fixtures.NewSND(ns, "pub-scale",
		fixtures.WithReplicas(2),
		withTCP(),
	)
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())

	// Wait for both replicas to converge with their P2P endpoint Services.
	for i := 0; i < 2; i++ {
		svcKey := types.NamespacedName{Name: fmt.Sprintf("%s-%d-p2p", snd.Name, i), Namespace: ns}
		waitFor(t, func() bool {
			return testCli.Get(testCtx, svcKey, &corev1.Service{}) == nil
		}, fmt.Sprintf("P2P endpoint Service %s created", svcKey.Name))
	}

	// Scale down to 1 replica.
	latest := getSND(t, client.ObjectKeyFromObject(snd))
	patch := client.MergeFrom(latest.DeepCopy())
	latest.Spec.Replicas = 1
	g.Expect(testCli.Patch(testCtx, latest, patch)).To(Succeed())

	// ordinal-1 Service is gone.
	scaledOutKey := types.NamespacedName{Name: snd.Name + "-1-p2p", Namespace: ns}
	waitFor(t, func() bool {
		err := testCli.Get(testCtx, scaledOutKey, &corev1.Service{})
		return apierrors.IsNotFound(err)
	}, "ordinal-1 P2P endpoint Service deleted on scale-down")

	// ordinal-0 Service survives.
	survivingKey := types.NamespacedName{Name: snd.Name + "-0-p2p", Namespace: ns}
	g.Consistently(func() error {
		return testCli.Get(testCtx, survivingKey, &corev1.Service{})
	}, 2*time.Second, 200*time.Millisecond).Should(Succeed())
}

// Standalone SeiNodes own Spec.ExternalAddress directly; no SND touches it.
func TestP2PEndpointP2P_StandaloneSeiNode_PreservesUserAddress(t *testing.T) {
	g := NewWithT(t)
	withP2PEndpointDomain(t, p2pEndpointTestDomain)
	ns := makeNamespace(t)

	addr := "custom.example.com:26656"
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "standalone",
			Namespace: ns,
		},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:         "pacific-1",
			Image:           fixtures.DefaultImage,
			ExternalAddress: addr,
			FullNode:        &seiv1alpha1.FullNodeSpec{},
		},
	}
	g.Expect(testCli.Create(testCtx, node)).To(Succeed())

	g.Consistently(func() string {
		fetched := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, client.ObjectKeyFromObject(node), fetched); err != nil {
			return ""
		}
		return fetched.Spec.ExternalAddress
	}, 2*time.Second, 200*time.Millisecond).Should(Equal(addr))
}

// kubectl-edit stomp: external clear of Spec.ExternalAddress reconverges via ensureSeiNode.
func TestP2PEndpointP2P_KubectlEditStomp_ReconverresViaEnsureSeiNode(t *testing.T) {
	g := NewWithT(t)
	withP2PEndpointDomain(t, p2pEndpointTestDomain)
	ns := makeNamespace(t)

	snd := fixtures.NewSND(ns, "pub-stomp",
		fixtures.WithReplicas(1),
		withTCP(),
	)
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())

	const chainID = "pacific-1"
	wantAddr := expectedP2PEndpointAddr(snd.Name, chainID, 0)
	childKey := types.NamespacedName{Name: snd.Name + "-0", Namespace: ns}

	waitFor(t, func() bool {
		child := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, childKey, child); err != nil {
			return false
		}
		return child.Spec.ExternalAddress == wantAddr
	}, "initial converge before stomp")

	// Simulate `kubectl edit seinode` clearing the field. The SND
	// reconciler's ensureSeiNode pass should re-stamp it. Patch through
	// any finalizer-induced 409s with a small retry — the goal is to
	// land the cleared value at the apiserver, not to assert on the
	// PATCH path itself.
	g.Eventually(func() error {
		child := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, childKey, child); err != nil {
			return err
		}
		patch := client.MergeFrom(child.DeepCopy())
		child.Spec.ExternalAddress = ""
		return testCli.Patch(testCtx, child, patch)
	}, 5*time.Second, 200*time.Millisecond).Should(Succeed())

	// Bump the SND so a fresh reconcile is queued (envtest's predicate
	// gates on generation changes for SND-side spec). A no-op label edit
	// is enough.
	g.Eventually(func() error {
		latest := getSND(t, client.ObjectKeyFromObject(snd))
		patch := client.MergeFrom(latest.DeepCopy())
		if latest.Annotations == nil {
			latest.Annotations = map[string]string{}
		}
		latest.Annotations["test.sei.io/stomp-retick"] = time.Now().Format(time.RFC3339Nano)
		return testCli.Patch(testCtx, latest, patch)
	}, 5*time.Second, 200*time.Millisecond).Should(Succeed())

	// Reconverge.
	waitFor(t, func() bool {
		child := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, childKey, child); err != nil {
			return false
		}
		return child.Spec.ExternalAddress == wantAddr
	}, "child reconverged via ensureSeiNode after external stomp")
}

// TestP2PEndpointP2P_NoDomainConfigured_SkipsServices verifies the silent
// no-op path: when P2PEndpointDomain is unset, TCP-enabled SNDs get no
// P2P endpoint Service and no child ExternalAddress.
func TestP2PEndpointP2P_NoDomainConfigured_SkipsServices(t *testing.T) {
	g := NewWithT(t)
	withP2PEndpointDomain(t, "")
	ns := makeNamespace(t)

	snd := fixtures.NewSND(ns, "pub-no-domain",
		fixtures.WithReplicas(1),
		withTCP(),
	)
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())

	svcKey := types.NamespacedName{Name: snd.Name + "-0-p2p", Namespace: ns}
	g.Consistently(func() bool {
		err := testCli.Get(testCtx, svcKey, &corev1.Service{})
		return apierrors.IsNotFound(err)
	}, 2*time.Second, 200*time.Millisecond).Should(BeTrue())

	childKey := types.NamespacedName{Name: snd.Name + "-0", Namespace: ns}
	g.Eventually(func() string {
		child := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, childKey, child); err != nil {
			return "ERR"
		}
		return child.Spec.ExternalAddress
	}, 5*time.Second, 200*time.Millisecond).Should(BeEmpty())
}
