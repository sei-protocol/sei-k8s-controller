package k8s

import (
	"testing"
	"time"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

func TestRenderTask_GovSoftwareUpgrade(t *testing.T) {
	spec := sei.TaskSpec{
		Name:      "upgrade",
		Namespace: testNS,
		Node:      "chaos-net-0",
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
	if task.Spec.Target.NodeRef.Name != "chaos-net-0" {
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

func TestRenderTask_UpdateNodeImageRelaxesRequirePhase(t *testing.T) {
	// A major upgrade swaps the binary on a chain halted at the upgrade height —
	// the target is not Running, so the caller relaxes RequirePhase.
	spec := sei.TaskSpec{
		Name:                "swap",
		Node:                "chaos-net-0",
		Kind:                sei.TaskUpdateNodeImage,
		RequirePhase:        "Pending",
		RequirePhaseTimeout: 2 * time.Minute,
		UpdateNodeImage:     &sei.UpdateNodeImage{Image: "img:v2"},
	}
	task := renderTask(spec, testNS)

	if got := string(task.Spec.Target.RequirePhase); got != "Pending" {
		t.Errorf("requirePhase = %q, want Pending", got)
	}
	if task.Spec.Target.RequirePhaseTimeout == nil || task.Spec.Target.RequirePhaseTimeout.Duration != 2*time.Minute {
		t.Errorf("requirePhaseTimeout = %v, want 2m", task.Spec.Target.RequirePhaseTimeout)
	}
	if task.Spec.UpdateNodeImage == nil || task.Spec.UpdateNodeImage.Image != "img:v2" {
		t.Errorf("updateNodeImage payload = %+v", task.Spec.UpdateNodeImage)
	}
}
