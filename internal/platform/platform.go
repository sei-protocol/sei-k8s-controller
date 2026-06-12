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
// environment. It is resolved by Load: the infra fields are read from the
// app-config file (FileConfig) when present, falling back to their historical
// env vars (PLT-475, transitional); the networking/gateway fields and
// ControllerConfigFile are env-sourced. Fields are required unless documented
// otherwise — ControllerConfigFile is optional (state-sync is opt-in). See
// platformtest.Config() for test fixtures.
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
//
// The infra sections (scheduling, storage, resources, snapshot, resultExport,
// genesis, images) carry the infra config that was historically env-sourced.
// They are resolved once at startup by Load, the file value winning over the
// env fallback. The stateSync section is read per-reconcile (it hot-reloads);
// the infra sections are not (an infra change warrants a restart).
//
// Networking/gateway config is deliberately absent — it stays env-sourced
// pending its removal from the controller in the GitOps networking move (PLT-451).
type FileConfig struct {
	StateSync    StateSyncConfig    `json:"stateSync"`
	Scheduling   SchedulingConfig   `json:"scheduling"`
	Storage      StorageConfig      `json:"storage"`
	Resources    ResourcesConfig    `json:"resources"`
	Snapshot     BucketConfig       `json:"snapshot"`
	ResultExport ResultExportConfig `json:"resultExport"`
	Genesis      BucketConfig       `json:"genesis"`
	Images       ImagesConfig       `json:"images"`
}

// StateSyncConfig is the state-sync section of the application config.
type StateSyncConfig struct {
	// Syncers maps chainID -> bare host:port RPC endpoints (no scheme; sidecar adds it).
	Syncers map[string][]string `json:"syncers"`
}

// SchedulingConfig places node pods onto Karpenter pools and the seid service account.
type SchedulingConfig struct {
	NodepoolName    string `json:"nodepoolName"`
	NodepoolArchive string `json:"nodepoolArchive"`
	TolerationKey   string `json:"tolerationKey"`
	ServiceAccount  string `json:"serviceAccount"`
}

// StorageConfig holds the PVC storage classes and sizes for default and archive nodes.
type StorageConfig struct {
	ClassPerf    string `json:"classPerf"`
	ClassDefault string `json:"classDefault"`
	ClassArchive string `json:"classArchive"`
	SizeDefault  string `json:"sizeDefault"`
	SizeArchive  string `json:"sizeArchive"`
}

// ResourcesConfig holds the CPU/memory requests for default and archive nodes.
type ResourcesConfig struct {
	CPUArchive string `json:"cpuArchive"`
	MemArchive string `json:"memArchive"`
	CPUDefault string `json:"cpuDefault"`
	MemDefault string `json:"memDefault"`
}

// BucketConfig is an S3 bucket + region pair (snapshot, genesis).
type BucketConfig struct {
	Bucket string `json:"bucket"`
	Region string `json:"region"`
}

// ResultExportConfig is the shadow-replay result-export bucket, region, and key prefix.
type ResultExportConfig struct {
	Bucket string `json:"bucket"`
	Region string `json:"region"`
	Prefix string `json:"prefix"`
}

// ImagesConfig holds the sidecar container images attached to every SeiNode pod.
type ImagesConfig struct {
	Sidecar        string `json:"sidecar"`
	KubeRBACProxy  string `json:"kubeRBACProxy"`
	CosmosExporter string `json:"cosmosExporter"`
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

// Validate returns an error if a required field is missing from both the
// app-config file and the environment. The source label names the file key and
// the env var so the error points at either fix; networking/gateway fields name
// only their env var.
func (c Config) Validate() error {
	// fileKey is empty for env-only fields (networking/gateway); they report
	// just the env var. Slice order is the report order for the first missing.
	required := []struct {
		fileKey string
		envVar  string
		val     string
	}{
		{"scheduling.nodepoolName", envNodepoolName, c.NodepoolName},
		{"scheduling.nodepoolArchive", envNodepoolArchive, c.NodepoolArchive},
		{"scheduling.tolerationKey", envTolerationKey, c.TolerationKey},
		{"scheduling.serviceAccount", envServiceAccount, c.ServiceAccount},
		{"storage.classPerf", envStorageClassPerf, c.StorageClassPerf},
		{"storage.classDefault", envStorageClassDefault, c.StorageClassDefault},
		{"storage.classArchive", envStorageClassArchive, c.StorageClassArchive},
		{"storage.sizeDefault", envStorageSizeDefault, c.StorageSizeDefault},
		{"storage.sizeArchive", envStorageSizeArchive, c.StorageSizeArchive},
		{"resources.cpuArchive", envResourceCPUArchive, c.ResourceCPUArchive},
		{"resources.memArchive", envResourceMemArchive, c.ResourceMemArchive},
		{"resources.cpuDefault", envResourceCPUDefault, c.ResourceCPUDefault},
		{"resources.memDefault", envResourceMemDefault, c.ResourceMemDefault},
		{"snapshot.bucket", envSnapshotBucket, c.SnapshotBucket},
		{"snapshot.region", envSnapshotRegion, c.SnapshotRegion},
		{"resultExport.bucket", envResultExportBucket, c.ResultExportBucket},
		{"resultExport.region", envResultExportRegion, c.ResultExportRegion},
		{"resultExport.prefix", envResultExportPrefix, c.ResultExportPrefix},
		{"genesis.bucket", envGenesisBucket, c.GenesisBucket},
		{"genesis.region", envGenesisRegion, c.GenesisRegion},
		{"images.sidecar", envSidecarImage, c.SidecarImage},
		{"images.kubeRBACProxy", envKubeRBACProxyImage, c.KubeRBACProxyImage},
		{"", envGatewayName, c.GatewayName},
		{"", envGatewayNamespace, c.GatewayNamespace},
		{"", envGatewayDomain, c.GatewayDomain},
	}
	for _, f := range required {
		if strings.TrimSpace(f.val) != "" {
			continue
		}
		if f.fileKey == "" {
			return fmt.Errorf("%s is required", f.envVar)
		}
		return fmt.Errorf("%s (or %s) is required", f.fileKey, f.envVar)
	}
	return nil
}
