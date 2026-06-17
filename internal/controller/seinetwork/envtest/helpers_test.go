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
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	pollInterval = 200 * time.Millisecond
	pollTimeout  = 30 * time.Second
)

// waitFor polls cond every pollInterval up to pollTimeout.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	waitForWithin(t, pollTimeout, cond, msg)
}

// waitForWithin is waitFor with an explicit timeout, for waits whose
// convergence chain is known to exceed the shared pollTimeout (e.g. the
// genesis ceremony + initial boot + the child NodeUpdate plan that
// backfills status.currentImage — see convergeTimeout).
func waitForWithin(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		select {
		case <-testCtx.Done():
			t.Fatalf("context cancelled while waiting: %s", msg)
		case <-time.After(pollInterval):
		}
	}
	t.Fatalf("timeout after %s waiting for: %s", timeout, msg)
}

// waitForStatus polls until the SeiNetwork identified by key satisfies predicate.
func waitForStatus(t *testing.T, key client.ObjectKey, predicate func(*seiv1alpha1.SeiNetwork) bool, msg string) {
	t.Helper()
	waitForStatusWithin(t, pollTimeout, key, predicate, msg)
}

// waitForStatusWithin is waitForStatus with an explicit timeout, for status
// predicates whose convergence chain exceeds the shared pollTimeout.
func waitForStatusWithin(t *testing.T, timeout time.Duration, key client.ObjectKey, predicate func(*seiv1alpha1.SeiNetwork) bool, msg string) {
	t.Helper()
	waitForWithin(t, timeout, func() bool {
		network := &seiv1alpha1.SeiNetwork{}
		if err := testCli.Get(testCtx, key, network); err != nil {
			if apierrors.IsNotFound(err) {
				return false
			}
			t.Logf("waitForStatus get error: %v", err)
			return false
		}
		return predicate(network)
	}, msg)
}

// makeNamespace creates a uniquely-suffixed Namespace and registers a
// t.Cleanup that removes child SeiNodes and SeiNetworks (envtest has no
// kube-controller-manager, so owner-cascade may not finalize on its own).
func makeNamespace(t *testing.T) string {
	t.Helper()
	g := NewWithT(t)

	name := fmt.Sprintf("envtest-%s", rand.String(8))
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	g.Expect(testCli.Create(testCtx, ns)).To(Succeed())

	t.Cleanup(func() {
		nodes := &seiv1alpha1.SeiNodeList{}
		if err := testCli.List(testCtx, nodes, client.InNamespace(name)); err == nil {
			for i := range nodes.Items {
				node := &nodes.Items[i]
				if len(node.Finalizers) > 0 {
					patch := client.MergeFrom(node.DeepCopy())
					node.Finalizers = nil
					_ = testCli.Patch(testCtx, node, patch)
				}
				_ = testCli.Delete(testCtx, node)
			}
		}

		networks := &seiv1alpha1.SeiNetworkList{}
		if err := testCli.List(testCtx, networks, client.InNamespace(name)); err == nil {
			for i := range networks.Items {
				network := &networks.Items[i]
				if len(network.Finalizers) > 0 {
					patch := client.MergeFrom(network.DeepCopy())
					network.Finalizers = nil
					_ = testCli.Patch(testCtx, network, patch)
				}
				_ = testCli.Delete(testCtx, network)
			}
		}

		_ = testCli.Delete(testCtx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	})

	return name
}

// getNetwork fetches the latest copy of the SeiNetwork identified by key.
func getNetwork(t *testing.T, key client.ObjectKey) *seiv1alpha1.SeiNetwork {
	t.Helper()
	g := NewWithT(t)
	network := &seiv1alpha1.SeiNetwork{}
	g.Expect(testCli.Get(testCtx, key, network)).To(Succeed())
	return network
}

// patchNetworkImage updates spec.image on the given SeiNetwork.
func patchNetworkImage(t *testing.T, network *seiv1alpha1.SeiNetwork, newImage string) {
	t.Helper()
	g := NewWithT(t)
	patch := client.MergeFrom(network.DeepCopy())
	network.Spec.Image = newImage
	g.Expect(testCli.Patch(testCtx, network, patch)).To(Succeed())
}

// listChildren returns the SeiNodes whose controller owner-ref points at
// the given SeiNetwork. Sorted by name for stable assertions.
func listChildren(t *testing.T, network *seiv1alpha1.SeiNetwork) []seiv1alpha1.SeiNode {
	t.Helper()
	g := NewWithT(t)
	list := &seiv1alpha1.SeiNodeList{}
	g.Expect(testCli.List(testCtx, list, client.InNamespace(network.Namespace))).To(Succeed())
	out := make([]seiv1alpha1.SeiNode, 0, len(list.Items))
	for i := range list.Items {
		if metav1.IsControlledBy(&list.Items[i], network) {
			out = append(out, list.Items[i])
		}
	}
	return out
}

// reasonAllUpToDate is the derived RolloutInProgress reason at steady state
// (every child reports spec.image).
const reasonAllUpToDate = "AllUpToDate"
