//go:build envtest

package envtest_test

import (
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/nodedeployment/envtest/fixtures"
)

const (
	// publishableTestDomain is the value the publishable tests pin onto
	// `testSNDReconciler.PublishableDomain` for the duration of each
	// test. The HTTP-route tests rely on the default empty value;
	// `withPublishabilityEnabled` restores both fields in t.Cleanup so
	// later tests see the unmodified suite state.
	publishableTestDomain = "test.platform.sei.io"
)

// expectedPublishableHost composes the deterministic vanity host the
// SND should stamp for a given SND name + child ordinal. Mirrors the
// production template (`<seinode>-p2p.<chainID>.<gatewayPublicDomain>`)
// so the tests fail loudly if either side drifts.
func expectedPublishableHost(sndName, chainID string, ordinal int) string {
	return fmt.Sprintf("%s-%d-p2p.%s.%s", sndName, ordinal, chainID, publishableTestDomain)
}

func expectedPublishableAddr(sndName, chainID string, ordinal int) string {
	return expectedPublishableHost(sndName, chainID, ordinal) + ":26656"
}

// withPublishabilityEnabled stamps the publishable test fixtures onto
// the shared SND reconciler and restores the original values when the
// test finishes. Tests that need a different capability state (e.g.
// VPCCIDRNotConfigured) call this with available=false.
func withPublishabilityEnabled(t *testing.T, available bool) {
	t.Helper()
	prevAvail := testSNDReconciler.PublishabilityAvailable
	prevDomain := testSNDReconciler.PublishableDomain
	testSNDReconciler.PublishabilityAvailable = available
	testSNDReconciler.PublishableDomain = publishableTestDomain
	t.Cleanup(func() {
		testSNDReconciler.PublishabilityAvailable = prevAvail
		testSNDReconciler.PublishableDomain = prevDomain
	})
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

// TestPublishableP2P_CreateWithTCP_ChildHasAddressAndServiceExists is
// the happy path. An SND with `networking.tcp` set should produce: a
// child SeiNode whose `Spec.ExternalAddress` matches the deterministic
// vanity host, a per-pod LB Service with the right annotations, and an
// entry in `Status.NetworkingStatus.PublishableEndpoints`.
func TestPublishableP2P_CreateWithTCP_ChildHasAddressAndServiceExists(t *testing.T) {
	g := NewWithT(t)
	withPublishabilityEnabled(t, true)
	ns := makeNamespace(t)

	snd := fixtures.NewSND(ns, "pub-create",
		fixtures.WithReplicas(1),
		withTCP(),
	)
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())

	const chainID = "pacific-1" // fixtures default
	wantHost := expectedPublishableHost(snd.Name, chainID, 0)
	wantAddr := expectedPublishableAddr(snd.Name, chainID, 0)

	// 1. Child SeiNode is created with Spec.ExternalAddress already set.
	//    The brief calls out "child appears in the API with the value
	//    already populated" — this poll asserts the no-race-window claim.
	childKey := types.NamespacedName{Name: snd.Name + "-0", Namespace: ns}
	waitFor(t, func() bool {
		child := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, childKey, child); err != nil {
			return false
		}
		return child.Spec.ExternalAddress != nil && *child.Spec.ExternalAddress == wantAddr
	}, "child SeiNode "+childKey.Name+" populated with publishable ExternalAddress")

	// 2. Per-pod LB Service exists with the expected annotations.
	svcKey := types.NamespacedName{Name: snd.Name + "-0-p2p", Namespace: ns}
	waitFor(t, func() bool {
		svc := &corev1.Service{}
		return testCli.Get(testCtx, svcKey, svc) == nil
	}, "publishable Service "+svcKey.Name+" created")

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

	// 4. PublishableEndpoints surfaces the same hostname the child got.
	waitForStatus(t, client.ObjectKeyFromObject(snd), func(latest *seiv1alpha1.SeiNodeDeployment) bool {
		ns := latest.Status.NetworkingStatus
		if ns == nil || len(ns.PublishableEndpoints) != 1 {
			return false
		}
		ep := ns.PublishableEndpoints[0]
		return ep.Ordinal == 0 &&
			ep.SeiNodeName == snd.Name+"-0" &&
			ep.Hostname == wantAddr
	}, "PublishableEndpoints stamped with deterministic vanity host")
}

// TestPublishableP2P_OptOut_ClearsAddressAndDeletesService asserts the
// opt-out lifecycle: removing TCP from `spec.networking` clears the
// child's `Spec.ExternalAddress` and deletes the per-pod LB Service.
// ensureSeiNode is the single write site for the child field, so this
// validates the diff propagation path end-to-end.
func TestPublishableP2P_OptOut_ClearsAddressAndDeletesService(t *testing.T) {
	g := NewWithT(t)
	withPublishabilityEnabled(t, true)
	ns := makeNamespace(t)

	snd := fixtures.NewSND(ns, "pub-optout",
		fixtures.WithReplicas(1),
		withTCP(),
	)
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())

	const chainID = "pacific-1"
	wantAddr := expectedPublishableAddr(snd.Name, chainID, 0)
	childKey := types.NamespacedName{Name: snd.Name + "-0", Namespace: ns}
	svcKey := types.NamespacedName{Name: snd.Name + "-0-p2p", Namespace: ns}

	// Wait for converged publishable state.
	waitFor(t, func() bool {
		child := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, childKey, child); err != nil {
			return false
		}
		return child.Spec.ExternalAddress != nil && *child.Spec.ExternalAddress == wantAddr
	}, "child has publishable address before opt-out")
	waitFor(t, func() bool {
		return testCli.Get(testCtx, svcKey, &corev1.Service{}) == nil
	}, "publishable Service exists before opt-out")

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
		return child.Spec.ExternalAddress == nil || *child.Spec.ExternalAddress == ""
	}, "child ExternalAddress cleared after opt-out")

	// Per-pod LB Service is deleted.
	waitFor(t, func() bool {
		err := testCli.Get(testCtx, svcKey, &corev1.Service{})
		return apierrors.IsNotFound(err)
	}, "publishable Service deleted after opt-out")

	// PublishableEndpoints is cleared (or NetworkingStatus is nil).
	waitForStatus(t, client.ObjectKeyFromObject(snd), func(latest *seiv1alpha1.SeiNodeDeployment) bool {
		return latest.Status.NetworkingStatus == nil ||
			len(latest.Status.NetworkingStatus.PublishableEndpoints) == 0
	}, "PublishableEndpoints cleared after opt-out")
}

// TestPublishableP2P_ReOptIn_RestoresAddressAndService is the dual of
// the opt-out test: re-adding TCP after a clear should bring the child
// back to the same vanity hostname (deterministic) and recreate the
// per-pod LB Service.
func TestPublishableP2P_ReOptIn_RestoresAddressAndService(t *testing.T) {
	g := NewWithT(t)
	withPublishabilityEnabled(t, true)
	ns := makeNamespace(t)

	snd := fixtures.NewSND(ns, "pub-reoptin",
		fixtures.WithReplicas(1),
		withTCP(),
	)
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())

	const chainID = "pacific-1"
	wantAddr := expectedPublishableAddr(snd.Name, chainID, 0)
	childKey := types.NamespacedName{Name: snd.Name + "-0", Namespace: ns}
	svcKey := types.NamespacedName{Name: snd.Name + "-0-p2p", Namespace: ns}

	waitFor(t, func() bool {
		child := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, childKey, child); err != nil {
			return false
		}
		return child.Spec.ExternalAddress != nil && *child.Spec.ExternalAddress == wantAddr
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
		return child.Spec.ExternalAddress != nil && *child.Spec.ExternalAddress == wantAddr
	}, "child re-stamped with same vanity address after re-opt-in")

	// Service is recreated.
	waitFor(t, func() bool {
		return testCli.Get(testCtx, svcKey, &corev1.Service{}) == nil
	}, "publishable Service recreated after re-opt-in")
}

// TestPublishableP2P_ScaleDown_DeletesOrdinalServiceBeforeChild guards
// the invariant called out in the brief: on scale-down the ordinal's
// publishable Service must be deleted before the child SeiNode so the
// NLB never sits in the no-healthy-targets-but-still-allocated state.
//
// envtest is happy-path here — both deletions are issued by the same
// reconcile pass, so we mainly assert both objects converge to gone.
// The ordering predicate runs inside `scaleDown`.
func TestPublishableP2P_ScaleDown_DeletesOrdinalServiceBeforeChild(t *testing.T) {
	g := NewWithT(t)
	withPublishabilityEnabled(t, true)
	ns := makeNamespace(t)

	snd := fixtures.NewSND(ns, "pub-scale",
		fixtures.WithReplicas(2),
		withTCP(),
	)
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())

	// Wait for both replicas to converge with their publishable Services.
	for i := 0; i < 2; i++ {
		svcKey := types.NamespacedName{Name: fmt.Sprintf("%s-%d-p2p", snd.Name, i), Namespace: ns}
		waitFor(t, func() bool {
			return testCli.Get(testCtx, svcKey, &corev1.Service{}) == nil
		}, fmt.Sprintf("publishable Service %s created", svcKey.Name))
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
	}, "ordinal-1 publishable Service deleted on scale-down")

	// ordinal-0 Service survives.
	survivingKey := types.NamespacedName{Name: snd.Name + "-0-p2p", Namespace: ns}
	g.Consistently(func() error {
		return testCli.Get(testCtx, survivingKey, &corev1.Service{})
	}, 2*time.Second, 200*time.Millisecond).Should(Succeed())
}

// TestPublishableP2P_StandaloneSeiNode_PreservesUserAddress asserts the
// brief's "no SND involvement" contract. A SeiNode created directly by
// a user with `Spec.ExternalAddress` set should keep that value through
// reconcile — no SND-side code path should clear it, because no SND
// owns it. The planner emits it verbatim. The check is interface-only
// (we read back the spec); the planner emit-side is covered by the
// unit test in `internal/planner/common_overrides_test.go`.
func TestPublishableP2P_StandaloneSeiNode_PreservesUserAddress(t *testing.T) {
	g := NewWithT(t)
	withPublishabilityEnabled(t, true)
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
			ExternalAddress: &addr,
			FullNode:        &seiv1alpha1.FullNodeSpec{},
		},
	}
	g.Expect(testCli.Create(testCtx, node)).To(Succeed())

	// Read back: the spec persists. No SND code path should touch this.
	g.Consistently(func() *string {
		fetched := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, client.ObjectKeyFromObject(node), fetched); err != nil {
			return nil
		}
		return fetched.Spec.ExternalAddress
	}, 2*time.Second, 200*time.Millisecond).Should(And(
		Not(BeNil()),
		HaveValue(Equal(addr)),
	))
}

// TestPublishableP2P_KubectlEditStomp_ReconverresViaEnsureSeiNode
// exercises the brief's stomp scenario: an external write clears the
// child's `Spec.ExternalAddress` mid-flight. ensureSeiNode must detect
// the diff on next reconcile and restore the SND-managed value. This
// is the load-bearing claim that the field truly converges through
// the single write site.
func TestPublishableP2P_KubectlEditStomp_ReconverresViaEnsureSeiNode(t *testing.T) {
	g := NewWithT(t)
	withPublishabilityEnabled(t, true)
	ns := makeNamespace(t)

	snd := fixtures.NewSND(ns, "pub-stomp",
		fixtures.WithReplicas(1),
		withTCP(),
	)
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())

	const chainID = "pacific-1"
	wantAddr := expectedPublishableAddr(snd.Name, chainID, 0)
	childKey := types.NamespacedName{Name: snd.Name + "-0", Namespace: ns}

	waitFor(t, func() bool {
		child := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, childKey, child); err != nil {
			return false
		}
		return child.Spec.ExternalAddress != nil && *child.Spec.ExternalAddress == wantAddr
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
		child.Spec.ExternalAddress = nil
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
		return child.Spec.ExternalAddress != nil && *child.Spec.ExternalAddress == wantAddr
	}, "child reconverged via ensureSeiNode after external stomp")
}

// TestPublishableP2P_VPCCIDRNotConfigured_SurfacesConditionAndSkipsService
// is the capability-disabled branch. With `PublishabilityAvailable=false`
// and a TCP-enabled SND, the reconciler must:
//   - set ConditionNetworkingReady=False/VPCCIDRNotConfigured
//   - not create a publishable Service
//   - leave the child's Spec.ExternalAddress nil/empty
//
// This is the equivalent of an operator misconfigured the controller
// (SEI_VPC_CIDR missing). The condition is the operator's debugging
// signal.
func TestPublishableP2P_VPCCIDRNotConfigured_SurfacesConditionAndSkipsService(t *testing.T) {
	g := NewWithT(t)
	withPublishabilityEnabled(t, false) // capability OFF for this test
	ns := makeNamespace(t)

	snd := fixtures.NewSND(ns, "pub-no-cidr",
		fixtures.WithReplicas(1),
		withTCP(),
	)
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())

	// Condition surfaces the misconfiguration.
	waitForStatus(t, client.ObjectKeyFromObject(snd), func(latest *seiv1alpha1.SeiNodeDeployment) bool {
		c := apimeta.FindStatusCondition(latest.Status.Conditions, seiv1alpha1.ConditionNetworkingReady)
		return c != nil && c.Status == metav1.ConditionFalse && c.Reason == "VPCCIDRNotConfigured"
	}, "ConditionNetworkingReady=False/VPCCIDRNotConfigured")

	// No publishable Service.
	svcKey := types.NamespacedName{Name: snd.Name + "-0-p2p", Namespace: ns}
	g.Consistently(func() bool {
		err := testCli.Get(testCtx, svcKey, &corev1.Service{})
		return apierrors.IsNotFound(err)
	}, 2*time.Second, 200*time.Millisecond).Should(BeTrue(),
		"no publishable Service created when capability is unavailable")

	// Child SeiNode has no ExternalAddress.
	childKey := types.NamespacedName{Name: snd.Name + "-0", Namespace: ns}
	g.Eventually(func() error {
		child := &seiv1alpha1.SeiNode{}
		return testCli.Get(testCtx, childKey, child)
	}, 5*time.Second, 200*time.Millisecond).Should(Succeed())
	child := &seiv1alpha1.SeiNode{}
	g.Expect(testCli.Get(testCtx, childKey, child)).To(Succeed())
	if child.Spec.ExternalAddress != nil {
		g.Expect(*child.Spec.ExternalAddress).To(BeEmpty(),
			"child ExternalAddress must be empty without capability")
	}
}

// TestPublishableP2P_PublishableDomainNotConfigured_SurfacesCondition is the
// L4-zone-not-configured branch. With `PublishabilityAvailable=true` (VPC
// CIDR set) but `PublishableDomain=""` (no SEI_PUBLISHABLE_DOMAIN), a
// TCP-enabled SND must surface `PublishableDomainNotConfigured` rather
// than getting silently misclassified as a VPC-CIDR or chain-ID problem.
// Real-world trigger: dev cluster, where SEI_PUBLISHABLE_DOMAIN is unset.
func TestPublishableP2P_PublishableDomainNotConfigured_SurfacesCondition(t *testing.T) {
	g := NewWithT(t)
	// Cap available, domain absent: pin both fields explicitly.
	prevAvail := testSNDReconciler.PublishabilityAvailable
	prevDomain := testSNDReconciler.PublishableDomain
	testSNDReconciler.PublishabilityAvailable = true
	testSNDReconciler.PublishableDomain = ""
	t.Cleanup(func() {
		testSNDReconciler.PublishabilityAvailable = prevAvail
		testSNDReconciler.PublishableDomain = prevDomain
	})

	ns := makeNamespace(t)
	snd := fixtures.NewSND(ns, "pub-no-domain",
		fixtures.WithReplicas(1),
		withTCP(),
	)
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())

	waitForStatus(t, client.ObjectKeyFromObject(snd), func(latest *seiv1alpha1.SeiNodeDeployment) bool {
		c := apimeta.FindStatusCondition(latest.Status.Conditions, seiv1alpha1.ConditionNetworkingReady)
		return c != nil && c.Status == metav1.ConditionFalse && c.Reason == "PublishableDomainNotConfigured"
	}, "ConditionNetworkingReady=False/PublishableDomainNotConfigured")

	// No publishable Service.
	svcKey := types.NamespacedName{Name: snd.Name + "-0-p2p", Namespace: ns}
	g.Consistently(func() bool {
		err := testCli.Get(testCtx, svcKey, &corev1.Service{})
		return apierrors.IsNotFound(err)
	}, 2*time.Second, 200*time.Millisecond).Should(BeTrue(),
		"no publishable Service created when domain is unconfigured")
}
