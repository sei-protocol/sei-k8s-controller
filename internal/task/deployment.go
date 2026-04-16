package task

import "github.com/google/uuid"

// Controller-managed deployment task types.
const (
	TaskTypeCreateEntrantNodes = "create-entrant-nodes"
	TaskTypeUpdateNodeSpecs    = "update-node-specs"
	TaskTypeAwaitSpecUpdate    = "await-spec-update"
	TaskTypeSubmitHaltSignal   = "submit-halt-signal"
	TaskTypeAwaitNodesAtHeight = "await-nodes-at-height"
	TaskTypeAwaitNodesCaughtUp = "await-nodes-caught-up"
	TaskTypeSwitchTraffic      = "switch-traffic"
	TaskTypeTeardownNodes      = "teardown-nodes"
)

// deploymentTaskNamespace is a fixed UUID v5 namespace for generating
// deterministic task IDs scoped to deployment operations. Distinct from
// taskIDNamespace to prevent cross-domain collisions.
var deploymentTaskNamespace = uuid.MustParse("d4a7e1f3-8b29-4c6d-a5e0-2f1d3c4b5a6e")

// CreateEntrantNodesParams holds parameters for creating entrant SeiNode resources.
type CreateEntrantNodesParams struct {
	GroupName       string   `json:"groupName"`
	Namespace       string   `json:"namespace"`
	EntrantRevision string   `json:"entrantRevision"`
	NodeNames       []string `json:"nodeNames"`
}

// AwaitNodesAtHeightParams holds parameters for waiting until nodes
// reach a specific block height.
type AwaitNodesAtHeightParams struct {
	Namespace    string   `json:"namespace"`
	NodeNames    []string `json:"nodeNames"`
	TargetHeight int64    `json:"targetHeight"`
}

// SubmitHaltSignalParams holds parameters for submitting
// await-condition(SIGTERM) to incumbent node sidecars.
type SubmitHaltSignalParams struct {
	Namespace  string   `json:"namespace"`
	NodeNames  []string `json:"nodeNames"`
	HaltHeight int64    `json:"haltHeight"`
}

// AwaitNodesCaughtUpParams holds parameters for waiting until nodes
// report catching_up == false (fully synced to chain tip).
type AwaitNodesCaughtUpParams struct {
	Namespace string   `json:"namespace"`
	NodeNames []string `json:"nodeNames"`
}

// SwitchTrafficParams holds parameters for switching the Service selector
// to the entrant revision.
type SwitchTrafficParams struct {
	GroupName       string `json:"groupName"`
	Namespace       string `json:"namespace"`
	EntrantRevision string `json:"entrantRevision"`
}

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

// TeardownNodesParams holds parameters for deleting incumbent SeiNode resources.
type TeardownNodesParams struct {
	Namespace string   `json:"namespace"`
	NodeNames []string `json:"nodeNames"`
}
