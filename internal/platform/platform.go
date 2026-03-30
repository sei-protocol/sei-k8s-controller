package platform

// Config holds infrastructure-level settings that vary per deployment
// environment. Values are read from environment variables in main.go with
// sensible defaults.
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
}

// DefaultConfig returns Config with production defaults.
func DefaultConfig() Config {
	return Config{
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
		SnapshotRegion:      "eu-central-1",
	}
}
