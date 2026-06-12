package platform

import (
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/yaml"
)

// envControllerConfig names the env var pointing at the read-only app-config
// file (a GitOps-written ConfigMap mounted as a directory).
const envControllerConfig = "SEI_CONTROLLER_CONFIG"

// Load resolves the platform Config at startup. For each migrated infra field
// (PLT-475) a non-empty value in the app-config file wins; an absent one falls
// back to its historical env var, so an unset SEI_CONTROLLER_CONFIG yields the
// original all-env behavior. Networking/gateway fields and the config-file path
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
		NodepoolName:    fileOrEnv(file.Scheduling.NodepoolName, "SEI_NODEPOOL_NAME"),
		NodepoolArchive: fileOrEnv(file.Scheduling.NodepoolArchive, "SEI_NODEPOOL_ARCHIVE"),
		TolerationKey:   fileOrEnv(file.Scheduling.TolerationKey, "SEI_TOLERATION_KEY"),
		ServiceAccount:  fileOrEnv(file.Scheduling.ServiceAccount, "SEI_SERVICE_ACCOUNT"),

		StorageClassPerf:    fileOrEnv(file.Storage.ClassPerf, "SEI_STORAGE_CLASS_PERF"),
		StorageClassDefault: fileOrEnv(file.Storage.ClassDefault, "SEI_STORAGE_CLASS_DEFAULT"),
		StorageClassArchive: fileOrEnv(file.Storage.ClassArchive, "SEI_STORAGE_CLASS_ARCHIVE"),
		StorageSizeDefault:  fileOrEnv(file.Storage.SizeDefault, "SEI_STORAGE_SIZE_DEFAULT"),
		StorageSizeArchive:  fileOrEnv(file.Storage.SizeArchive, "SEI_STORAGE_SIZE_ARCHIVE"),

		ResourceCPUArchive: fileOrEnv(file.Resources.CPUArchive, "SEI_RESOURCE_CPU_ARCHIVE"),
		ResourceMemArchive: fileOrEnv(file.Resources.MemArchive, "SEI_RESOURCE_MEM_ARCHIVE"),
		ResourceCPUDefault: fileOrEnv(file.Resources.CPUDefault, "SEI_RESOURCE_CPU_DEFAULT"),
		ResourceMemDefault: fileOrEnv(file.Resources.MemDefault, "SEI_RESOURCE_MEM_DEFAULT"),

		SnapshotBucket: fileOrEnv(file.Snapshot.Bucket, "SEI_SNAPSHOT_BUCKET"),
		SnapshotRegion: fileOrEnv(file.Snapshot.Region, "SEI_SNAPSHOT_REGION"),

		ResultExportBucket: fileOrEnv(file.ResultExport.Bucket, "SEI_RESULT_EXPORT_BUCKET"),
		ResultExportRegion: fileOrEnv(file.ResultExport.Region, "SEI_RESULT_EXPORT_REGION"),
		ResultExportPrefix: fileOrEnv(file.ResultExport.Prefix, "SEI_RESULT_EXPORT_PREFIX"),

		GenesisBucket: fileOrEnv(file.Genesis.Bucket, "SEI_GENESIS_BUCKET"),
		GenesisRegion: fileOrEnv(file.Genesis.Region, "SEI_GENESIS_REGION"),

		SidecarImage:        fileOrEnv(file.Images.Sidecar, "SEI_SIDECAR_IMAGE"),
		KubeRBACProxyImage:  fileOrEnv(file.Images.KubeRBACProxy, "SEI_KUBE_RBAC_PROXY_IMAGE"),
		CosmosExporterImage: fileOrEnv(file.Images.CosmosExporter, "SEI_COSMOS_EXPORTER_IMAGE"),

		// Networking/gateway: env-only, pending removal in the GitOps networking
		// move (PLT-451). Not migrated to the file to avoid migrate-then-delete.
		GatewayName:         os.Getenv("SEI_GATEWAY_NAME"),
		GatewayNamespace:    os.Getenv("SEI_GATEWAY_NAMESPACE"),
		GatewayDomain:       os.Getenv("SEI_GATEWAY_DOMAIN"),
		GatewayPublicDomain: os.Getenv("SEI_GATEWAY_PUBLIC_DOMAIN"),

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
// (the transitional PLT-475 fallback).
func fileOrEnv(fileVal, envVar string) string {
	if strings.TrimSpace(fileVal) != "" {
		return fileVal
	}
	return os.Getenv(envVar)
}
