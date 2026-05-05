package bootstrap

import "github.com/sei-protocol/sei-k8s-controller/internal/task"

// Task types registered by this package. Wire-format strings are unchanged
// from when these constants lived in internal/task/task.go.
const (
	TaskTypeService  = "deploy-bootstrap-service"
	TaskTypeJob      = "deploy-bootstrap-job"
	TaskTypeAwait    = "await-bootstrap-complete"
	TaskTypeTeardown = "teardown-bootstrap"
)

func init() {
	task.Register(TaskTypeService, deserializeBootstrapService)
	task.Register(TaskTypeJob, deserializeBootstrapJob)
	task.Register(TaskTypeAwait, deserializeBootstrapAwait)
	task.Register(TaskTypeTeardown, deserializeBootstrapTeardown)
}
