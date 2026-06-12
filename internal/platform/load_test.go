package platform

import (
	"os"
	"path/filepath"
	"testing"
)

// envNodepool is asserted in multiple fallback cases, so it's named (goconst).
const envNodepool = "env-nodepool"

// setMigratedEnv sets every migrated infra env var to a recognizable "env-"
// prefixed value so a test can assert which source a resolved field came from.
func setMigratedEnv(t *testing.T) {
	t.Helper()
	for _, kv := range [][2]string{
		{envNodepoolName, envNodepool},
		{envNodepoolArchive, "env-nodepool-archive"},
		{envTolerationKey, "env-toleration"},
		{envServiceAccount, "env-sa"},
		{envStorageClassPerf, "env-perf"},
		{envStorageClassDefault, "env-default"},
		{envStorageClassArchive, "env-archive"},
		{envStorageSizeDefault, "env-size-default"},
		{envStorageSizeArchive, "env-size-archive"},
		{envResourceCPUArchive, "env-cpu-archive"},
		{envResourceMemArchive, "env-mem-archive"},
		{envResourceCPUDefault, "env-cpu-default"},
		{envResourceMemDefault, "env-mem-default"},
		{envSnapshotBucket, "env-snap-bucket"},
		{envSnapshotRegion, "env-snap-region"},
		{envResultExportBucket, "env-export-bucket"},
		{envResultExportRegion, "env-export-region"},
		{envResultExportPrefix, "env-export-prefix"},
		{envGenesisBucket, "env-genesis-bucket"},
		{envGenesisRegion, "env-genesis-region"},
		{envSidecarImage, "env-sidecar"},
		{envKubeRBACProxyImage, "env-rbac-proxy"},
		{envCosmosExporterImage, "env-cosmos-exporter"},
		{envGatewayName, "env-gw-name"},
		{envGatewayNamespace, "env-gw-ns"},
		{envGatewayDomain, "env-gw-domain"},
		{envGatewayPublicDomain, "env-gw-public"},
	} {
		t.Setenv(kv[0], kv[1])
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// No file configured: every infra field resolves from the environment.
func TestLoad_NoFile_AllEnv(t *testing.T) {
	setMigratedEnv(t)
	t.Setenv(envControllerConfig, "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.NodepoolName != envNodepool || cfg.SnapshotBucket != "env-snap-bucket" || cfg.SidecarImage != "env-sidecar" {
		t.Errorf("expected env-sourced values, got nodepool=%q snapshot=%q sidecar=%q",
			cfg.NodepoolName, cfg.SnapshotBucket, cfg.SidecarImage)
	}
	if cfg.ControllerConfigFile != "" {
		t.Errorf("ControllerConfigFile = %q, want empty", cfg.ControllerConfigFile)
	}
}

// A field present in the file wins; a field absent from the file falls back to
// its env var. Networking/gateway fields are always env-sourced.
func TestLoad_FileWinsEnvFallback(t *testing.T) {
	setMigratedEnv(t)
	path := writeConfig(t, `
scheduling:
  nodepoolName: file-nodepool
  serviceAccount: file-sa
storage:
  classPerf: file-perf
images:
  sidecar: file-sidecar
`)
	t.Setenv(envControllerConfig, path)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// File-sourced.
	if cfg.NodepoolName != "file-nodepool" {
		t.Errorf("NodepoolName = %q, want file-nodepool", cfg.NodepoolName)
	}
	if cfg.ServiceAccount != "file-sa" {
		t.Errorf("ServiceAccount = %q, want file-sa", cfg.ServiceAccount)
	}
	if cfg.StorageClassPerf != "file-perf" {
		t.Errorf("StorageClassPerf = %q, want file-perf", cfg.StorageClassPerf)
	}
	if cfg.SidecarImage != "file-sidecar" {
		t.Errorf("SidecarImage = %q, want file-sidecar", cfg.SidecarImage)
	}

	// Env fallback (absent from file).
	if cfg.NodepoolArchive != "env-nodepool-archive" {
		t.Errorf("NodepoolArchive = %q, want env fallback", cfg.NodepoolArchive)
	}
	if cfg.TolerationKey != "env-toleration" {
		t.Errorf("TolerationKey = %q, want env fallback", cfg.TolerationKey)
	}

	// Networking/gateway: always env, never file.
	if cfg.GatewayName != "env-gw-name" || cfg.GatewayDomain != "env-gw-domain" {
		t.Errorf("gateway fields should be env-sourced, got name=%q domain=%q", cfg.GatewayName, cfg.GatewayDomain)
	}
	if cfg.ControllerConfigFile != path {
		t.Errorf("ControllerConfigFile = %q, want %q", cfg.ControllerConfigFile, path)
	}
}

// A configured-but-missing file is not an error (the file is opt-in); resolution
// falls back to the environment.
func TestLoad_MissingFileFallsBackToEnv(t *testing.T) {
	setMigratedEnv(t)
	t.Setenv(envControllerConfig, filepath.Join(t.TempDir(), "absent.yaml"))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NodepoolName != envNodepool {
		t.Errorf("NodepoolName = %q, want env fallback", cfg.NodepoolName)
	}
}

// Malformed YAML is a hard error — a present-but-broken file must not silently
// fall back to env (that would mask an operator mistake).
func TestLoad_MalformedFile_Errors(t *testing.T) {
	path := writeConfig(t, "scheduling: [not-a-map")
	t.Setenv(envControllerConfig, path)

	if _, err := Load(); err == nil {
		t.Fatal("expected error for malformed config file, got nil")
	}
}

func TestReadFileConfig_EmptyPath(t *testing.T) {
	cfg, err := ReadFileConfig("")
	if err != nil {
		t.Fatalf("ReadFileConfig(\"\"): %v", err)
	}
	if cfg.StateSync.Syncers != nil || cfg.Scheduling.NodepoolName != "" {
		t.Errorf("empty path should yield zero FileConfig, got %+v", cfg)
	}
}
