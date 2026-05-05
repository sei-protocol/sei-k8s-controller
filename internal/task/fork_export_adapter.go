package task

import (
	"fmt"

	seiconfig "github.com/sei-protocol/sei-config"
	corev1 "k8s.io/api/core/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
)

// exporterRoot returns the resource-name root used for the bootstrap Job,
// export Job, headless Service, and PVC produced by the SND fork-genesis
// sub-plan. ExportJobName(<root>) produces the per-Job name.
func exporterRoot(groupName string) string { return fmt.Sprintf("%s-exporter", groupName) }

// sndToBootstrapInputs builds BootstrapPodInputs for a fork-genesis bootstrap
// Job from the SND's spec + the operator-supplied platform config. The
// SeiNode-driven path uses nodeToBootstrapInputs; this is its SND-shaped twin.
//
// ForbiddenSecretNames is intentionally nil — there is no validator in scope
// on the SND fork path, so no signing-key Secrets exist that the bootstrap
// builder needs to refuse.
func sndToBootstrapInputs(group *seiv1alpha1.SeiNodeDeployment, platformCfg platform.Config) BootstrapPodInputs {
	fork := group.Spec.Genesis.Fork
	return BootstrapPodInputs{
		Name:             exporterRoot(group.Name),
		Namespace:        group.Namespace,
		ChainID:          fork.SourceChainID,
		Image:            fork.SourceImage,
		SidecarImage:     resolvedSidecarImage(group, platformCfg),
		SidecarPort:      resolvedSidecarPort(group),
		SidecarResources: resolvedSidecarResources(group),
		Mode:             string(seiconfig.ModeFull),
		HaltHeight:       fork.ExportHeight,
	}
}

// sndToExportInputs builds ExportJobInputs for the export Job. ChainID, image,
// and height come from Spec.Genesis.Fork; bucket/region come from platformCfg;
// the genesis key is derived deterministically.
func sndToExportInputs(group *seiv1alpha1.SeiNodeDeployment, platformCfg platform.Config) ExportJobInputs {
	fork := group.Spec.Genesis.Fork
	return ExportJobInputs{
		Name:             exporterRoot(group.Name),
		Namespace:        group.Namespace,
		ChainID:          fork.SourceChainID,
		SeidImage:        fork.SourceImage,
		SidecarImage:     resolvedSidecarImage(group, platformCfg),
		SidecarPort:      resolvedSidecarPort(group),
		SidecarResources: resolvedSidecarResources(group),
		Mode:             string(seiconfig.ModeFull),
		PVCClaimName:     ExporterPVCName(group.Name),
		ExportHeight:     fork.ExportHeight,
		GenesisBucket:    platformCfg.GenesisBucket,
		GenesisRegion:    platformCfg.GenesisRegion,
		GenesisKey:       fmt.Sprintf("%s/exported-state.json", fork.SourceChainID),
	}
}

func resolvedSidecarImage(group *seiv1alpha1.SeiNodeDeployment, _ platform.Config) string {
	if s := group.Spec.Template.Spec.Sidecar; s != nil && s.Image != "" {
		return s.Image
	}
	return platform.DefaultSidecarImage
}

func resolvedSidecarPort(group *seiv1alpha1.SeiNodeDeployment) int32 {
	if s := group.Spec.Template.Spec.Sidecar; s != nil && s.Port != 0 {
		return s.Port
	}
	return seiconfig.PortSidecar
}

func resolvedSidecarResources(group *seiv1alpha1.SeiNodeDeployment) *corev1.ResourceRequirements {
	if s := group.Spec.Template.Spec.Sidecar; s != nil {
		return s.Resources
	}
	return nil
}
