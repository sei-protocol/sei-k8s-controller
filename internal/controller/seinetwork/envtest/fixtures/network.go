// Package fixtures provides reusable SeiNetwork constructors for envtest
// integration tests. The default builder produces a minimal single-replica
// genesis validator pool — the only shape the Kind admits (genesis is
// required; the controller synthesizes each child's validator role from the
// scoped genesis spec). Options layer narrowly.
package fixtures

import (
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

// WithDataVolumeImport sets spec.dataVolume to import a pre-existing PVC.
func WithDataVolumeImport(pvcName string) Option {
	return func(network *seiv1alpha1.SeiNetwork) {
		network.Spec.DataVolume = &seiv1alpha1.DataVolumeSpec{
			Import: &seiv1alpha1.DataVolumeImport{PVCName: pvcName},
		}
	}
}
