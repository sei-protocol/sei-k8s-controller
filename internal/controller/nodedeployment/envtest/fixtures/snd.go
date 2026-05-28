// Package fixtures provides reusable SeiNodeDeployment constructors for
// envtest integration tests. The default builder produces a minimal
// InPlace, full-node SND with no networking and no genesis ceremony —
// the shape Phase 1 exercises end-to-end. Options layer narrowly.
package fixtures

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// DefaultImage is the seid image stamped onto the template by NewSND.
// Tests that exercise rollouts patch to a different tag and assert
// propagation; the value below is the pre-rollout baseline.
const DefaultImage = "ghcr.io/sei-protocol/seid:v1.0.0"

// Option mutates a SeiNodeDeployment built by NewSND.
type Option func(*seiv1alpha1.SeiNodeDeployment)

// NewSND returns a SeiNodeDeployment with InPlace updateStrategy, a single
// full-node replica, and the default seid image. Options layer on top.
// The returned object has no creationTimestamp / resourceVersion — call
// client.Create to persist it.
func NewSND(namespace, name string, opts ...Option) *seiv1alpha1.SeiNodeDeployment {
	snd := &seiv1alpha1.SeiNodeDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: seiv1alpha1.SeiNodeDeploymentSpec{
			Replicas: 1,
			UpdateStrategy: seiv1alpha1.UpdateStrategy{
				Type: seiv1alpha1.UpdateStrategyInPlace,
			},
			Template: seiv1alpha1.SeiNodeTemplate{
				Spec: seiv1alpha1.SeiNodeSpec{
					ChainID:  "pacific-1",
					Image:    DefaultImage,
					FullNode: &seiv1alpha1.FullNodeSpec{},
				},
			},
		},
	}
	for _, opt := range opts {
		opt(snd)
	}
	return snd
}

// WithReplicas sets spec.replicas.
func WithReplicas(n int32) Option {
	return func(snd *seiv1alpha1.SeiNodeDeployment) {
		snd.Spec.Replicas = n
	}
}

// WithImage sets spec.template.spec.image.
func WithImage(image string) Option {
	return func(snd *seiv1alpha1.SeiNodeDeployment) {
		snd.Spec.Template.Spec.Image = image
	}
}

// WithNetworking enables the presence-signal Networking config so the
// controller publishes external Service + HTTPRoutes. Off by default
// because the controller's hostname-resolvability gate blocks status
// progression unless DNS resolves the route's hostname.
func WithNetworking() Option {
	return func(snd *seiv1alpha1.SeiNodeDeployment) {
		snd.Spec.Networking = &seiv1alpha1.NetworkingConfig{}
	}
}

// WithValidator swaps the default full-node spec for a validator-mode
// spec. Validator mode has no externally-routable protocols, so the
// controller publishes zero HTTPRoutes even when WithNetworking is set —
// the negative case the networking test exercises.
func WithValidator() Option {
	return func(snd *seiv1alpha1.SeiNodeDeployment) {
		snd.Spec.Template.Spec.FullNode = nil
		snd.Spec.Template.Spec.Validator = &seiv1alpha1.ValidatorSpec{}
	}
}

// WithPublishableP2P enables the per-pod L4 NLB (Networking.TCP) on the
// SND. Used by tests that exercise the SeiNode controller's
// reconcileExternalAddress path. Composable with WithNetworking when an
// SND wants both HTTP routes and per-pod NLB exposure.
func WithPublishableP2P() Option {
	return func(snd *seiv1alpha1.SeiNodeDeployment) {
		if snd.Spec.Networking == nil {
			snd.Spec.Networking = &seiv1alpha1.NetworkingConfig{}
		}
		snd.Spec.Networking.TCP = &seiv1alpha1.TCPConfig{}
	}
}
