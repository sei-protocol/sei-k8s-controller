package export

import "github.com/sei-protocol/sei-k8s-controller/internal/task"

// Task type constants registered by this package. Wire-format strings
// describe the SND-driven fork-genesis sub-plan introduced when the broken
// `submit-export-state` cross-container exec was replaced.
const (
	TaskTypeEnsurePVC         = "ensure-exporter-pvc"
	TaskTypeApplyBootstrapJob = "apply-bootstrap-job"
	TaskTypeAwaitBootstrapJob = "await-bootstrap-job"
	TaskTypeApplyExportJob    = "apply-export-job"
	TaskTypeAwaitExportJob    = "await-export-job"
	TaskTypeTeardownExporter  = "teardown-exporter"
)

func init() {
	task.Register(TaskTypeEnsurePVC, deserializeEnsureExporterPVC)
	task.Register(TaskTypeApplyBootstrapJob, deserializeApplyBootstrapJob)
	task.Register(TaskTypeAwaitBootstrapJob, deserializeAwaitJob)
	task.Register(TaskTypeApplyExportJob, deserializeApplyExportJob)
	task.Register(TaskTypeAwaitExportJob, deserializeAwaitJob)
	task.Register(TaskTypeTeardownExporter, deserializeTeardownExporter)
}
