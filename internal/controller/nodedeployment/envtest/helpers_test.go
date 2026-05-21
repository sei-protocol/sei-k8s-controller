//go:build envtest

package envtest_test

import (
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1 "k8s.io/api/core/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	pollInterval = 200 * time.Millisecond
	pollTimeout  = 30 * time.Second
)

// waitFor polls cond every pollInterval up to pollTimeout. Fails the test
// with msg if cond never returns true. The condition is invoked with the
// shared testCtx so cancellation during teardown short-circuits the loop.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(pollTimeout)
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
	t.Fatalf("timeout after %s waiting for: %s", pollTimeout, msg)
}

// waitForStatus polls until the SND identified by key satisfies predicate.
// Resource-not-found is treated as predicate-false (the controller may not
// have produced the object yet on the very first iteration).
func waitForStatus(t *testing.T, key client.ObjectKey, predicate func(*seiv1alpha1.SeiNodeDeployment) bool, msg string) {
	t.Helper()
	waitFor(t, func() bool {
		snd := &seiv1alpha1.SeiNodeDeployment{}
		if err := testCli.Get(testCtx, key, snd); err != nil {
			if apierrors.IsNotFound(err) {
				return false
			}
			t.Logf("waitForStatus get error: %v", err)
			return false
		}
		return predicate(snd)
	}, msg)
}

// makeNamespace creates a uniquely-suffixed Namespace and registers a
// t.Cleanup that deletes both the namespace and any child SeiNodes that
// outlived their owning SND. envtest has no kube-controller-manager, so
// owner-cascade may not finalize on its own.
func makeNamespace(t *testing.T) string {
	t.Helper()
	g := NewWithT(t)

	name := fmt.Sprintf("envtest-%s", rand.String(8))
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	g.Expect(testCli.Create(testCtx, ns)).To(Succeed())

	t.Cleanup(func() {
		// Best-effort: list all SeiNodes in the namespace and remove
		// finalizers so the apiserver can fully reap them. The SeiNode
		// controller isn't wired in this harness; its finalizer would
		// otherwise pin every child indefinitely.
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

		snds := &seiv1alpha1.SeiNodeDeploymentList{}
		if err := testCli.List(testCtx, snds, client.InNamespace(name)); err == nil {
			for i := range snds.Items {
				snd := &snds.Items[i]
				if len(snd.Finalizers) > 0 {
					patch := client.MergeFrom(snd.DeepCopy())
					snd.Finalizers = nil
					_ = testCli.Patch(testCtx, snd, patch)
				}
				_ = testCli.Delete(testCtx, snd)
			}
		}

		_ = testCli.Delete(testCtx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	})

	return name
}

// getSND fetches the latest copy of the SND identified by key.
func getSND(t *testing.T, key client.ObjectKey) *seiv1alpha1.SeiNodeDeployment {
	t.Helper()
	g := NewWithT(t)
	snd := &seiv1alpha1.SeiNodeDeployment{}
	g.Expect(testCli.Get(testCtx, key, snd)).To(Succeed())
	return snd
}

// patchSNDImage updates spec.template.spec.image on the given SND using a
// strategic-merge patch from the pre-mutated copy.
func patchSNDImage(t *testing.T, snd *seiv1alpha1.SeiNodeDeployment, newImage string) {
	t.Helper()
	g := NewWithT(t)
	patch := client.MergeFrom(snd.DeepCopy())
	snd.Spec.Template.Spec.Image = newImage
	g.Expect(testCli.Patch(testCtx, snd, patch)).To(Succeed())
}

// listChildren returns the SeiNodes whose controller owner-ref points at
// the given SND. Sorted by name for stable assertions.
func listChildren(t *testing.T, snd *seiv1alpha1.SeiNodeDeployment) []seiv1alpha1.SeiNode {
	t.Helper()
	g := NewWithT(t)
	list := &seiv1alpha1.SeiNodeList{}
	g.Expect(testCli.List(testCtx, list, client.InNamespace(snd.Namespace))).To(Succeed())
	out := make([]seiv1alpha1.SeiNode, 0, len(list.Items))
	for i := range list.Items {
		if metav1.IsControlledBy(&list.Items[i], snd) {
			out = append(out, list.Items[i])
		}
	}
	return out
}

// condTrue reports whether condType is present and True on the SND.
func condTrue(snd *seiv1alpha1.SeiNodeDeployment, condType string) bool {
	c := apimeta.FindStatusCondition(snd.Status.Conditions, condType)
	return c != nil && c.Status == metav1.ConditionTrue
}

// listEventsForSND returns events whose InvolvedObject UID matches the SND
// and (optionally) whose Reason matches the filter. Empty reason returns
// every event for the SND.
func listEventsForSND(t *testing.T, snd *seiv1alpha1.SeiNodeDeployment, reason string) []corev1.Event {
	t.Helper()
	list := &corev1.EventList{}
	if err := testCli.List(testCtx, list, client.InNamespace(snd.Namespace)); err != nil {
		t.Logf("listEventsForSND list error: %v", err)
		return nil
	}
	out := make([]corev1.Event, 0, len(list.Items))
	for i := range list.Items {
		e := list.Items[i]
		if e.InvolvedObject.UID != snd.UID {
			continue
		}
		if reason != "" && e.Reason != reason {
			continue
		}
		out = append(out, e)
	}
	return out
}
