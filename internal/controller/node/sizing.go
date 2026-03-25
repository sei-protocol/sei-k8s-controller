package node

import (
	seiconfig "github.com/sei-protocol/sei-config"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
)

// nodeMode returns the sei-config mode string for the node based on which
// sub-spec is populated. Falls back to "full" if none is set.
func nodeMode(node *seiv1alpha1.SeiNode) string {
	p, err := planner.ForNode(node, "")
	if err != nil {
		return string(seiconfig.ModeFull)
	}
	return p.Mode()
}

// defaultResourcesForMode returns CPU and memory requests for the seid
// container based on the node's operating mode.
func defaultResourcesForMode(mode string, platform PlatformConfig) corev1.ResourceRequirements {
	switch mode {
	case string(seiconfig.ModeArchive):
		return makeResources(platform.ResourceCPUArchive, platform.ResourceMemArchive)
	default:
		return makeResources(platform.ResourceCPUDefault, platform.ResourceMemDefault)
	}
}

// defaultStorageForMode returns the StorageClass name and PVC size for a
// node based on its operating mode.
func defaultStorageForMode(mode string, platform PlatformConfig) (storageClass string, size string) {
	switch mode {
	case string(seiconfig.ModeArchive):
		return platform.StorageClassPerf, platform.StorageSizeArchive
	case string(seiconfig.ModeFull), string(seiconfig.ModeValidator):
		return platform.StorageClassPerf, platform.StorageSizeDefault
	default:
		return platform.StorageClassDefault, platform.StorageSizeDefault
	}
}

func makeResources(cpu, memory string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(memory),
		},
	}
}
