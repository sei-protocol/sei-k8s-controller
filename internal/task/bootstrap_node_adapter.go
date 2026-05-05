package task

import (
	"fmt"

	seiconfig "github.com/sei-protocol/sei-config"
	corev1 "k8s.io/api/core/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// nodeToBootstrapInputs translates a SeiNode (and its resolved SnapshotSource,
// when present) into the value-typed BootstrapPodInputs consumed by the
// SeiNode-free bootstrap helpers in bootstrap_resources.go.
//
// snap may be nil. The Service path (bootstrap_service.go) does not have a
// snapshot in scope; only Name/Namespace/SidecarPort are read by the Service
// builder, so this adapter still produces a usable struct in that case.
// GenerateBootstrapJob enforces the Job-side invariants (BootstrapImage,
// HaltHeight) directly.
//
// Resolution rules:
//   - BootstrapImage: snap.BootstrapImage if non-empty, else node.Spec.Image.
//     SeidImage is set to the same value — both seid containers (init + main)
//     share the bootstrap-resolved image today.
//   - SidecarImage / SidecarPort: SeiNode override if set, else platform default.
//   - Mode: derived from which Spec.{Archive,Validator} block is set.
//   - HaltHeight: snap.S3.TargetHeight when snap.S3 is set; 0 otherwise.
func nodeToBootstrapInputs(node *seiv1alpha1.SeiNode, snap *seiv1alpha1.SnapshotSource) BootstrapPodInputs {
	bootstrapImage := node.Spec.Image
	if snap != nil && snap.BootstrapImage != "" {
		bootstrapImage = snap.BootstrapImage
	}

	sidecarImage := bootstrapDefaultSidecarImage
	if node.Spec.Sidecar != nil && node.Spec.Sidecar.Image != "" {
		sidecarImage = node.Spec.Sidecar.Image
	}

	sidecarPort := int32(seiconfig.PortSidecar)
	if node.Spec.Sidecar != nil && node.Spec.Sidecar.Port != 0 {
		sidecarPort = node.Spec.Sidecar.Port
	}

	var sidecarResources *corev1.ResourceRequirements
	if node.Spec.Sidecar != nil && node.Spec.Sidecar.Resources != nil {
		sidecarResources = node.Spec.Sidecar.Resources
	}

	var mode string
	switch {
	case node.Spec.Archive != nil:
		mode = string(seiconfig.ModeArchive)
	case node.Spec.Validator != nil:
		mode = string(seiconfig.ModeValidator)
	default:
		mode = string(seiconfig.ModeFull)
	}

	var haltHeight int64
	if snap != nil && snap.S3 != nil {
		haltHeight = snap.S3.TargetHeight
	}

	return BootstrapPodInputs{
		Name:             node.Name,
		Namespace:        node.Namespace,
		ChainID:          node.Spec.ChainID,
		SeidImage:        bootstrapImage,
		BootstrapImage:   bootstrapImage,
		SidecarImage:     sidecarImage,
		SidecarPort:      sidecarPort,
		SidecarResources: sidecarResources,
		Mode:             mode,
		HaltHeight:       haltHeight,
	}
}

// assertNoSigningKeyOnBootstrapPod fails closed if a future refactor
// accidentally lands the validator's signing-key Secret on the bootstrap
// pod-spec. The bootstrap path must never carry consensus signing material.
//
// Lives at the per-SeiNode adapter boundary because it reads
// node.Spec.Validator.SigningKey.Secret.SecretName, which doesn't exist
// in BootstrapPodInputs. SND fork-genesis paths physically cannot leak
// signing material — there is no SeiNode and no Validator config in scope.
func assertNoSigningKeyOnBootstrapPod(node *seiv1alpha1.SeiNode, spec *corev1.PodSpec) error {
	if node.Spec.Validator == nil ||
		node.Spec.Validator.SigningKey == nil ||
		node.Spec.Validator.SigningKey.Secret == nil {
		return nil
	}
	secretName := node.Spec.Validator.SigningKey.Secret.SecretName
	for _, v := range spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == secretName {
			return fmt.Errorf("bootstrap pod-spec for %s/%s references signing-key Secret %q on volume %q; "+
				"bootstrap pods must never carry consensus signing material",
				node.Namespace, node.Name, secretName, v.Name)
		}
	}
	return nil
}
