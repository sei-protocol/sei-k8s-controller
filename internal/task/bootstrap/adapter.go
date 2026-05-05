package bootstrap

import (
	seiconfig "github.com/sei-protocol/sei-config"
	corev1 "k8s.io/api/core/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// nodeToBootstrapInputs translates a SeiNode (and its resolved SnapshotSource,
// when present) into the value-typed PodInputs.
//
// snap may be nil; the Service path doesn't have a snapshot in scope and only
// reads Name/Namespace/SidecarPort. GenerateJob enforces the rest.
//
// Resolution rules:
//   - Image: snap.BootstrapImage if non-empty, else node.Spec.Image.
//   - SidecarImage / SidecarPort: SeiNode override if set, else platform default.
//   - Mode: derived from which Spec.{Archive,Validator} block is set; full otherwise.
//   - HaltHeight: snap.S3.TargetHeight when snap.S3 is set; 0 otherwise.
//   - ForbiddenSecretNames: validator's signing-key and node-key Secret names
//     when the SeiNode is a validator. GenerateJob fails closed if
//     either ever lands as a Volume on the bootstrap pod.
func nodeToBootstrapInputs(node *seiv1alpha1.SeiNode, snap *seiv1alpha1.SnapshotSource) PodInputs {
	image := node.Spec.Image
	if snap != nil && snap.BootstrapImage != "" {
		image = snap.BootstrapImage
	}

	sidecarImage := bootstrapDefaultSidecarImage
	if node.Spec.Sidecar != nil && node.Spec.Sidecar.Image != "" {
		sidecarImage = node.Spec.Sidecar.Image
	}

	sidecarPort := seiconfig.PortSidecar
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

	return PodInputs{
		Name:                 node.Name,
		Namespace:            node.Namespace,
		ChainID:              node.Spec.ChainID,
		Image:                image,
		SidecarImage:         sidecarImage,
		SidecarPort:          sidecarPort,
		SidecarResources:     sidecarResources,
		Mode:                 mode,
		HaltHeight:           haltHeight,
		ForbiddenSecretNames: validatorSecretNames(node),
	}
}

// validatorSecretNames returns the Secret names referenced by a validator's
// signing-key and node-key sources, so GenerateJob can refuse to
// produce a bootstrap pod that mounts either.
func validatorSecretNames(node *seiv1alpha1.SeiNode) []string {
	if node.Spec.Validator == nil {
		return nil
	}
	var names []string
	if k := node.Spec.Validator.SigningKey; k != nil && k.Secret != nil && k.Secret.SecretName != "" {
		names = append(names, k.Secret.SecretName)
	}
	if k := node.Spec.Validator.NodeKey; k != nil && k.Secret != nil && k.Secret.SecretName != "" {
		names = append(names, k.Secret.SecretName)
	}
	return names
}
