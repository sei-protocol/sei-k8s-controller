package node

import (
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func endpointNode(name, namespace string, spec seiv1alpha1.SeiNodeSpec) *seiv1alpha1.SeiNode { //nolint:unparam // test helper designed for reuse
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       spec,
	}
}

func TestServesEVM_TruthTable(t *testing.T) {
	g := NewWithT(t)
	cases := []struct {
		name string
		spec seiv1alpha1.SeiNodeSpec
		want bool
	}{
		{"fullNode", seiv1alpha1.SeiNodeSpec{FullNode: &seiv1alpha1.FullNodeSpec{}}, true},
		{"archive", seiv1alpha1.SeiNodeSpec{Archive: &seiv1alpha1.ArchiveSpec{}}, true},
		{"validator", seiv1alpha1.SeiNodeSpec{Validator: &seiv1alpha1.ValidatorSpec{}}, false},
		{"replayer", seiv1alpha1.SeiNodeSpec{Replayer: &seiv1alpha1.ReplayerSpec{}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g.Expect(servesEVM(endpointNode("n", "sei", tc.spec))).To(Equal(tc.want))
		})
	}
}

func TestComposeNodeEndpoints_FullNode(t *testing.T) {
	g := NewWithT(t)
	node := endpointNode("chaos-rpc", "sei", seiv1alpha1.SeiNodeSpec{FullNode: &seiv1alpha1.FullNodeSpec{}})

	got := composeNodeEndpoints(node)

	g.Expect(got).To(Equal(&seiv1alpha1.NodeEndpointStatus{
		EvmJsonRpc:     "http://chaos-rpc.sei.svc:8545",
		EvmWs:          "ws://chaos-rpc.sei.svc:8546",
		TendermintRpc:  "http://chaos-rpc.sei.svc:26657",
		TendermintRest: "http://chaos-rpc.sei.svc:1317",
	}))
}

func TestComposeNodeEndpoints_Archive(t *testing.T) {
	g := NewWithT(t)
	node := endpointNode("chaos-archive", "sei", seiv1alpha1.SeiNodeSpec{Archive: &seiv1alpha1.ArchiveSpec{}})

	got := composeNodeEndpoints(node)

	g.Expect(got).To(Equal(&seiv1alpha1.NodeEndpointStatus{
		EvmJsonRpc:     "http://chaos-archive.sei.svc:8545",
		EvmWs:          "ws://chaos-archive.sei.svc:8546",
		TendermintRpc:  "http://chaos-archive.sei.svc:26657",
		TendermintRest: "http://chaos-archive.sei.svc:1317",
	}))
}

func TestComposeNodeEndpoints_ValidatorNil(t *testing.T) {
	g := NewWithT(t)
	node := endpointNode("genesis-val-0", "sei", seiv1alpha1.SeiNodeSpec{Validator: &seiv1alpha1.ValidatorSpec{}})

	g.Expect(composeNodeEndpoints(node)).To(BeNil())
}

// Replayer must return nil. noderesource.NodeMode collapses replayer -> ModeFull,
// so a mode-string gate would mis-classify it as EVM-serving; servesEVM gates on
// the spec sub-spec to avoid that. This is the ModeFull-collapse regression guard.
func TestComposeNodeEndpoints_ReplayerNil(t *testing.T) {
	g := NewWithT(t)
	node := endpointNode("replay-0", "sei", seiv1alpha1.SeiNodeSpec{Replayer: &seiv1alpha1.ReplayerSpec{}})

	g.Expect(composeNodeEndpoints(node)).To(BeNil())
}
