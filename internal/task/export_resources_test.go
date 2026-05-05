package task

import (
	"strings"
	"testing"

	"github.com/sei-protocol/seictl/sidecar/engine"
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

	// errNameNamespace is the validation-error substring shared by the empty-Name
	// and empty-Namespace branches of GenerateExportJob.
	errNameNamespace = "Name and Namespace"
)

func validInputs() ExportJobInputs {
	return ExportJobInputs{
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
	job, err := GenerateExportJob(validInputs(), platformtest.Config())
	if err != nil {
		t.Fatalf("GenerateExportJob: %v", err)
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
	if spec.InitContainers[0].Name != seidInitContainerName {
		t.Errorf("initContainer[0] = %q, want %q", spec.InitContainers[0].Name, seidInitContainerName)
	}
	if spec.InitContainers[1].Name != "sei-sidecar" {
		t.Errorf("initContainer[1] = %q, want sei-sidecar", spec.InitContainers[1].Name)
	}
	if spec.InitContainers[1].RestartPolicy == nil || *spec.InitContainers[1].RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Errorf("sei-sidecar restartPolicy = %v, want Always", spec.InitContainers[1].RestartPolicy)
	}
	if len(spec.Containers) != 1 || spec.Containers[0].Name != seidContainerName {
		t.Fatalf("expected single %q main container, got %v", seidContainerName, spec.Containers)
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
		mutate    func(*ExportJobInputs)
		errSubstr string
	}{
		{"empty Name", func(in *ExportJobInputs) { in.Name = "" }, errNameNamespace},
		{"empty Namespace", func(in *ExportJobInputs) { in.Namespace = "" }, errNameNamespace},
		{"empty ChainID", func(in *ExportJobInputs) { in.ChainID = "" }, "ChainID"},
		{"empty SeidImage", func(in *ExportJobInputs) { in.SeidImage = "" }, "SeidImage"},
		{"empty PVCClaimName", func(in *ExportJobInputs) { in.PVCClaimName = "" }, "PVCClaimName"},
		{"zero ExportHeight", func(in *ExportJobInputs) { in.ExportHeight = 0 }, "ExportHeight"},
		{"negative ExportHeight", func(in *ExportJobInputs) { in.ExportHeight = -1 }, "ExportHeight"},
		{"empty GenesisBucket", func(in *ExportJobInputs) { in.GenesisBucket = "" }, "GenesisBucket"},
		{"empty GenesisRegion", func(in *ExportJobInputs) { in.GenesisRegion = "" }, "GenesisRegion"},
		{"empty GenesisKey", func(in *ExportJobInputs) { in.GenesisKey = "" }, "GenesisKey"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validInputs()
			tt.mutate(&in)
			_, err := GenerateExportJob(in, platformtest.Config())
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.errSubstr) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.errSubstr)
			}
		})
	}
}

// TestExportTriggerScript_PinsContract pins the bash const's wire shape
// against the actual sidecar HTTP surface (single POST /v0/tasks endpoint
// with {type,params} envelope) and the engine status string constants. A
// drift in either direction silently breaks the seid container's poll loop
// in production — this test catches it at controller-PR-review time.
func TestExportTriggerScript_PinsContract(t *testing.T) {
	wantSubstrings := []string{
		// POST envelope shape: type + params, not a flat /v0/tasks/<type>.
		`"type":"upload-file"`,
		`"params":{`,
		// JSON keys inside params must match the upload-file task's
		// UploadFileRequest decoder.
		`"file":"/sei/tmp/exported-state.json"`,
		`"bucket":"%s"`,
		`"key":"%s"`,
		`"region":"%s"`,
		// Sidecar route paths the bash actually hits.
		"/v0/tasks ",   // POST endpoint (trailing space delimits path in printf)
		"/v0/tasks/%s", // GET-by-id (the format string for polling)
		"/v0/healthz",  // health gate
		// Wall-clock timeout on the healthz wait so /dev/tcp failure can't hang.
		"SECONDS",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(exportTriggerScript, want) {
			t.Errorf("exportTriggerScript missing %q", want)
		}
	}
}

// TestExportTriggerScript_PinsEngineStatusStrings asserts the bash case-arms
// match the actual engine.TaskStatusCompleted / TaskStatusFailed string
// values. Renaming those constants would silently break the seid container's
// poll loop; this test fails fast at controller-PR-review time.
func TestExportTriggerScript_PinsEngineStatusStrings(t *testing.T) {
	if string(engine.TaskStatusCompleted) != "completed" {
		t.Fatalf("engine.TaskStatusCompleted = %q, but bash matches \"completed\"", engine.TaskStatusCompleted)
	}
	if string(engine.TaskStatusFailed) != "failed" {
		t.Fatalf("engine.TaskStatusFailed = %q, but bash matches \"failed\"", engine.TaskStatusFailed)
	}
	if !strings.Contains(exportTriggerScript, "completed) exit 0") {
		t.Errorf("exportTriggerScript missing 'completed) exit 0' arm")
	}
	if !strings.Contains(exportTriggerScript, "failed)") {
		t.Errorf("exportTriggerScript missing 'failed)' arm")
	}
}
