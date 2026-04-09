// Package platformtest provides test fixtures for platform.Config.
// This package must not be imported outside of test code.
package platformtest

import "github.com/sei-protocol/sei-k8s-controller/internal/platform"

// Config returns a fully-populated platform.Config suitable for unit tests.
func Config() platform.Config {
	return platform.Config{
		NodepoolName:        "sei-node",
		TolerationKey:       "sei.io/workload",
		TolerationVal:       "sei-node",
		ServiceAccount:      "seid-node",
		StorageClassPerf:    "gp3-10k-750",
		StorageClassDefault: "gp3",
		StorageSizeDefault:  "2000Gi",
		StorageSizeArchive:  "4000Gi",
		ResourceCPUArchive:  "16",
		ResourceMemArchive:  "256Gi",
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
		GatewaySectionName:  "",
	}
}
