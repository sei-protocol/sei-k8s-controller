// Package platform exposes the cluster-environment knobs (storage classes,
// node pools, sidecar/exporter images, S3 buckets, Gateway coordinates)
// the controller consumes when generating workload resources.
package platform

import (
	"fmt"
	"strings"
)

const (
	// HomeDir is the container HOME for seid pods and the parent of the seid
	// data dir. It is an emptyDir mount; the data PVC nests inside it at
	// HomeDir/.sei (kubelet creates the nested mountpoint). Cosmos-SDK derives
	// DefaultNodeHome = $HOME/.sei, so with HOME=HomeDir a bare `seid keys list`
	// / `seid tx sign` resolves to HomeDir/.sei (== DataDir, the data PVC) with
	// no --home flag, matching the org's EC2 fleet convention. DataDir must stay
	// a child of HomeDir, never equal it: HOME == the data dir makes a bare seid
	// nest into DataDir/.sei and leak shell files onto the PVC (locked by
	// TestHomeDirIsDataDirParent; #449).
	HomeDir = "/home/nonroot"

	// DataDir is the mount path for the sei data volume inside node pods, and
	// the seid home passed explicitly via `--home <DataDir>` on every seid
	// invocation. Derived as HomeDir/.sei so a flagless seid CLI (which resolves
	// $HOME/.sei) converges on the same directory. See HomeDir for the
	// parent/child rationale.
	DataDir = HomeDir + "/.sei"

	// modeArchive matches seiconfig.ModeArchive without importing sei-config.
	modeArchive = "archive"
)

// Config holds infrastructure-level settings that vary per deployment
// environment. It is resolved by Load: infra fields come from the app-config
// file (FileConfig), which is authoritative; the networking/gateway fields and
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
// genesis, images) are the authoritative source for that config, resolved once
// at startup by Load. The stateSync section is read per-reconcile (it
// hot-reloads); the infra sections are not (an infra change warrants a restart).
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

// Validate returns an error if a required field is unset. name is the field's
// app-config file key, except the networking/gateway fields (still env-sourced
// pending PLT-451) which name their env var. Slice order is the report order
// for the first missing field.
func (c Config) Validate() error {
	required := []struct {
		name string
		val  string
	}{
		{"scheduling.nodepoolName", c.NodepoolName},
		{"scheduling.nodepoolArchive", c.NodepoolArchive},
		{"scheduling.tolerationKey", c.TolerationKey},
		{"scheduling.serviceAccount", c.ServiceAccount},
		{"storage.classPerf", c.StorageClassPerf},
		{"storage.classDefault", c.StorageClassDefault},
		{"storage.classArchive", c.StorageClassArchive},
		{"storage.sizeDefault", c.StorageSizeDefault},
		{"storage.sizeArchive", c.StorageSizeArchive},
		{"resources.cpuArchive", c.ResourceCPUArchive},
		{"resources.memArchive", c.ResourceMemArchive},
		{"resources.cpuDefault", c.ResourceCPUDefault},
		{"resources.memDefault", c.ResourceMemDefault},
		{"snapshot.bucket", c.SnapshotBucket},
		{"snapshot.region", c.SnapshotRegion},
		{"resultExport.bucket", c.ResultExportBucket},
		{"resultExport.region", c.ResultExportRegion},
		{"resultExport.prefix", c.ResultExportPrefix},
		{"genesis.bucket", c.GenesisBucket},
		{"genesis.region", c.GenesisRegion},
		{"images.sidecar", c.SidecarImage},
		{"images.kubeRBACProxy", c.KubeRBACProxyImage},
		{"images.cosmosExporter", c.CosmosExporterImage},
		{envGatewayName, c.GatewayName},
		{envGatewayNamespace, c.GatewayNamespace},
		{envGatewayDomain, c.GatewayDomain},
	}
	for _, f := range required {
		if strings.TrimSpace(f.val) == "" {
			return fmt.Errorf("%s is required", f.name)
		}
	}
	return nil
}
