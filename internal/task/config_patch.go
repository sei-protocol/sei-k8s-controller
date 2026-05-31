package task

import (
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
)

// ConfigPatchTask satisfies sidecar.TaskBuilder for config-patch. The
// controller stamps a subset of TOML keys it directly owns into named
// files; the sidecar handler is a generic merge-and-write per file with
// no sei-config involvement. Used for day-2 reconvergence where the
// controller wants surgical authority over operational fields without
// re-running the typed mode-defaulted resolver.
//
// JSON tag matches the wire format sidecar.ConfigPatchTask serializes
// (sidecar handler expects {"files": {...}}).
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
