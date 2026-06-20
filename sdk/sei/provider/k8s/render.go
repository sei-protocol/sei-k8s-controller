package k8s

import (
	"encoding/json"
	"maps"
	"strconv"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

// fieldOwner is the SDK's SSA field manager (one-way door D6). A distinct writer
// from seictl/seitask.
const fieldOwner client.FieldOwner = sei.FieldOwner

// nodeName composes the ordinal-scoped follower name, mirroring
// provisionnode.nodeName: <base>-<ordinal>.
func nodeName(prefix string, ordinal int) string {
	return prefix + "-" + strconv.Itoa(ordinal)
}

// renderNetwork builds the SeiNetwork object from a NetworkSpec. ChainID maps to
// spec.genesis.chainId (not a top-level field, per the CRD), and overrides/
// accounts map into the genesis ceremony config. The network object is NOT
// label-stamped — followers match on its name, not a label on it (LLD §5.3).
func renderNetwork(spec sei.NetworkSpec, namespace string) *seiv1alpha1.SeiNetwork {
	net := &seiv1alpha1.SeiNetwork{
		TypeMeta: metav1.TypeMeta{
			APIVersion: seiv1alpha1.GroupVersion.String(),
			Kind:       "SeiNetwork",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.Name,
			Namespace: namespace,
		},
		Spec: seiv1alpha1.SeiNetworkSpec{
			Image:    spec.Image,
			Replicas: int32(spec.Replicas),
			Genesis: seiv1alpha1.GenesisCeremonyConfig{
				ChainID: spec.ChainID,
			},
		},
	}
	if len(spec.Overrides) > 0 {
		net.Spec.Genesis.Overrides = make(map[string]apiextensionsv1.JSON, len(spec.Overrides))
		for k, v := range spec.Overrides {
			// genesis.overrides values are raw JSON; a bare string from the typed
			// spec is encoded to a JSON string literal so the controller's
			// apiextensions JSON unmarshals it (json.Marshal of a string never
			// errors).
			raw, _ := json.Marshal(v)
			net.Spec.Genesis.Overrides[k] = apiextensionsv1.JSON{Raw: raw}
		}
	}
	for _, a := range spec.GenesisAccounts {
		net.Spec.Genesis.Accounts = append(net.Spec.Genesis.Accounts,
			seiv1alpha1.GenesisAccount{Address: a.Address, Balance: a.Balance})
	}
	return net
}

// renderNode builds one follower SeiNode and stamps the canonical object labels
// and synthesized peer source, mirroring provisionnode.stampMetadata. The
// network arg drives both the object label and the peer selector — derived, not
// caller-supplied (LLD §5.3).
func renderNode(spec sei.FleetSpec, namespace, networkName, networkNamespace, chainID string, ordinal int) *seiv1alpha1.SeiNode {
	node := &seiv1alpha1.SeiNode{
		TypeMeta: metav1.TypeMeta{
			APIVersion: seiv1alpha1.GroupVersion.String(),
			Kind:       "SeiNode",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      nodeName(spec.NamePrefix, ordinal),
			Namespace: namespace,
			Labels: map[string]string{
				sei.LabelRole:       sei.RoleNode,
				sei.LabelSeiNetwork: networkName,
			},
		},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  chainID,
			Image:    spec.Image,
			FullNode: &seiv1alpha1.FullNodeSpec{}, // "rpc" preset == fullNode mode
			Peers: []seiv1alpha1.PeerSource{{
				Label: &seiv1alpha1.LabelPeerSource{
					Selector:  map[string]string{sei.LabelSeiNetwork: networkName},
					Namespace: networkNamespace,
				},
			}},
		},
	}
	if len(spec.Overrides) > 0 {
		node.Spec.Overrides = maps.Clone(spec.Overrides)
	}
	return node
}
