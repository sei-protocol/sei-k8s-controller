package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fullConfig is a complete, valid app-config file — every required infra field set.
const fullConfig = `
scheduling:
  nodepoolName: file-nodepool
  nodepoolArchive: file-nodepool-archive
  tolerationKey: file-toleration
  serviceAccount: file-sa
storage:
  classPerf: file-perf
  classDefault: file-default
  classArchive: file-archive
  sizeDefault: file-size-default
  sizeArchive: file-size-archive
resources:
  cpuArchive: file-cpu-archive
  memArchive: file-mem-archive
  cpuDefault: file-cpu-default
  memDefault: file-mem-default
snapshot:
  bucket: file-snap-bucket
  region: file-snap-region
resultExport:
  bucket: file-export-bucket
  region: file-export-region
  prefix: file-export-prefix
genesis:
  bucket: file-genesis-bucket
  region: file-genesis-region
images:
  sidecar: file-sidecar
  kubeRBACProxy: file-rbac-proxy
  cosmosExporter: file-cosmos-exporter
`

func setGatewayEnv(t *testing.T) {
	t.Helper()
	t.Setenv(envGatewayName, "env-gw-name")
	t.Setenv(envGatewayNamespace, "env-gw-ns")
	t.Setenv(envGatewayDomain, "env-gw-domain")
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// Infra config is sourced from the file (authoritative); gateway from env. An
// infra env var, even when set, is ignored — there is no fallback.
func TestLoad_InfraFromFile_GatewayFromEnv(t *testing.T) {
	setGatewayEnv(t)
	t.Setenv("SEI_NODEPOOL_NAME", "env-ignored") // no longer consulted
	path := writeConfig(t, fullConfig)
	t.Setenv(envControllerConfig, path)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.NodepoolName != "file-nodepool" {
		t.Errorf("NodepoolName = %q, want file-nodepool (infra env must be ignored)", cfg.NodepoolName)
	}
	if cfg.CosmosExporterImage != "file-cosmos-exporter" {
		t.Errorf("CosmosExporterImage = %q, want file-cosmos-exporter", cfg.CosmosExporterImage)
	}
	if cfg.GatewayName != "env-gw-name" {
		t.Errorf("GatewayName = %q, want env-gw-name", cfg.GatewayName)
	}
	if cfg.ControllerConfigFile != path {
		t.Errorf("ControllerConfigFile = %q, want %q", cfg.ControllerConfigFile, path)
	}
}

// No file means no infra config — Validate fails at startup (no env fallback),
// naming the file key.
func TestLoad_NoFile_FailsValidate(t *testing.T) {
	setGatewayEnv(t)
	t.Setenv("SEI_NODEPOOL_NAME", "env-ignored")
	t.Setenv(envControllerConfig, "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = cfg.Validate()
	if err == nil {
		t.Fatal("expected Validate to fail with no infra config, got nil")
	}
	if !strings.Contains(err.Error(), "scheduling.nodepoolName") {
		t.Errorf("error should name the file key, got %q", err.Error())
	}
}

// A required field absent from the file fails Validate, naming the file key.
// Covers the CosmosExporterImage fold-in (now required at startup).
func TestLoad_MissingField_FailsValidate(t *testing.T) {
	setGatewayEnv(t)
	body := strings.Replace(fullConfig, "  cosmosExporter: file-cosmos-exporter\n", "", 1)
	path := writeConfig(t, body)
	t.Setenv(envControllerConfig, path)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "images.cosmosExporter") {
		t.Fatalf("want Validate error naming images.cosmosExporter, got %v", err)
	}
}

// A missing gateway env var fails Validate, naming the env var (gateway is still
// env-sourced pending PLT-451).
func TestLoad_MissingGateway_FailsValidate(t *testing.T) {
	path := writeConfig(t, fullConfig)
	t.Setenv(envControllerConfig, path)
	// gateway env deliberately unset

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), envGatewayName) {
		t.Fatalf("want Validate error naming %s, got %v", envGatewayName, err)
	}
}

// Malformed YAML is a hard error — a present-but-broken file must not resolve to
// an empty (and silently invalid) Config.
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
