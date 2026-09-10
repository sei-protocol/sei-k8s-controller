// Package fixtures provides reusable SeiNetwork constructors for envtest
// integration tests. The default builder produces a minimal single-replica
// genesis validator pool — the only shape the Kind admits (genesis is
// required; the controller synthesizes each child's validator role from the
// scoped genesis spec). Options layer narrowly.
package fixtures

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// DefaultImage is the seid image stamped on the network by NewNetwork.
const DefaultImage = "ghcr.io/sei-protocol/seid:v1.0.0"

// DefaultChainID is the genesis chain ID used by NewNetwork.
const DefaultChainID = "pacific-1"

// Option mutates a SeiNetwork built by NewNetwork.
type Option func(*seiv1alpha1.SeiNetwork)

// NewNetwork returns a SeiNetwork with a single genesis validator replica, a
// required genesis ceremony, and the default seid image. Options layer on top.
// The returned object has no creationTimestamp / resourceVersion — call
// client.Create to persist it.
func NewNetwork(namespace, name string, opts ...Option) *seiv1alpha1.SeiNetwork {
	network := &seiv1alpha1.SeiNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: seiv1alpha1.SeiNetworkSpec{
			Image:    DefaultImage,
			Replicas: 1,
			Genesis:  seiv1alpha1.GenesisCeremonyConfig{ChainID: DefaultChainID},
		},
	}
	for _, opt := range opts {
		opt(network)
	}
	return network
}

// WithReplicas sets spec.replicas.
func WithReplicas(n int32) Option {
	return func(network *seiv1alpha1.SeiNetwork) {
		network.Spec.Replicas = n
	}
}

// WithImage sets spec.image.
func WithImage(image string) Option {
	return func(network *seiv1alpha1.SeiNetwork) {
		network.Spec.Image = image
	}
}

// WithGenesisAccounts adds non-validator genesis accounts.
func WithGenesisAccounts(accounts ...seiv1alpha1.GenesisAccount) Option {
	return func(network *seiv1alpha1.SeiNetwork) {
		network.Spec.Genesis.Accounts = accounts
	}
}

// WithConfigOverrides sets spec.configOverrides (seid runtime TOML).
func WithConfigOverrides(overrides map[string]string) Option {
	return func(network *seiv1alpha1.SeiNetwork) {
		network.Spec.ConfigOverrides = overrides
	}
}

// WithSidecar sets spec.sidecar.
func WithSidecar(sidecar *seiv1alpha1.SidecarConfig) Option {
	return func(network *seiv1alpha1.SeiNetwork) {
		network.Spec.Sidecar = sidecar
	}
}

// WithResources sets spec.resources — the seid footprint every genesis
// validator in the pool receives. cpu and memory are quantity strings.
func WithResources(cpu, memory string) Option {
	return func(network *seiv1alpha1.SeiNetwork) {
		network.Spec.Resources = &seiv1alpha1.Resources{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(memory),
			},
		}
	}
}

// WithDataVolumeStorage sets the data volume size every pool validator receives.
func WithDataVolumeStorage(size string) Option {
	return func(network *seiv1alpha1.SeiNetwork) {
		network.Spec.DataVolume = &seiv1alpha1.DataVolumeSpec{
			Storage: &seiv1alpha1.DataVolumeStorage{
				Resources: &seiv1alpha1.VolumeClaimResources{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse(size),
					},
				},
			},
		}
	}
}

// WithDataVolumeVAC sets the VolumeAttributesClass name every pool validator's
// volume selects. Layers onto an existing spec.dataVolume.storage (from
// WithDataVolumeStorage) rather than replacing it, so the two compose.
func WithDataVolumeVAC(name string) Option {
	return func(network *seiv1alpha1.SeiNetwork) {
		if network.Spec.DataVolume == nil {
			network.Spec.DataVolume = &seiv1alpha1.DataVolumeSpec{}
		}
		if network.Spec.DataVolume.Storage == nil {
			network.Spec.DataVolume.Storage = &seiv1alpha1.DataVolumeStorage{}
		}
		network.Spec.DataVolume.Storage.VolumeAttributesClassName = &name
	}
}

// WithDataVolumeImport sets spec.dataVolume to import a pre-existing PVC.
func WithDataVolumeImport(pvcName string) Option {
	return func(network *seiv1alpha1.SeiNetwork) {
		network.Spec.DataVolume = &seiv1alpha1.DataVolumeSpec{
			Import: &seiv1alpha1.DataVolumeImport{PVCName: pvcName},
		}
	}
}
