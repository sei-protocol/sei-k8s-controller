package platform

import "fmt"

const (
	// DefaultSidecarImage is the seictl sidecar image used when not overridden
	// by the SeiNode spec. Shared between the node controller and bootstrap task.
	DefaultSidecarImage = "ghcr.io/sei-protocol/seictl@sha256:63860a7cf1810e70cc8647d72ff705f87a203250b12bbdec2f88f26b850b628e"

	// DataDir is the mount path for the sei data volume inside node pods.
	DataDir = "/sei"
)

// Config holds infrastructure-level settings that vary per deployment
// environment. All fields are required and read from environment variables
// in main.go. See platformtest.Config() for test fixtures.
type Config struct {
	NodepoolName        string
	TolerationKey       string
	TolerationVal       string
	ServiceAccount      string
	StorageClassPerf    string
	StorageClassDefault string
	StorageSizeDefault  string
	StorageSizeArchive  string
	ResourceCPUArchive  string
	ResourceMemArchive  string
	ResourceCPUDefault  string
	ResourceMemDefault  string
	SnapshotRegion      string

	ResultExportBucket string
	ResultExportRegion string
	ResultExportPrefix string

	GenesisBucket string
	GenesisRegion string
}

// Validate returns an error if required fields are missing.
func (c Config) Validate() error {
	required := map[string]string{
		"SEI_NODEPOOL_NAME":         c.NodepoolName,
		"SEI_TOLERATION_KEY":        c.TolerationKey,
		"SEI_TOLERATION_VALUE":      c.TolerationVal,
		"SEI_SERVICE_ACCOUNT":       c.ServiceAccount,
		"SEI_STORAGE_CLASS_PERF":    c.StorageClassPerf,
		"SEI_STORAGE_CLASS_DEFAULT": c.StorageClassDefault,
		"SEI_STORAGE_SIZE_DEFAULT":  c.StorageSizeDefault,
		"SEI_STORAGE_SIZE_ARCHIVE":  c.StorageSizeArchive,
		"SEI_RESOURCE_CPU_ARCHIVE":  c.ResourceCPUArchive,
		"SEI_RESOURCE_MEM_ARCHIVE":  c.ResourceMemArchive,
		"SEI_RESOURCE_CPU_DEFAULT":  c.ResourceCPUDefault,
		"SEI_RESOURCE_MEM_DEFAULT":  c.ResourceMemDefault,
		"SEI_SNAPSHOT_REGION":       c.SnapshotRegion,
		"SEI_RESULT_EXPORT_BUCKET":  c.ResultExportBucket,
		"SEI_RESULT_EXPORT_REGION":  c.ResultExportRegion,
		"SEI_RESULT_EXPORT_PREFIX":  c.ResultExportPrefix,
		"SEI_GENESIS_BUCKET":        c.GenesisBucket,
		"SEI_GENESIS_REGION":        c.GenesisRegion,
	}
	for name, val := range required {
		if val == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	return nil
}
