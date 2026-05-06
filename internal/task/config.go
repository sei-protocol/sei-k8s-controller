package task

import (
	seiconfig "github.com/sei-protocol/sei-config"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
)

// configApplyTask satisfies sidecar.TaskBuilder for the config-apply task.
// It anonymously embeds seiconfig.ConfigIntent so on-disk PlannedTask.Params.Raw
// stays as a flat camelCase shape (matching ConfigIntent's json tags),
// while wire format and validation delegate to sidecar.ConfigApplyTask —
// the single source of truth in seictl. Unexported because it has no
// callers outside the deserialize registry.
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
