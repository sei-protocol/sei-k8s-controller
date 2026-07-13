package platform

import (
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/yaml"
)

// Environment-variable names. SEI_CONTROLLER_CONFIG points at the read-only
// app-config file (a GitOps-written ConfigMap mounted as a directory). The
// gateway vars stay env-sourced pending their removal from the controller in
// the GitOps networking move (PLT-451); all other infra config is file-sourced.
const (
	envControllerConfig = "SEI_CONTROLLER_CONFIG"

	envGatewayName         = "SEI_GATEWAY_NAME"
	envGatewayNamespace    = "SEI_GATEWAY_NAMESPACE"
	envGatewayDomain       = "SEI_GATEWAY_DOMAIN"
	envGatewayPublicDomain = "SEI_GATEWAY_PUBLIC_DOMAIN"
)

// Load resolves the platform Config at startup. Infra config is read from the
// app-config file (SEI_CONTROLLER_CONFIG → FileConfig), which is authoritative.
// The networking/gateway fields and the config-file path itself stay
// env-sourced, pending the gateway fields' removal in the GitOps networking
// move (PLT-451).
//
// The file is read once here; infra changes require a controller restart. The
// stateSync section is read per-reconcile elsewhere (it hot-reloads). Caller is
// expected to run Config.Validate after Load.
func Load() (Config, error) {
	path := strings.TrimSpace(os.Getenv(envControllerConfig))
	file, err := ReadFileConfig(path)
	if err != nil {
		return Config{}, err
	}

	return Config{
		NodepoolName:      file.Scheduling.NodepoolName,
		NodepoolArchive:   file.Scheduling.NodepoolArchive,
		NodepoolValidator: file.Scheduling.NodepoolValidator,
		TolerationKey:     file.Scheduling.TolerationKey,
		ServiceAccount:    file.Scheduling.ServiceAccount,

		StorageClassPerf:    file.Storage.ClassPerf,
		StorageClassDefault: file.Storage.ClassDefault,
		StorageClassArchive: file.Storage.ClassArchive,
		StorageSizeDefault:  file.Storage.SizeDefault,
		StorageSizeArchive:  file.Storage.SizeArchive,

		NodeResourcesValidator: file.Resources.Validator,
		NodeResourcesNode:      file.Resources.Node,
		NodeResourcesReplayer:  file.Resources.Replayer,
		NodeResourcesArchive:   file.Resources.Archive,

		SnapshotBucket: file.Snapshot.Bucket,
		SnapshotRegion: file.Snapshot.Region,

		ResultExportBucket: file.ResultExport.Bucket,
		ResultExportRegion: file.ResultExport.Region,
		ResultExportPrefix: file.ResultExport.Prefix,

		GenesisBucket: file.Genesis.Bucket,
		GenesisRegion: file.Genesis.Region,

		SidecarImage:        file.Images.Sidecar,
		KubeRBACProxyImage:  file.Images.KubeRBACProxy,
		CosmosExporterImage: file.Images.CosmosExporter,

		// Networking/gateway: env-only, pending removal in the GitOps networking
		// move (PLT-451).
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
