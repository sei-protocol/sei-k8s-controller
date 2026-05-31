package task

import (
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
)

// ConfigPatchTask stamps controller-owned TOML keys into named seid
// config files via a generic merge-and-write on the sidecar — no
// sei-config involvement. JSON tag matches the wire format
// sidecar.ConfigPatchTask emits.
type ConfigPatchTask struct {
	Files map[string]map[string]any `json:"files"`
}

func (t ConfigPatchTask) TaskType() string { return sidecar.TaskTypeConfigPatch }

func (t ConfigPatchTask) Validate() error {
	return sidecar.ConfigPatchTask{Files: t.Files}.Validate()
}

func (t ConfigPatchTask) ToTaskRequest() sidecar.TaskRequest {
	return sidecar.ConfigPatchTask{Files: t.Files}.ToTaskRequest()
}
