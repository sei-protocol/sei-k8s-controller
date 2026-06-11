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
		Spec: seiv1alpha1.SeiNodeSpec{
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
	if got := overrides["logging.level"]; got != "error" {
		t.Errorf("logging.level = %q, want %q", got, "error")
	}
}

func TestCommonOverrides_EmptyExternalAddress(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
	}
	overrides := commonOverrides(node)
	if got := overrides["logging.level"]; got != "error" {
		t.Errorf("logging.level = %q, want %q", got, "error")
	}
	if _, ok := overrides[seiconfig.KeyP2PExternalAddress]; ok {
		t.Errorf("expected no p2p.external_address, got %q", overrides[seiconfig.KeyP2PExternalAddress])
	}
}

// persistent_peers is stamped from status.resolvedPeers: joined when present,
// empty (but present) when none — symmetric, like external_address. The empty
// case is what lets us delete the #394 cold-start hack: an off-sidecar empty
// peer set is just persistent_peers="", not a DiscoverPeers deadlock.
func TestCommonOverrides_PersistentPeers(t *testing.T) {
	withPeers := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		Status: seiv1alpha1.SeiNodeStatus{
			ResolvedPeers: []string{"a@h1:26656", "b@h2:26656"},
		},
	}
	if got := commonOverrides(withPeers)[keyP2PPersistentPeers]; got != "a@h1:26656,b@h2:26656" {
		t.Errorf("persistent_peers = %q, want joined set", got)
	}

	none := &seiv1alpha1.SeiNode{ObjectMeta: metav1.ObjectMeta{Name: "test-node"}}
	got, ok := commonOverrides(none)[keyP2PPersistentPeers]
	if !ok {
		t.Fatal("persistent_peers must be present (stamped empty), not absent")
	}
	if got != "" {
		t.Errorf("persistent_peers = %q, want empty", got)
	}
}

func TestCommonOverrides_UserOverrideTakesPrecedence(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:         "test-1",
			Image:           "seid:v1",
			FullNode:        &seiv1alpha1.FullNodeSpec{},
			ExternalAddress: "lb.address:26656",
			Overrides: map[string]string{
				seiconfig.KeyP2PExternalAddress: "custom.address:26656",
			},
		},
	}

	common := commonOverrides(node)
	merged := mergeOverrides(common, node.Spec.Overrides)

	if got := merged[seiconfig.KeyP2PExternalAddress]; got != "custom.address:26656" {
		t.Errorf("user override should win: got %q, want %q", got, "custom.address:26656")
	}
}
