package task

import (
	seiconfig "github.com/sei-protocol/sei-config"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
)

// configApplyTask satisfies sidecar.TaskBuilder for config-apply. The
// embedded seiconfig.ConfigIntent flattens at marshal time, keeping
// PlannedTask.Params.Raw on the same flat shape ConfigIntent's json tags
// produce. Wire format and validation delegate to sidecar.ConfigApplyTask —
// the seictl wrapper wraps the same fields in a nested Intent struct,
// which would otherwise change the on-disk shape.
type configApplyTask struct {
	seiconfig.ConfigIntent
}

func (t configApplyTask) TaskType() string { return sidecar.TaskTypeConfigApply }

func (t configApplyTask) Validate() error {
	return sidecar.ConfigApplyTask{Intent: t.ConfigIntent}.Validate()
}

func (t configApplyTask) ToTaskRequest() sidecar.TaskRequest {
	return sidecar.ConfigApplyTask{Intent: t.ConfigIntent}.ToTaskRequest()
}
