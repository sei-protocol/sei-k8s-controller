package task

// Controller-managed deployment task types.
const (
	TaskTypeCreateGreenNodes   = "create-green-nodes"
	TaskTypeAwaitGreenRunning  = "await-green-running"
	TaskTypeSubmitHaltSignal   = "submit-halt-signal-blue"
	TaskTypeAwaitGreenAtHeight = "await-green-at-height"
	TaskTypeAwaitGreenCaughtUp = "await-green-caught-up"
	TaskTypeSwitchTraffic      = "switch-traffic"
	TaskTypeTeardownBlue       = "teardown-blue"
)

// CreateGreenNodesParams holds parameters for creating green SeiNode resources.
type CreateGreenNodesParams struct {
	GroupName        string   `json:"groupName"`
	Namespace        string   `json:"namespace"`
	IncomingRevision string   `json:"incomingRevision"`
	NodeNames        []string `json:"nodeNames"`
}

// AwaitGreenRunningParams holds parameters for waiting until green nodes reach Running.
type AwaitGreenRunningParams struct {
	Namespace string   `json:"namespace"`
	NodeNames []string `json:"nodeNames"`
}

// SubmitHaltSignalParams holds parameters for submitting await-condition(SIGTERM)
// to blue node sidecars.
type SubmitHaltSignalParams struct {
	Namespace  string   `json:"namespace"`
	NodeNames  []string `json:"nodeNames"`
	HaltHeight int64    `json:"haltHeight"`
}

// AwaitGreenAtHeightParams holds parameters for waiting until green nodes
// reach a specific block height.
type AwaitGreenAtHeightParams struct {
	Namespace  string   `json:"namespace"`
	NodeNames  []string `json:"nodeNames"`
	HaltHeight int64    `json:"haltHeight"`
}

// AwaitGreenCaughtUpParams holds parameters for waiting until green nodes
// report catching_up == false (fully synced to chain tip).
type AwaitGreenCaughtUpParams struct {
	Namespace string   `json:"namespace"`
	NodeNames []string `json:"nodeNames"`
}

// SwitchTrafficParams holds parameters for switching the Service selector
// from the active revision to the incoming revision.
type SwitchTrafficParams struct {
	GroupName        string `json:"groupName"`
	Namespace        string `json:"namespace"`
	IncomingRevision string `json:"incomingRevision"`
}

// TeardownBlueParams holds parameters for deleting old (blue) SeiNode resources.
type TeardownBlueParams struct {
	Namespace string   `json:"namespace"`
	NodeNames []string `json:"nodeNames"`
}
