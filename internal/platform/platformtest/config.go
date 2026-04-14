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
		StorageClassArchive: "io2-archive",
		StorageSizeDefault:  "2000Gi",
		StorageSizeArchive:  "25000Gi",
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
	}
}
