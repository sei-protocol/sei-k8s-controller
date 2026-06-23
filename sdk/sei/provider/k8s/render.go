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

// fieldOwner is the SDK's SSA field manager. A distinct writer from seictl.
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
			// "" leaves the CRD default (Retain); a caller sets Delete so an
			// ephemeral chain's validators cascade-delete instead of orphaning.
			DeletionPolicy: seiv1alpha1.DeletionPolicy(spec.DeletionPolicy),
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

// renderTask builds a SeiNodeTask from a TaskSpec. Kind and the matching payload
// are validated in core before this runs; here we translate the SDK-native
// payload to the CRD sub-spec. An empty RequirePhase / zero RequirePhaseTimeout /
// zero Timeout leaves the CRD defaults (Running / 5m / unbounded) in place.
func renderTask(spec sei.TaskSpec, namespace string) *seiv1alpha1.SeiNodeTask {
	target := seiv1alpha1.SeiNodeTaskTarget{
		NodeRef: seiv1alpha1.SeiNodeTaskNodeRef{Name: spec.Node},
	}
	if spec.RequirePhase != "" {
		target.RequirePhase = seiv1alpha1.SeiNodePhase(spec.RequirePhase)
	}
	if spec.RequirePhaseTimeout > 0 {
		target.RequirePhaseTimeout = &metav1.Duration{Duration: spec.RequirePhaseTimeout}
	}

	task := &seiv1alpha1.SeiNodeTask{
		TypeMeta: metav1.TypeMeta{
			APIVersion: seiv1alpha1.GroupVersion.String(),
			Kind:       "SeiNodeTask",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.Name,
			Namespace: namespace,
			Labels:    maps.Clone(spec.Labels), // nil-safe; caller GC/run-id selector
		},
		Spec: seiv1alpha1.SeiNodeTaskSpec{
			Kind:           seiv1alpha1.SeiNodeTaskKind(spec.Kind),
			Target:         target,
			TimeoutSeconds: int32(spec.Timeout.Seconds()),
		},
	}

	switch {
	case spec.GovSoftwareUpgrade != nil:
		p := spec.GovSoftwareUpgrade
		task.Spec.GovSoftwareUpgrade = &seiv1alpha1.GovSoftwareUpgradePayload{
			ChainID:        p.ChainID,
			KeyName:        p.KeyName,
			Title:          p.Title,
			Description:    p.Description,
			UpgradeName:    p.UpgradeName,
			UpgradeHeight:  p.UpgradeHeight,
			UpgradeInfo:    p.UpgradeInfo,
			InitialDeposit: p.InitialDeposit,
			Memo:           p.Memo,
			Fees:           p.Fees,
			Gas:            p.Gas,
		}
	case spec.GovVote != nil:
		p := spec.GovVote
		task.Spec.GovVote = &seiv1alpha1.GovVotePayload{
			ChainID:    p.ChainID,
			KeyName:    p.KeyName,
			ProposalID: p.ProposalID,
			Option:     p.Option,
			Memo:       p.Memo,
			Fees:       p.Fees,
			Gas:        p.Gas,
		}
	case spec.AwaitNodesAtHeight != nil:
		task.Spec.AwaitNodesAtHeight = &seiv1alpha1.AwaitNodesAtHeightPayload{
			TargetHeight: spec.AwaitNodesAtHeight.TargetHeight,
		}
	case spec.UpdateNodeImage != nil:
		task.Spec.UpdateNodeImage = &seiv1alpha1.UpdateNodeImagePayload{
			Image: spec.UpdateNodeImage.Image,
		}
	}
	return task
}
