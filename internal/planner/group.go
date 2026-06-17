package planner

import (
	"encoding/json"

	"github.com/google/uuid"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const groupAssemblyMaxRetries = 180

type genesisGroupPlanner struct{}

// BuildPlan constructs a TaskPlan for the SeiNetwork that:
//  1. Assembles all per-node genesis artifacts into a final genesis.json
//     (retried until the sidecar succeeds).
//  2. Collects node IDs and sets persistent_peers on each child node.
//  3. Waits for all child SeiNodes to reach PhaseRunning.
func (p *genesisGroupPlanner) BuildPlan(
	network *seiv1alpha1.SeiNetwork,
) (*seiv1alpha1.TaskPlan, error) {
	planID := uuid.New().String()
	planIndex := 0
	incumbentNodes := network.Status.IncumbentNodes

	nodeParams := make([]sidecar.GenesisNodeParam, len(incumbentNodes))
	for i, name := range incumbentNodes {
		nodeParams[i] = sidecar.GenesisNodeParam{Name: name}
	}

	accounts := make([]sidecar.GenesisAccountEntry, len(network.Spec.Genesis.Accounts))
	for i, a := range network.Spec.Genesis.Accounts {
		accounts[i] = sidecar.GenesisAccountEntry{Address: a.Address, Balance: a.Balance}
	}

	// Validate at planner-time so bech32 / shape errors hit
	// `kubectl describe seinetwork` rather than a sidecar Job pod.
	assembleParams := sidecar.AssembleAndUploadGenesisTask{
		AccountBalance: network.Spec.Genesis.AccountBalance,
		Namespace:      network.Namespace,
		Nodes:          nodeParams,
		Accounts:       accounts,
		Overrides:      toRawMessages(network.Spec.Genesis.Overrides),
	}
	if err := assembleParams.Validate(); err != nil {
		return nil, err
	}

	assembleTask, err := buildPlannedTask(planID, TaskAssembleGenesis, planIndex, assembleParams)
	if err != nil {
		return nil, err
	}
	planIndex++

	collectPeersTask, err := buildPlannedTask(planID, task.TaskTypeCollectAndSetPeers, planIndex,
		&task.CollectAndSetPeersParams{
			GroupName: network.Name,
			Namespace: network.Namespace,
			NodeNames: incumbentNodes,
		})
	if err != nil {
		return nil, err
	}
	planIndex++

	awaitTask, err := buildPlannedTask(planID, TaskAwaitNodesRunning, planIndex,
		&task.AwaitNodesRunningParams{
			GroupName: network.Name,
			Namespace: network.Namespace,
			Expected:  len(incumbentNodes),
			NodeNames: incumbentNodes,
		})
	if err != nil {
		return nil, err
	}

	return &seiv1alpha1.TaskPlan{
		ID:    planID,
		Phase: seiv1alpha1.TaskPlanActive,
		Tasks: []seiv1alpha1.PlannedTask{assembleTask, collectPeersTask, awaitTask},
	}, nil
}

// toRawMessages converts the CRD's apiextensionsv1.JSON values (required by
// OpenAPI/CEL on the API server side) into encoding/json.RawMessage values
// (the wire shape the seictl sidecar consumes). Both store the same raw JSON
// bytes; only the Go type differs.
func toRawMessages(in map[string]apiextensionsv1.JSON) map[string]json.RawMessage {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]json.RawMessage, len(in))
	for k, v := range in {
		out[k] = json.RawMessage(v.Raw)
	}
	return out
}
