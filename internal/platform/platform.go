// Package platform exposes the cluster-environment knobs (storage classes,
// node pools, sidecar/exporter images, S3 buckets, Gateway coordinates)
// the controller consumes when generating workload resources.
package platform

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
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

	// modeValidator matches seiconfig.ModeValidator without importing sei-config.
	modeValidator = "validator"
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
	NodepoolValidator   string
	TolerationKey       string
	ServiceAccount      string
	StorageClassPerf    string
	StorageClassDefault string
	StorageClassArchive string
	StorageSizeDefault  string
	StorageSizeArchive  string

	// Per-role seid-container resource overrides, keyed by sei.io/role. Each is
	// optional: an unset field (or unset sub-field) falls back to the
	// code-authoritative, prod-safe default for that role in internal/noderesource.
	// See ResourcesConfig.
	NodeResourcesValidator ResourceOverride
	NodeResourcesNode      ResourceOverride
	NodeResourcesReplayer  ResourceOverride
	NodeResourcesArchive   ResourceOverride

	SnapshotBucket string
	SnapshotRegion string

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
	NodepoolName      string `json:"nodepoolName"`
	NodepoolArchive   string `json:"nodepoolArchive"`
	NodepoolValidator string `json:"nodepoolValidator"`
	TolerationKey     string `json:"tolerationKey"`
	ServiceAccount    string `json:"serviceAccount"`
}

// StorageConfig holds the PVC storage classes and sizes for default and archive nodes.
type StorageConfig struct {
	ClassPerf    string `json:"classPerf"`
	ClassDefault string `json:"classDefault"`
	ClassArchive string `json:"classArchive"`
	SizeDefault  string `json:"sizeDefault"`
	SizeArchive  string `json:"sizeArchive"`
}

// ResourcesConfig holds seid-container resource sizing.
//
// The per-role blocks size the seid container: each is optional and falls back
// to the code-authoritative default for that role in internal/noderesource.
// Both the long-running node container and the transient genesis-bootstrap Job
// take this per-role footprint (the Job via noderesource.ResourcesForNode).
type ResourcesConfig struct {
	// Per-role overrides, keyed to sei.io/role.
	Validator ResourceOverride `json:"validator"`
	Node      ResourceOverride `json:"node"`
	Replayer  ResourceOverride `json:"replayer"`
	Archive   ResourceOverride `json:"archive"`
}

// ResourceOverride is a per-mode seid-container resource override. Both fields
// are optional and, when set, must be valid Kubernetes quantity strings.
//
// Memory is a single value used as BOTH the memory request and the memory limit
// (memory-Guaranteed: the mode's footprint is hard-reserved and hard-capped).
// CPU is request-only by design: seid is work-conserving and CPU is
// compressible, so a CPU limit would only throttle seid's measured 10–12 core
// consensus/replay bursts — there is deliberately no CPU-limit field.
type ResourceOverride struct {
	CPURequest string `json:"cpuRequest"`
	Memory     string `json:"memory"`
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
// sei-config mode string. Archive and validator nodes each use a
// dedicated pool; all other modes share the default pool.
func (c Config) NodepoolForMode(mode string) string {
	switch mode {
	case modeArchive:
		return c.NodepoolArchive
	case modeValidator:
		return c.NodepoolValidator
	default:
		return c.NodepoolName
	}
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
		{"scheduling.nodepoolValidator", c.NodepoolValidator},
		{"scheduling.tolerationKey", c.TolerationKey},
		{"scheduling.serviceAccount", c.ServiceAccount},
		{"storage.classPerf", c.StorageClassPerf},
		{"storage.classDefault", c.StorageClassDefault},
		{"storage.classArchive", c.StorageClassArchive},
		{"storage.sizeDefault", c.StorageSizeDefault},
		{"storage.sizeArchive", c.StorageSizeArchive},
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

	overrides := []struct {
		key string
		val ResourceOverride
	}{
		{"resources.validator", c.NodeResourcesValidator},
		{"resources.node", c.NodeResourcesNode},
		{"resources.replayer", c.NodeResourcesReplayer},
		{"resources.archive", c.NodeResourcesArchive},
	}
	for _, o := range overrides {
		if err := o.val.validate(o.key); err != nil {
			return err
		}
	}
	return nil
}

// validate parse-checks the override's quantity strings (fail-fast at startup so
// the reconcile path can MustParse them). Empty sub-fields are skipped: they
// fall back to the per-mode code default.
func (o ResourceOverride) validate(key string) error {
	fields := []struct {
		name string
		val  string
	}{
		{"cpuRequest", o.CPURequest},
		{"memory", o.Memory},
	}
	for _, f := range fields {
		if strings.TrimSpace(f.val) == "" {
			continue
		}
		if _, err := resource.ParseQuantity(f.val); err != nil {
			return fmt.Errorf("%s.%s: invalid quantity %q: %w", key, f.name, f.val, err)
		}
	}
	return nil
}
