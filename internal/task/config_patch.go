package task

import (
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
)

// ConfigPatchTask stamps controller-owned TOML keys into named seid
// config files via the sidecar's generic merge-and-write. The json tag
// matches the wire format the sidecar deserializes.
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
