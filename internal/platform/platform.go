package platform

import (
	"fmt"
	"strings"
)

const (
	// DataDir is the mount path for the sei data volume inside node pods.
	// `/.sei` follows the conventional Cosmos SDK home layout (`~/.sei`),
	// resolved against the seid container's HOME env var. Lock-step with
	// the SEI_HOME env var injected on the sidecar container; both flow
	// from this single constant.
	DataDir = "/.sei"

	// modeArchive matches seiconfig.ModeArchive without importing sei-config.
	modeArchive = "archive"
)

// Config holds infrastructure-level settings that vary per deployment
// environment. All fields are required and read from environment variables
// in main.go. See platformtest.Config() for test fixtures.
type Config struct {
	NodepoolName        string
	NodepoolArchive     string
	TolerationKey       string
	ServiceAccount      string
	StorageClassPerf    string
	StorageClassDefault string
	StorageClassArchive string
	StorageSizeDefault  string
	StorageSizeArchive  string
	ResourceCPUArchive  string
	ResourceMemArchive  string
	ResourceCPUDefault  string
	ResourceMemDefault  string
	SnapshotBucket      string
	SnapshotRegion      string

	ResultExportBucket string
	ResultExportRegion string
	ResultExportPrefix string

	GenesisBucket string
	GenesisRegion string

	GatewayName         string
	GatewayNamespace    string
	GatewayDomain       string
	GatewayPublicDomain string

	KubeRBACProxyImage string
	SidecarImage       string

	// CosmosExporterImage is the sei-cosmos-exporter sidecar image.
	// Required when any SeiNode sets spec.cosmosExporter: true.
	CosmosExporterImage string
}

// NodepoolForMode returns the Karpenter NodePool name for the given
// sei-config mode string. Archive nodes use a dedicated pool; all
// other modes share the default pool.
func (c Config) NodepoolForMode(mode string) string {
	if mode == modeArchive {
		return c.NodepoolArchive
	}
	return c.NodepoolName
}

// Validate returns an error if required fields are missing.
func (c Config) Validate() error {
	required := map[string]string{
		"SEI_NODEPOOL_NAME":         c.NodepoolName,
		"SEI_TOLERATION_KEY":        c.TolerationKey,
		"SEI_SERVICE_ACCOUNT":       c.ServiceAccount,
		"SEI_STORAGE_CLASS_PERF":    c.StorageClassPerf,
		"SEI_STORAGE_CLASS_DEFAULT": c.StorageClassDefault,
		"SEI_STORAGE_CLASS_ARCHIVE": c.StorageClassArchive,
		"SEI_STORAGE_SIZE_DEFAULT":  c.StorageSizeDefault,
		"SEI_STORAGE_SIZE_ARCHIVE":  c.StorageSizeArchive,
		"SEI_NODEPOOL_ARCHIVE":      c.NodepoolArchive,
		"SEI_RESOURCE_CPU_ARCHIVE":  c.ResourceCPUArchive,
		"SEI_RESOURCE_MEM_ARCHIVE":  c.ResourceMemArchive,
		"SEI_RESOURCE_CPU_DEFAULT":  c.ResourceCPUDefault,
		"SEI_RESOURCE_MEM_DEFAULT":  c.ResourceMemDefault,
		"SEI_SNAPSHOT_BUCKET":       c.SnapshotBucket,
		"SEI_SNAPSHOT_REGION":       c.SnapshotRegion,
		"SEI_RESULT_EXPORT_BUCKET":  c.ResultExportBucket,
		"SEI_RESULT_EXPORT_REGION":  c.ResultExportRegion,
		"SEI_RESULT_EXPORT_PREFIX":  c.ResultExportPrefix,
		"SEI_GENESIS_BUCKET":        c.GenesisBucket,
		"SEI_GENESIS_REGION":        c.GenesisRegion,
		"SEI_GATEWAY_NAME":          c.GatewayName,
		"SEI_GATEWAY_NAMESPACE":     c.GatewayNamespace,
		"SEI_GATEWAY_DOMAIN":        c.GatewayDomain,
		"SEI_SIDECAR_IMAGE":         c.SidecarImage,
		"SEI_KUBE_RBAC_PROXY_IMAGE": c.KubeRBACProxyImage,
	}
	for name, val := range required {
		if strings.TrimSpace(val) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	return nil
}
