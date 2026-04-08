package planner

import (
	"testing"

	seiconfig "github.com/sei-protocol/sei-config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func TestCommonOverrides_WithExternalAddress(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		Status: seiv1alpha1.SeiNodeStatus{
			ExternalAddress: "p2p.atlantic-2.seinetwork.io:26656",
		},
	}
	overrides := commonOverrides(node)
	if overrides == nil {
		t.Fatal("expected overrides, got nil")
	}
	if got := overrides[seiconfig.KeyP2PExternalAddress]; got != "p2p.atlantic-2.seinetwork.io:26656" {
		t.Errorf("p2p.external_address = %q, want %q", got, "p2p.atlantic-2.seinetwork.io:26656")
	}
}

func TestCommonOverrides_EmptyExternalAddress(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
	}
	overrides := commonOverrides(node)
	if overrides != nil {
		t.Errorf("expected nil overrides, got %v", overrides)
	}
}

func TestCommonOverrides_UserOverrideTakesPrecedence(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  "test-1",
			Image:    "seid:v1",
			FullNode: &seiv1alpha1.FullNodeSpec{},
			Overrides: map[string]string{
				seiconfig.KeyP2PExternalAddress: "custom.address:26656",
			},
		},
		Status: seiv1alpha1.SeiNodeStatus{
			ExternalAddress: "lb.address:26656",
		},
	}

	common := commonOverrides(node)
	merged := mergeOverrides(common, node.Spec.Overrides)

	if got := merged[seiconfig.KeyP2PExternalAddress]; got != "custom.address:26656" {
		t.Errorf("user override should win: got %q, want %q", got, "custom.address:26656")
	}
}
