package node

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	modeArchive   = "archive"
	modeRPC       = "rpc"
	modeFull      = "full"
	modeValidator = "validator"
	modeSeed      = "seed"

	storageClassPerf    = "gp3-perf"
	storageClassDefault = "gp3"
)

// defaultResourcesForMode returns CPU and memory requests for the seid
// container based on the node's operating mode. These requests drive
// Karpenter's instance selection — the scheduler places the pod on a node
// whose allocatable resources satisfy the request.
//
// Values are derived from sei-infra production sizing, scaled down for
// staging parity while remaining large enough for SeiDB's memIAVL page
// cache to function without constant disk reads.
func defaultResourcesForMode(mode string) corev1.ResourceRequirements {
	switch mode {
	case modeArchive:
		return makeResources("8", "48Gi")
	case modeRPC:
		return makeResources("8", "48Gi")
	case modeFull:
		return makeResources("4", "32Gi")
	case modeValidator:
		return makeResources("4", "32Gi")
	case modeSeed:
		return makeResources("2", "8Gi")
	default:
		return makeResources("4", "32Gi")
	}
}

// defaultStorageForMode returns the StorageClass name and PVC size for a
// node based on its operating mode. Heavier modes use the gp3-perf class
// with provisioned IOPS; lightweight modes fall back to baseline gp3.
func defaultStorageForMode(mode string) (storageClass string, size string) {
	switch mode {
	case modeArchive:
		return storageClassPerf, "2000Gi"
	case modeRPC:
		return storageClassPerf, "1500Gi"
	case modeFull, modeValidator:
		return storageClassPerf, "1000Gi"
	case modeSeed:
		return storageClassDefault, "500Gi"
	default:
		return storageClassDefault, "1000Gi"
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
