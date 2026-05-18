package task

import (
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// AssembleAndUploadGenesisTask satisfies sidecar.TaskBuilder for the
// assemble-and-upload-genesis task. It embeds the upstream
// sidecar.AssembleAndUploadGenesisTask for shared-field parity and adds
// Overrides — a flat map of dotted snake_case key paths to raw JSON values
// merged on top of sei-config's GenesisDefaults() before gentx generation.
//
// Persistence keeps the title-cased shape of the upstream struct (no JSON
// tags on the embedded type); ToTaskRequest emits the wire key "overrides"
// alongside the camelCase fields the upstream type already produces.
//
// The wrapper exists because the upstream sidecar struct does not yet carry
// the Overrides field (companion seictl PR in flight). Once the seictl
// release ships with Overrides on AssembleAndUploadGenesisTask, fold this
// back into the upstream type and delete this wrapper.
type AssembleAndUploadGenesisTask struct {
	sidecar.AssembleAndUploadGenesisTask
	Overrides map[string]apiextensionsv1.JSON `json:"Overrides,omitempty"`
}

func (t AssembleAndUploadGenesisTask) TaskType() string { return sidecar.TaskTypeAssembleGenesis }

// Validate delegates shared-field validation to the upstream type. The sidecar
// fails loud on unknown override keys, so there is no controller-side allowlist.
func (t AssembleAndUploadGenesisTask) Validate() error {
	return t.AssembleAndUploadGenesisTask.Validate()
}

func (t AssembleAndUploadGenesisTask) ToTaskRequest() sidecar.TaskRequest {
	req := t.AssembleAndUploadGenesisTask.ToTaskRequest()
	if len(t.Overrides) == 0 {
		return req
	}
	// Upstream ToTaskRequest always sets Params for assemble-genesis.
	overrides := make(map[string]any, len(t.Overrides))
	for k, v := range t.Overrides {
		overrides[k] = v // apiextensionsv1.JSON marshals to its Raw bytes
	}
	(*req.Params)["overrides"] = overrides
	return req
}
