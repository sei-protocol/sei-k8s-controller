//go:build envtest

package envtest_test

import (
	"strconv"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/nodedeployment/envtest/fixtures"
)

// TestInPlaceRollout_EndToEnd exercises the SND controller's rollout
// detection and child-spec propagation against a real apiserver, with
// the SeiNode controller wired in alongside. The test now drives the
// full lifecycle to terminal state:
//
// Pre-rollout (SND-side only):
//   - child SeiNode creation + owner references
//   - status.templateHash on first converge
//
// Rollout-in-flight:
//   - status.rollout populated + RolloutInProgress condition True
//
// Rollout-complete (requires SeiNode controller + StatefulSet faker):
//   - child SeiNode.Status.CurrentImage flips to new image
//   - child spec.image propagation
//   - SND status.plan reaches TaskPlanComplete then is cleared
//   - status.rollout cleared after plan completes
//   - RolloutInProgress condition transitions True → False with reason
//     RolloutComplete
//
// Teardown:
//   - SND delete + finalizer cleanup
//
// The StatefulSet faker (StartStatefulSetStatusFaker in suite_test.go)
// patches every STS's .Status to look "fully rolled out at current
// generation," collapsing what would otherwise be a kubelet-dependent
// dance to instantaneous. ReplacePod and ObserveImage gate on those
// fields; with the faker running, they complete immediately.
func TestInPlaceRollout_EndToEnd(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	const (
		oldImage = "ghcr.io/sei-protocol/seid:v1.0.0"
		newImage = "ghcr.io/sei-protocol/seid:v2.0.0"
		replicas = 3
	)

	snd := fixtures.NewSND(ns, "rollout-test",
		fixtures.WithReplicas(replicas),
		fixtures.WithImage(oldImage),
	)
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())
	key := client.ObjectKeyFromObject(snd)

	// 1. Children are created with the correct controller owner reference.
	waitFor(t, func() bool {
		latest := &seiv1alpha1.SeiNodeDeployment{}
		if err := testCli.Get(testCtx, key, latest); err != nil {
			return false
		}
		kids := listChildren(t, latest)
		if len(kids) != replicas {
			return false
		}
		for i := range kids {
			refs := kids[i].GetOwnerReferences()
			if len(refs) == 0 {
				return false
			}
			if refs[0].UID != latest.UID {
				return false
			}
			if refs[0].Controller == nil || !*refs[0].Controller {
				return false
			}
			want := map[string]string{
				"sei.io/nodedeployment":         "rollout-test",
				"sei.io/nodedeployment-ordinal": strconv.Itoa(i),
				"sei.io/chain":                  "pacific-1",
			}
			labels := kids[i].GetLabels()
			ok := true
			for k, v := range want {
				if labels[k] != v {
					t.Logf("child %s: label %s = %q, want %q", kids[i].GetName(), k, labels[k], v)
					ok = false
				}
			}
			if labels["sei.io/revision"] == "" {
				t.Logf("child %s: sei.io/revision empty", kids[i].GetName())
				ok = false
			}
			if !ok {
				return false
			}
		}
		return true
	}, "3 child SeiNodes owned by the SND with sei.io/chain stamped")

	// 2. Status converges: templateHash populated, replicas reported,
	//    no rollout in flight on the steady-state spec.
	waitForStatus(t, key, func(latest *seiv1alpha1.SeiNodeDeployment) bool {
		if latest.Status.TemplateHash == "" {
			return false
		}
		if latest.Status.Replicas != replicas {
			return false
		}
		// Before any image churn, rollout must not exist and the
		// condition (if present at all) must not be True.
		if latest.Status.Rollout != nil {
			return false
		}
		return !condTrue(latest, seiv1alpha1.ConditionRolloutInProgress)
	}, "templateHash set, replicas=3, no rollout in progress")

	initial := getSND(t, key)
	initialHash := initial.Status.TemplateHash
	g.Expect(initialHash).ToNot(BeEmpty())

	// 3. Patch image and watch the rollout start.
	patchSNDImage(t, initial, newImage)

	waitForStatus(t, key, func(latest *seiv1alpha1.SeiNodeDeployment) bool {
		if latest.Status.Rollout == nil {
			return false
		}
		if latest.Status.Rollout.TargetHash == "" || latest.Status.Rollout.TargetHash == initialHash {
			return false
		}
		return condTrue(latest, seiv1alpha1.ConditionRolloutInProgress)
	}, "Status.Rollout populated with new TargetHash and RolloutInProgress=True")

	// 4. The new image propagates to every child SeiNode. This happens
	//    either via ensureSeiNode's direct spec patch or via the
	//    UpdateNodeSpecs task in the rollout plan — both reach the
	//    same end state from the test's perspective.
	waitFor(t, func() bool {
		latest := getSND(t, key)
		kids := listChildren(t, latest)
		if len(kids) != replicas {
			return false
		}
		for i := range kids {
			if kids[i].Spec.Image != newImage {
				return false
			}
		}
		return true
	}, "all 3 children have spec.image == "+newImage)

	// 5. Each child SeiNode's NodeUpdate plan stamps Status.CurrentImage
	//    via the observe-image task once its StatefulSet rollout is
	//    observed complete. The faker drives the rollout to terminal
	//    instantly; under that, observe-image fires on the next
	//    reconcile after apply-statefulset.
	waitFor(t, func() bool {
		latest := getSND(t, key)
		kids := listChildren(t, latest)
		if len(kids) != replicas {
			return false
		}
		for i := range kids {
			if kids[i].Status.CurrentImage != newImage {
				return false
			}
		}
		return true
	}, "all 3 children have status.currentImage == "+newImage)

	// 6. The SND-side AwaitSpecUpdate task polls each child's
	//    Status.CurrentImage. Once all match, the plan completes,
	//    completePlan clears Status.Rollout, and sets
	//    RolloutInProgress=False/RolloutComplete.
	waitForStatus(t, key, func(latest *seiv1alpha1.SeiNodeDeployment) bool {
		// Plan cleared (completePlan nils Status.Plan after the
		// terminal observation in the next reconcile).
		if latest.Status.Plan != nil {
			return false
		}
		if latest.Status.Rollout != nil {
			return false
		}
		cond := apimeta.FindStatusCondition(latest.Status.Conditions, seiv1alpha1.ConditionRolloutInProgress)
		if cond == nil {
			return false
		}
		if cond.Status != metav1.ConditionFalse {
			return false
		}
		return cond.Reason == "RolloutComplete"
	}, "rollout plan complete, status.rollout cleared, RolloutInProgress=False/RolloutComplete")

	// 6b. No post-rollout churn. The major-upgrade scenario depends on the
	//     SND template being the single source of child image: once the
	//     rollout settles, the controller must stop touching child spec, so
	//     child .metadata.generation holds steady. A regression that
	//     re-asserts a drifting child image (e.g. fighting an external
	//     per-child patcher) would keep bumping generation here.
	settled := getSND(t, key)
	gens := map[string]int64{}
	for _, kid := range listChildren(t, settled) {
		gens[kid.Name] = kid.Generation
	}
	g.Expect(gens).To(HaveLen(replicas))
	g.Consistently(func() bool {
		for _, kid := range listChildren(t, getSND(t, key)) {
			if kid.Generation != gens[kid.Name] {
				t.Logf("child %s generation churned: %d -> %d", kid.Name, gens[kid.Name], kid.Generation)
				return false
			}
		}
		return true
	}, 3*time.Second, pollInterval).Should(BeTrue(),
		"child generations hold steady after rollout (no flip-flop churn)")

	// 7. Deleting the SND removes all children. envtest has no kube-
	//    controller-manager to perform garbage-collection by owner-ref,
	//    so the SND controller's finalizer path is what we exercise:
	//    it observes DeletionTimestamp, runs the DeletionPolicy=Delete
	//    branch (default), and removes the finalizer. Children are
	//    deleted by the apiserver's foreground cascade — but again,
	//    no GC controller runs. We strip the SeiNode finalizers
	//    proactively in t.Cleanup; here we just confirm the SND itself
	//    is gone (its own deletion is finalizer-gated by the controller).
	g.Expect(testCli.Delete(testCtx, getSND(t, key))).To(Succeed())

	waitFor(t, func() bool {
		latest := &seiv1alpha1.SeiNodeDeployment{}
		err := testCli.Get(testCtx, key, latest)
		return apierrors.IsNotFound(err)
	}, "SND removed after delete + finalizer cleanup")

	// Sanity: belt-and-suspenders timing log so a future maintainer
	// can spot a regression that pushes a previously sub-second flow
	// up against the pollTimeout.
	t.Logf("rollout test completed at %s", time.Now().Format(time.RFC3339Nano))
}
