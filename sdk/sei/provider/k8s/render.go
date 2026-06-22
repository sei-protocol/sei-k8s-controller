package k8s

import (
	"encoding/json"
	"maps"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

// fieldOwner is the SDK's SSA field manager. A distinct writer from
// seictl/seitask.
const fieldOwner client.FieldOwner = sei.FieldOwner

// renderNetwork builds the SeiNetwork from a NetworkSpec. ChainID is not a spec
// field: it defaults to Name (chain ID == network name) and maps to
// spec.genesis.chainId. Genesis maps to spec.genesis.overrides; Config maps to
// spec.configOverrides. Nodes peer by the network's name, not a label on it, so
// the object carries no canonical labels — but spec.Labels (e.g. a caller GC
// selector) are stamped when provided.
func renderNetwork(spec sei.NetworkSpec, namespace string) *seiv1alpha1.SeiNetwork {
	net := &seiv1alpha1.SeiNetwork{
		TypeMeta: metav1.TypeMeta{
			APIVersion: seiv1alpha1.GroupVersion.String(),
			Kind:       "SeiNetwork",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.Name,
			Namespace: namespace,
			Labels:    maps.Clone(spec.Labels), // nil-safe; caller GC/run-id selector
		},
		Spec: seiv1alpha1.SeiNetworkSpec{
			Image:    spec.Image,
			Replicas: int32(spec.Validators),
			Genesis: seiv1alpha1.GenesisCeremonyConfig{
				ChainID: spec.Name, // chain ID defaults to the network name
			},
		},
	}
	if len(spec.Genesis) > 0 {
		net.Spec.Genesis.Overrides = make(map[string]apiextensionsv1.JSON, len(spec.Genesis))
		for k, v := range spec.Genesis {
			// genesis.overrides values are raw JSON; a bare string is encoded to a
			// JSON string literal so the controller's apiextensions JSON unmarshals
			// it (json.Marshal of a string never errors).
			raw, _ := json.Marshal(v)
			net.Spec.Genesis.Overrides[k] = apiextensionsv1.JSON{Raw: raw}
		}
	}
	if len(spec.Config) > 0 {
		net.Spec.ConfigOverrides = maps.Clone(spec.Config)
	}
	for _, a := range spec.Accounts {
		net.Spec.Genesis.Accounts = append(net.Spec.Genesis.Accounts,
			seiv1alpha1.GenesisAccount{Address: a.Address, Balance: a.Balance})
	}
	return net
}

// renderNode builds one RPC SeiNode and stamps the canonical object labels and
// synthesized peer source. spec.Network drives both the object label and the peer
// selector — the canonical sei.io/seinetwork wiring. The node lives at namespace;
// the peer selector searches networkNS, where the genesis validators live (equal
// to namespace when co-located). ChainID defaults to spec.Network.
func renderNode(spec sei.NodeSpec, namespace, networkNS string) *seiv1alpha1.SeiNode {
	// Caller labels first, then the canonical labels on top — the canonical
	// sei.io/role + sei.io/seinetwork are load-bearing (peer wiring, chaos
	// selectors) and must win on any key collision.
	labels := maps.Clone(spec.Labels)
	if labels == nil {
		labels = make(map[string]string, 2)
	}
	labels[sei.LabelRole] = sei.RoleNode
	labels[sei.LabelSeiNetwork] = spec.Network

	node := &seiv1alpha1.SeiNode{
		TypeMeta: metav1.TypeMeta{
			APIVersion: seiv1alpha1.GroupVersion.String(),
			Kind:       "SeiNode",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.Name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  spec.Network,
			Image:    spec.Image,
			FullNode: &seiv1alpha1.FullNodeSpec{}, // rpc == fullNode mode
			Peers: []seiv1alpha1.PeerSource{{
				Label: &seiv1alpha1.LabelPeerSource{
					Selector:  map[string]string{sei.LabelSeiNetwork: spec.Network},
					Namespace: networkNS,
				},
			}},
		},
	}
	if len(spec.Config) > 0 {
		node.Spec.Overrides = maps.Clone(spec.Config)
	}
	return node
}
