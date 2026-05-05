package export

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest"
)

const (
	testGroupRoot     = "fork-group-exporter"
	testNamespace     = "default"
	testChainID       = "atlantic-1"
	testSeidImage     = "sei:source-v5.0.0"
	testSidecarImage  = "ghcr.io/sei/sidecar:test"
	testPVCClaim      = "fork-group-exporter-data"
	testGenesisBucket = "sei-genesis"
	testGenesisRegion = "eu-central-1"
	testGenesisKey    = "atlantic-1/exported-state.json"
)

func validInputs() PodInputs {
	return PodInputs{
		Name:          testGroupRoot,
		Namespace:     testNamespace,
		ChainID:       testChainID,
		SeidImage:     testSeidImage,
		SidecarImage:  testSidecarImage,
		SidecarPort:   7777,
		Mode:          "full",
		PVCClaimName:  testPVCClaim,
		ExportHeight:  100000,
		GenesisBucket: testGenesisBucket,
		GenesisRegion: testGenesisRegion,
		GenesisKey:    testGenesisKey,
	}
}

func TestGenerateJob_HappyPath(t *testing.T) {
	job, err := GenerateJob(validInputs(), platformtest.Config())
	if err != nil {
		t.Fatalf("GenerateJob: %v", err)
	}
	if job.Name != "fork-group-exporter-export" {
		t.Errorf("Job name = %q, want fork-group-exporter-export", job.Name)
	}
	spec := job.Spec.Template.Spec
	if spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q, want Never", spec.RestartPolicy)
	}
	if spec.TerminationGracePeriodSeconds == nil || *spec.TerminationGracePeriodSeconds != 600 {
		t.Errorf("TerminationGracePeriodSeconds = %v, want 600", spec.TerminationGracePeriodSeconds)
	}
	if len(spec.InitContainers) != 2 {
		t.Fatalf("expected 2 initContainers (seid-init + sei-sidecar), got %d", len(spec.InitContainers))
	}
	if spec.InitContainers[0].Name != "seid-init" {
		t.Errorf("initContainer[0] = %q, want seid-init", spec.InitContainers[0].Name)
	}
	if spec.InitContainers[1].Name != "sei-sidecar" {
		t.Errorf("initContainer[1] = %q, want sei-sidecar", spec.InitContainers[1].Name)
	}
	if spec.InitContainers[1].RestartPolicy == nil || *spec.InitContainers[1].RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Errorf("sei-sidecar restartPolicy = %v, want Always", spec.InitContainers[1].RestartPolicy)
	}
	if len(spec.Containers) != 1 || spec.Containers[0].Name != "seid" {
		t.Fatalf("expected single seid main container, got %v", spec.Containers)
	}
	if len(spec.Containers[0].Args) != 1 || spec.Containers[0].Args[0] != exportTriggerScript {
		t.Errorf("seid Args[0] != exportTriggerScript const")
	}
	// Sidecar env must omit SEI_SNAPSHOT_*. Only SEI_GENESIS_* is in scope.
	for _, e := range spec.InitContainers[1].Env {
		if strings.HasPrefix(e.Name, "SEI_SNAPSHOT_") {
			t.Errorf("sidecar env %q leaked snapshot config to export Job", e.Name)
		}
	}
}

func TestGenerateJob_Validation(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*PodInputs)
		errSubstr string
	}{
		{"empty Name", func(in *PodInputs) { in.Name = "" }, "Name and Namespace"},
		{"empty Namespace", func(in *PodInputs) { in.Namespace = "" }, "Name and Namespace"},
		{"empty ChainID", func(in *PodInputs) { in.ChainID = "" }, "ChainID"},
		{"empty SeidImage", func(in *PodInputs) { in.SeidImage = "" }, "SeidImage"},
		{"empty PVCClaimName", func(in *PodInputs) { in.PVCClaimName = "" }, "PVCClaimName"},
		{"zero ExportHeight", func(in *PodInputs) { in.ExportHeight = 0 }, "ExportHeight"},
		{"negative ExportHeight", func(in *PodInputs) { in.ExportHeight = -1 }, "ExportHeight"},
		{"empty GenesisBucket", func(in *PodInputs) { in.GenesisBucket = "" }, "GenesisBucket"},
		{"empty GenesisRegion", func(in *PodInputs) { in.GenesisRegion = "" }, "GenesisRegion"},
		{"empty GenesisKey", func(in *PodInputs) { in.GenesisKey = "" }, "GenesisKey"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validInputs()
			tt.mutate(&in)
			_, err := GenerateJob(in, platformtest.Config())
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.errSubstr) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.errSubstr)
			}
		})
	}
}

// TestExportTriggerScript_PinsContract pins the bash const's wire shape: the
// JSON keys POSTed (file/bucket/key/region) must match seictl's
// UploadFileRequest tags, and the case-arm strings must match
// engine.TaskStatusCompleted / TaskStatusFailed. A drift in either direction
// silently breaks the seid container's poll loop in production — this test
// catches it at controller-PR-review time.
func TestExportTriggerScript_PinsContract(t *testing.T) {
	wantSubstrings := []string{
		// JSON keys must match the upload-file task's UploadFileRequest tags.
		`"file":"/sei/tmp/exported-state.json"`,
		`"bucket":"%s"`,
		`"key":"%s"`,
		`"region":"%s"`,
		// Sidecar URL path matches the registered task type.
		"/v0/tasks/upload-file",
		// Status case-arms match engine.TaskStatusCompleted / TaskStatusFailed.
		"completed) exit 0",
		"failed)",
		// Wall-clock timeout on the healthz wait so /dev/tcp failure can't hang.
		"SECONDS",
		// Healthz endpoint we wait on.
		"/v0/healthz",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(exportTriggerScript, want) {
			t.Errorf("exportTriggerScript missing %q", want)
		}
	}
}
