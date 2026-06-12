// Package platform exposes the cluster-environment knobs (storage classes,
// node pools, sidecar/exporter images, S3 buckets, Gateway coordinates)
// the controller consumes when generating workload resources.
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
// environment. Fields are read from environment variables in main.go and are
// required unless documented otherwise — ControllerConfigFile is optional
// (state-sync is opt-in). See platformtest.Config() for test fixtures.
//
// Config is env-sourced infra; FileConfig (below) is the file-sourced
// application config. They are deliberately distinct: ControllerConfigFile is
// the path to the latter, not its contents.
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
	// The cosmos-exporter container is attached to every SeiNode pod.
	CosmosExporterImage string

	// ControllerConfigFile is the path to the read-only application-config file
	// the controller reads (SEI_CONTROLLER_CONFIG). It is the trust root for
	// state-sync today: a GitOps-written ConfigMap mounted read-only (directory
	// mount, not subPath, so atomic ConfigMap swaps propagate without a pod
	// restart). Content is YAML decoded into FileConfig.
	//
	// The file is opt-in, so this may be empty when no node uses state-sync.
	// When a node DOES enable state-sync and this is unset (or the file is
	// missing, or yields <2 entries for its chain), the controller fails closed
	// via StateSyncReady=False/NoSyncersConfigured rather than building a
	// witness-less plan.
	ControllerConfigFile string
}

// FileConfig is the controller's file-sourced application config (SEI_CONTROLLER_CONFIG).
// One section today (StateSync); PLT-475 folds the remaining infra knobs in.
type FileConfig struct {
	StateSync StateSyncConfig `json:"stateSync"`
}

// StateSyncConfig is the state-sync section of the application config.
type StateSyncConfig struct {
	// Syncers maps chainID -> bare host:port RPC endpoints (no scheme; sidecar adds it).
	Syncers map[string][]string `json:"syncers"`
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
