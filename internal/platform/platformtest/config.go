// Package platformtest provides test fixtures for platform.Config.
// This package must not be imported outside of test code.
package platformtest

import "github.com/sei-protocol/sei-k8s-controller/internal/platform"

// Config returns a fully-populated platform.Config suitable for unit tests.
func Config() platform.Config {
	return platform.Config{
		NodepoolName:        "sei-node",
		NodepoolArchive:     "sei-archive",
		TolerationKey:       "sei.io/workload",
		ServiceAccount:      "seid-node",
		StorageClassPerf:    "gp3-10k-750",
		StorageClassDefault: "gp3",
		StorageClassArchive: "gp3-archive",
		StorageSizeDefault:  "2000Gi",
		StorageSizeArchive:  "40Ti",
		ResourceCPUArchive:  "48",
		ResourceMemArchive:  "448Gi",
		ResourceCPUDefault:  "4",
		ResourceMemDefault:  "32Gi",
		SnapshotBucket:      "test-sei-snapshots",
		SnapshotRegion:      "us-east-2",
		ResultExportBucket:  "test-sei-shadow-results",
		ResultExportRegion:  "us-east-2",
		ResultExportPrefix:  "shadow-results/",
		GenesisBucket:       "test-sei-k8s-genesis-artifacts",
		GenesisRegion:       "us-east-2",
		GatewayName:         "sei-gateway",
		GatewayNamespace:    "istio-system",
		GatewayDomain:       "test.platform.sei.io",
		KubeRBACProxyImage:  "quay.io/brancz/kube-rbac-proxy:v0.19.1",
		// Arbitrary fixture; not authoritative. Production digest is set
		// via SEI_SIDECAR_IMAGE in the platform repo's controller Deployment.
		SidecarImage:        "ghcr.io/sei-protocol/seictl@sha256:a2af4e1b8ed4c12661a3c98cce050bae3f292cc7560abc2ba98fd7dfc80d9be5",
		CosmosExporterImage: "ghcr.io/sei-protocol/sei-cosmos-exporter@sha256:0000000000000000000000000000000000000000000000000000000000000000",
	}
}
