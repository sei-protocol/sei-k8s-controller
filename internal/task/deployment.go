package task

// Controller-managed deployment task types.
const (
	TaskTypeUpdateNodeSpecs = "update-node-specs"
	TaskTypeAwaitSpecUpdate = "await-spec-update"
)

// UpdateNodeSpecsParams holds parameters for patching child SeiNode specs
// during an InPlace deployment.
type UpdateNodeSpecsParams struct {
	GroupName string   `json:"groupName"`
	Namespace string   `json:"namespace"`
	NodeNames []string `json:"nodeNames"`
}

// AwaitSpecUpdateParams holds parameters for waiting until all nodes
// have converged to the desired image (status.currentImage == spec.image).
type AwaitSpecUpdateParams struct {
	Namespace string   `json:"namespace"`
	NodeNames []string `json:"nodeNames"`
}
