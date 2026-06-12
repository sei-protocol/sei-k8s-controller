package platform

import (
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/yaml"
)

// Environment-variable names. SEI_CONTROLLER_CONFIG points at the read-only
// app-config file (a GitOps-written ConfigMap mounted as a directory); the rest
// are the historical infra knobs Load falls back to when a field is absent from
// that file. Single source of truth — referenced by Load and Config.Validate.
const (
	envControllerConfig = "SEI_CONTROLLER_CONFIG"

	envNodepoolName    = "SEI_NODEPOOL_NAME"
	envNodepoolArchive = "SEI_NODEPOOL_ARCHIVE"
	envTolerationKey   = "SEI_TOLERATION_KEY"
	envServiceAccount  = "SEI_SERVICE_ACCOUNT"

	envStorageClassPerf    = "SEI_STORAGE_CLASS_PERF"
	envStorageClassDefault = "SEI_STORAGE_CLASS_DEFAULT"
	envStorageClassArchive = "SEI_STORAGE_CLASS_ARCHIVE"
	envStorageSizeDefault  = "SEI_STORAGE_SIZE_DEFAULT"
	envStorageSizeArchive  = "SEI_STORAGE_SIZE_ARCHIVE"

	envResourceCPUArchive = "SEI_RESOURCE_CPU_ARCHIVE"
	envResourceMemArchive = "SEI_RESOURCE_MEM_ARCHIVE"
	envResourceCPUDefault = "SEI_RESOURCE_CPU_DEFAULT"
	envResourceMemDefault = "SEI_RESOURCE_MEM_DEFAULT"

	envSnapshotBucket = "SEI_SNAPSHOT_BUCKET"
	envSnapshotRegion = "SEI_SNAPSHOT_REGION"

	envResultExportBucket = "SEI_RESULT_EXPORT_BUCKET"
	envResultExportRegion = "SEI_RESULT_EXPORT_REGION"
	envResultExportPrefix = "SEI_RESULT_EXPORT_PREFIX"

	envGenesisBucket = "SEI_GENESIS_BUCKET"
	envGenesisRegion = "SEI_GENESIS_REGION"

	envSidecarImage        = "SEI_SIDECAR_IMAGE"
	envKubeRBACProxyImage  = "SEI_KUBE_RBAC_PROXY_IMAGE"
	envCosmosExporterImage = "SEI_COSMOS_EXPORTER_IMAGE"

	envGatewayName         = "SEI_GATEWAY_NAME"
	envGatewayNamespace    = "SEI_GATEWAY_NAMESPACE"
	envGatewayDomain       = "SEI_GATEWAY_DOMAIN"
	envGatewayPublicDomain = "SEI_GATEWAY_PUBLIC_DOMAIN"
)

// Load resolves the platform Config at startup. A non-empty value in the
// app-config file wins; an absent infra field falls back to its historical env
// var, so an unset SEI_CONTROLLER_CONFIG yields the original all-env behavior.
// That env fallback is transitional — removed once the ConfigMap is populated
// everywhere (PLT-475). Networking/gateway fields and the config-file path
// itself are env-sourced.
//
// The file is read once here; infra changes therefore require a controller
// restart. The stateSync section is read per-reconcile elsewhere (it hot-reloads).
// Caller is expected to run Config.Validate after Load.
func Load() (Config, error) {
	path := strings.TrimSpace(os.Getenv(envControllerConfig))
	file, err := ReadFileConfig(path)
	if err != nil {
		return Config{}, err
	}

	return Config{
		NodepoolName:    fileOrEnv(file.Scheduling.NodepoolName, envNodepoolName),
		NodepoolArchive: fileOrEnv(file.Scheduling.NodepoolArchive, envNodepoolArchive),
		TolerationKey:   fileOrEnv(file.Scheduling.TolerationKey, envTolerationKey),
		ServiceAccount:  fileOrEnv(file.Scheduling.ServiceAccount, envServiceAccount),

		StorageClassPerf:    fileOrEnv(file.Storage.ClassPerf, envStorageClassPerf),
		StorageClassDefault: fileOrEnv(file.Storage.ClassDefault, envStorageClassDefault),
		StorageClassArchive: fileOrEnv(file.Storage.ClassArchive, envStorageClassArchive),
		StorageSizeDefault:  fileOrEnv(file.Storage.SizeDefault, envStorageSizeDefault),
		StorageSizeArchive:  fileOrEnv(file.Storage.SizeArchive, envStorageSizeArchive),

		ResourceCPUArchive: fileOrEnv(file.Resources.CPUArchive, envResourceCPUArchive),
		ResourceMemArchive: fileOrEnv(file.Resources.MemArchive, envResourceMemArchive),
		ResourceCPUDefault: fileOrEnv(file.Resources.CPUDefault, envResourceCPUDefault),
		ResourceMemDefault: fileOrEnv(file.Resources.MemDefault, envResourceMemDefault),

		SnapshotBucket: fileOrEnv(file.Snapshot.Bucket, envSnapshotBucket),
		SnapshotRegion: fileOrEnv(file.Snapshot.Region, envSnapshotRegion),

		ResultExportBucket: fileOrEnv(file.ResultExport.Bucket, envResultExportBucket),
		ResultExportRegion: fileOrEnv(file.ResultExport.Region, envResultExportRegion),
		ResultExportPrefix: fileOrEnv(file.ResultExport.Prefix, envResultExportPrefix),

		GenesisBucket: fileOrEnv(file.Genesis.Bucket, envGenesisBucket),
		GenesisRegion: fileOrEnv(file.Genesis.Region, envGenesisRegion),

		SidecarImage:        fileOrEnv(file.Images.Sidecar, envSidecarImage),
		KubeRBACProxyImage:  fileOrEnv(file.Images.KubeRBACProxy, envKubeRBACProxyImage),
		CosmosExporterImage: fileOrEnv(file.Images.CosmosExporter, envCosmosExporterImage),

		// Networking/gateway: env-only, pending removal in the GitOps networking
		// move (PLT-451). Not migrated to the file to avoid migrate-then-delete.
		GatewayName:         os.Getenv(envGatewayName),
		GatewayNamespace:    os.Getenv(envGatewayNamespace),
		GatewayDomain:       os.Getenv(envGatewayDomain),
		GatewayPublicDomain: os.Getenv(envGatewayPublicDomain),

		ControllerConfigFile: path,
	}, nil
}

// ReadFileConfig reads and decodes the app-config file. An empty path or a
// missing file yields a zero FileConfig (the file is opt-in) — only a present
// file that can't be read or parsed is an error. It is the single read path for
// the config file, shared by Load (startup) and the per-reconcile state-sync
// reader.
func ReadFileConfig(path string) (FileConfig, error) {
	if strings.TrimSpace(path) == "" {
		return FileConfig{}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return FileConfig{}, nil
		}
		return FileConfig{}, fmt.Errorf("reading controller config file %q: %w", path, err)
	}
	var cfg FileConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return FileConfig{}, fmt.Errorf("parsing controller config file %q: %w", path, err)
	}
	return cfg, nil
}

// fileOrEnv returns the file value when non-empty, otherwise the named env var
// (the transitional fallback).
func fileOrEnv(fileVal, envVar string) string {
	if strings.TrimSpace(fileVal) != "" {
		return fileVal
	}
	return os.Getenv(envVar)
}
