//go:build envtest

package envtest_test

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/seinetwork/envtest/fixtures"
)

// TestInPlaceRollout_StuckUntilUnstuck pins the operator-visible
// "stuck rollout" shape: when AwaitSpecUpdate cannot observe child
// convergence (sidecar wedged, StatefulSet status not advancing,
// pod replacement gated by a slow scheduler, etc.), the controller
// does not auto-complete or false-signal success. The forthcoming
// Paused feature must remain observably distinguishable from this
// shape on every field below.
//
// Stuck vs Paused contrast matrix (anchors the Paused feature's
// CRD/status contract):
//
//	Field                          Stuck             Paused (target shape)
//	─────────────────────────────  ────────────────  ────────────────────────────
//	Status.Phase                   Upgrading         Paused (or Ready with Cond)
//	Status.Rollout                 != nil            nil
//	Status.Plan                    != nil, Active    nil
//	ConditionRolloutInProgress     True              False or absent
//	ConditionPlanInProgress        True              False or absent
//	ConditionPaused                absent            True
//	Status.ReadyReplicas           == replicas       == replicas
//	ConditionNodesReady            True              True
//	Status.Rollout.StartedAt       stable (no rest)  n/a
//	RolloutStarted event           fired             never (no rollout begun)
//	RolloutComplete event          never fires       n/a
//	Child Spec.Image               new image         old image (no propagation)
//
// The "Phase=Upgrading + RolloutInProgress=True + ReadyReplicas=replicas"
// triple is the on-call's tell for stuck: existing pods are healthy,
// but the controller hasn't observed the rollout completing.
//
// Production-recovery equivalent: an operator unwedges the sidecar
// (kubectl delete pod, fix the image, etc.), at which point ObserveImage
// progresses and the rollout completes on its own. The test models this
// by Resume()-ing the StatefulSet status faker.
func TestInPlaceRollout_StuckUntilUnstuck(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	const (
		v1       = "ghcr.io/sei-protocol/seid:v1.0.0"
		v2       = "ghcr.io/sei-protocol/seid:v2.0.0"
		replicas = 2
	)

	snd := fixtures.NewNetwork(ns, "stuck",
		fixtures.WithReplicas(replicas),
		fixtures.WithImage(v1),
	)
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())
	key := client.ObjectKeyFromObject(snd)

	// Initial v1 deployment reaches steady state. The SND-level gate
	// (TemplateHash set, no in-flight Rollout) fires as soon as the
	// init plan ends at mark-ready — which does NOT include observe-image.
	// For a fresh InPlace SND with no Genesis, Status.CurrentImage on each
	// child is stamped only later, via the steady-state drift NodeUpdate
	// that runs observe-image. If we Pause the faker before that drift
	// pass observes the StatefulSet, observe-image stalls forever on
	// ObservedGeneration < Generation and Status.CurrentImage stays "".
	// Wait at the child level: every child must have Status.CurrentImage
	// == v1 before the v2 patch lands.
	waitFor(t, func() bool {
		kids := listChildren(t, getNetwork(t, key))
		if len(kids) != replicas {
			return false
		}
		for i := range kids {
			if kids[i].Status.CurrentImage != v1 {
				return false
			}
		}
		return true
	}, "all children Status.CurrentImage stamped at v1 before pausing faker")

	// Pause the faker. StatefulSet .Status freezes, ObserveImage stalls,
	// AwaitSpecUpdate stalls behind it. The Cleanup ensures other tests
	// run on a working harness even if this one fails partway.
	testFaker.Pause()
	t.Cleanup(testFaker.Resume)

	// Trigger the rollout.
	patchNetworkImage(t, getNetwork(t, key), v2)

	// Wait for the rollout to register.
	waitForStatus(t, key, func(latest *seiv1alpha1.SeiNetwork) bool {
		return latest.Status.Rollout != nil &&
			condTrue(latest, seiv1alpha1.ConditionRolloutInProgress)
	}, "v2 rollout registered")

	// Wait for UpdateNodeSpecs to propagate v2 to children's spec.
	waitFor(t, func() bool {
		kids := listChildren(t, getNetwork(t, key))
		if len(kids) != replicas {
			return false
		}
		for i := range kids {
			if kids[i].Spec.Image != v2 {
				return false
			}
		}
		return true
	}, "all children Spec.Image advanced to v2")

	// Snapshot the initial in-flight state. StartedAt is captured here
	// and asserted stable inside Consistently — a regression that
	// re-stamps StartedAt on every reconcile would hide stalls from
	// any age-based alerting (e.g., "rollout stuck >1h").
	initial := snapshotStuck(t, key)
	g.Expect(initial.HasRollout).To(BeTrue(), "initial: Status.Rollout populated")
	g.Expect(initial.RolloutCond).To(BeTrue(), "initial: RolloutInProgress=True")
	g.Expect(initial.HasPlan).To(BeTrue(), "initial: Status.Plan populated")
	g.Expect(initial.PlanInProg).To(BeTrue(), "initial: PlanInProgress=True")
	g.Expect(initial.Phase).To(Equal(seiv1alpha1.GroupPhaseUpgrading), "initial: Phase=Upgrading")
	g.Expect(initial.RolloutComplete).To(BeFalse(), "initial: no RolloutComplete event")
	g.Expect(initial.StartedAt.IsZero()).To(BeFalse(), "initial: StartedAt set")

	// Sustained stuck assertion: every field above stays put for 10s,
	// at 500ms polling cadence. SatisfyAll + HaveField gives Gomega the
	// per-field diagnostic on flake — no opaque "expected true, got false."
	g.Consistently(func() stuckSnapshot {
		return snapshotStuck(t, key)
	}, "10s", "500ms").Should(SatisfyAll(
		HaveField("HasRollout", BeTrue()),
		HaveField("RolloutCond", BeTrue()),
		HaveField("HasPlan", BeTrue()),
		HaveField("PlanPhase", Equal(seiv1alpha1.TaskPlanActive)),
		HaveField("PlanInProg", BeTrue()),
		HaveField("Phase", Equal(seiv1alpha1.GroupPhaseUpgrading)),
		HaveField("ReadyReplicas", Equal(int32(replicas))),
		HaveField("NodesReady", BeTrue()),
		HaveField("StartedAt", Equal(initial.StartedAt)),
		HaveField("RolloutComplete", BeFalse()),
	), "stuck rollout shape holds for 10s with faker paused")

	// Children are observably mid-rollout: Spec advanced to v2 but
	// Status.CurrentImage stays at v1. Operators see this gap and
	// conclude the rollout has stalled mid-flight.
	kids := listChildren(t, getNetwork(t, key))
	g.Expect(kids).To(HaveLen(replicas))
	for i := range kids {
		g.Expect(kids[i].Spec.Image).To(Equal(v2),
			"child %s spec.image advanced to v2", kids[i].Name)
		// Status.CurrentImage is stamped by the SeiNode-side ObserveImage
		// task; with the faker paused, that task never observes the new
		// StatefulSet revision and never stamps the field. It stays at v1
		// — the value stamped earlier by the steady-state drift NodeUpdate
		// (init plan ends at mark-ready and never runs observe-image, so
		// the v1 stamp comes from drift, gated on by the pre-pause barrier
		// above). Not empty, not v2.
		g.Expect(kids[i].Status.CurrentImage).To(Equal(v1),
			"child %s status.currentImage stays at v1 (stuck signal)", kids[i].Name)
	}

	// Recovery: resuming the faker lets the rollout reach terminal
	// state on its own — proves the stuck state is the stall, not a
	// deeper controller failure that would require external intervention.
	testFaker.Resume()

	// The recovery budget is deliberately wider than the default
	// pollTimeout (30s). After a 10s stall the faker has to catch up on
	// every paused tick, the SeiNode controller's NodeUpdate plan has
	// to pick up the now-advancing StatefulSet status, the AwaitSpecUpdate
	// task has to observe child convergence, and the SND has to
	// completePlan — five reconcile-loop hops with apiserver round-trips
	// at each. 60s is enough for p99 on a loaded CI runner.
	g.Eventually(func() bool {
		latest := getNetwork(t, key)
		if latest.Status.Plan != nil {
			return false
		}
		if latest.Status.Rollout != nil {
			return false
		}
		cond := apimeta.FindStatusCondition(latest.Status.Conditions, seiv1alpha1.ConditionRolloutInProgress)
		if cond == nil || cond.Status != metav1.ConditionFalse {
			return false
		}
		return cond.Reason == "RolloutComplete"
	}).WithTimeout(60*time.Second).WithPolling(500*time.Millisecond).Should(BeTrue(),
		"rollout reaches terminal state after faker resume")
}

// stuckSnapshot captures every operator-observable signal exercised by
// the stuck assertion. Centralizing the read in one place lets
// SatisfyAll + HaveField deliver per-field diagnostic messages when a
// Consistently iteration fails — far better than a bool-returning
// predicate that collapses ten failure modes into "expected true, got
// false."
type stuckSnapshot struct {
	HasRollout      bool
	RolloutCond     bool
	HasPlan         bool
	PlanPhase       seiv1alpha1.TaskPlanPhase
	PlanInProg      bool
	Phase           seiv1alpha1.SeiNetworkPhase
	ReadyReplicas   int32
	NodesReady      bool
	StartedAt       metav1.Time
	RolloutComplete bool
}

func snapshotStuck(t *testing.T, key client.ObjectKey) stuckSnapshot {
	t.Helper()
	latest := getNetwork(t, key)
	snap := stuckSnapshot{
		HasRollout:      latest.Status.Rollout != nil,
		RolloutCond:     condTrue(latest, seiv1alpha1.ConditionRolloutInProgress),
		HasPlan:         latest.Status.Plan != nil,
		PlanInProg:      condTrue(latest, seiv1alpha1.ConditionPlanInProgress),
		Phase:           latest.Status.Phase,
		ReadyReplicas:   latest.Status.ReadyReplicas,
		NodesReady:      condTrue(latest, seiv1alpha1.ConditionNodesReady),
		RolloutComplete: len(listEventsForNetwork(t, latest, "RolloutComplete")) > 0,
	}
	if latest.Status.Plan != nil {
		snap.PlanPhase = latest.Status.Plan.Phase
	}
	if latest.Status.Rollout != nil {
		snap.StartedAt = latest.Status.Rollout.StartedAt
	}
	return snap
}
