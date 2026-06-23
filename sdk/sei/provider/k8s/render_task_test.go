package k8s

import (
	"testing"
	"time"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

const (
	testTaskNode = "chaos-net-0" // target SeiNode name used across the task render tests
	testNewImage = "img:v2"      // upgrade image used across the task render tests
)

func TestRenderTask_GovSoftwareUpgrade(t *testing.T) {
	spec := sei.TaskSpec{
		Name:      "upgrade",
		Namespace: testNS,
		Node:      testTaskNode,
		Kind:      sei.TaskGovSoftwareUpgrade,
		Timeout:   90 * time.Second,
		GovSoftwareUpgrade: &sei.GovSoftwareUpgrade{
			ChainID:        testNet,
			Title:          "v2 upgrade",
			Description:    "bump",
			UpgradeName:    "v2",
			UpgradeHeight:  200,
			InitialDeposit: "10000000usei",
			Fees:           "2000usei",
			Gas:            200000,
		},
	}
	task := renderTask(spec, testNS)

	if task.Name != "upgrade" || task.Namespace != testNS {
		t.Fatalf("metadata = %s/%s", task.Namespace, task.Name)
	}
	if string(task.Spec.Kind) != sei.TaskGovSoftwareUpgrade {
		t.Errorf("kind = %q, want %q", task.Spec.Kind, sei.TaskGovSoftwareUpgrade)
	}
	if task.Spec.Target.NodeRef.Name != testTaskNode {
		t.Errorf("target.nodeRef.name = %q", task.Spec.Target.NodeRef.Name)
	}
	// Timeout maps to whole seconds.
	if task.Spec.TimeoutSeconds != 90 {
		t.Errorf("timeoutSeconds = %d, want 90", task.Spec.TimeoutSeconds)
	}
	// Zero RequirePhase / RequirePhaseTimeout leave the CRD defaults in place.
	if task.Spec.Target.RequirePhase != "" {
		t.Errorf("requirePhase = %q, want empty (CRD default)", task.Spec.Target.RequirePhase)
	}
	if task.Spec.Target.RequirePhaseTimeout != nil {
		t.Errorf("requirePhaseTimeout = %v, want nil (CRD default)", task.Spec.Target.RequirePhaseTimeout)
	}
	p := task.Spec.GovSoftwareUpgrade
	if p == nil {
		t.Fatal("govSoftwareUpgrade payload nil")
	}
	if p.UpgradeName != "v2" || p.UpgradeHeight != 200 || p.Gas != 200000 || p.InitialDeposit != "10000000usei" {
		t.Errorf("payload mistranslated = %+v", p)
	}
	// Exactly the matching payload is set.
	if task.Spec.GovVote != nil || task.Spec.AwaitNodesAtHeight != nil || task.Spec.UpdateNodeImage != nil {
		t.Error("non-matching payload set")
	}
}

func TestRenderTask_RequirePhaseOverride(t *testing.T) {
	// A non-default RequirePhase / RequirePhaseTimeout threads through verbatim.
	// (The gate is exact-equality, so a caller sets this only to match a genuinely
	// non-Running target — never to "relax" a halted upgrade node, which stays
	// Running; that case keeps the default. This test covers the mechanics only.)
	spec := sei.TaskSpec{
		Name:                "swap",
		Node:                testTaskNode,
		Kind:                sei.TaskUpdateNodeImage,
		RequirePhase:        "Initializing",
		RequirePhaseTimeout: 2 * time.Minute,
		UpdateNodeImage:     &sei.UpdateNodeImage{Image: testNewImage},
	}
	task := renderTask(spec, testNS)

	if got := string(task.Spec.Target.RequirePhase); got != "Initializing" {
		t.Errorf("requirePhase = %q, want Initializing", got)
	}
	if task.Spec.Target.RequirePhaseTimeout == nil || task.Spec.Target.RequirePhaseTimeout.Duration != 2*time.Minute {
		t.Errorf("requirePhaseTimeout = %v, want 2m", task.Spec.Target.RequirePhaseTimeout)
	}
	if task.Spec.UpdateNodeImage == nil || task.Spec.UpdateNodeImage.Image != testNewImage {
		t.Errorf("updateNodeImage payload = %+v", task.Spec.UpdateNodeImage)
	}
}

func TestTranslateTaskOutputs(t *testing.T) {
	// Only UpdateNodeImage is surfaced — the one kind the controller populates.
	if got := translateTaskOutputs(nil); got != nil {
		t.Errorf("nil outputs => %+v, want nil", got)
	}
	// An outputs object with no UpdateNodeImage (e.g. a gov task's empty outputs)
	// translates to nil — the SDK never surfaces an always-empty field.
	if got := translateTaskOutputs(&seiv1alpha1.SeiNodeTaskOutputs{}); got != nil {
		t.Errorf("empty outputs => %+v, want nil", got)
	}
	out := &seiv1alpha1.SeiNodeTaskOutputs{
		UpdateNodeImage: &seiv1alpha1.UpdateNodeImageOutputs{AppliedImage: testNewImage},
	}
	got := translateTaskOutputs(out)
	if got == nil || got.UpdateNodeImage == nil || got.UpdateNodeImage.AppliedImage != testNewImage {
		t.Errorf("translate => %+v, want AppliedImage=img:v2", got)
	}
}
