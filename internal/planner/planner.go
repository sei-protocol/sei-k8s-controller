package planner

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const (
	TaskSnapshotRestore    = sidecar.TaskTypeSnapshotRestore
	TaskDiscoverPeers      = sidecar.TaskTypeDiscoverPeers
	TaskConfigureGenesis   = sidecar.TaskTypeConfigureGenesis
	TaskConfigureStateSync = sidecar.TaskTypeConfigureStateSync
	TaskConfigApply        = sidecar.TaskTypeConfigApply
	TaskConfigValidate     = sidecar.TaskTypeConfigValidate
	TaskMarkReady          = sidecar.TaskTypeMarkReady
	TaskSnapshotUpload     = sidecar.TaskTypeSnapshotUpload
	TaskResultExport       = sidecar.TaskTypeResultExport
	TaskAwaitCondition     = sidecar.TaskTypeAwaitCondition

	TaskGenerateIdentity       = sidecar.TaskTypeGenerateIdentity
	TaskGenerateGentx          = sidecar.TaskTypeGenerateGentx
	TaskUploadGenesisArtifacts = sidecar.TaskTypeUploadGenesisArtifacts
	TaskAssembleGenesis        = sidecar.TaskTypeAssembleGenesis
	TaskSetGenesisPeers        = sidecar.TaskTypeSetGenesisPeers
	TaskAwaitNodesRunning      = task.TaskTypeAwaitNodesRunning
)

// baseProgression defines the ordered task sequence for each bootstrap mode.
var baseProgression = map[string][]string{
	"snapshot":   {TaskSnapshotRestore, TaskConfigApply, TaskConfigValidate, TaskMarkReady},
	"state-sync": {TaskConfigApply, TaskConfigValidate, TaskMarkReady},
	"genesis":    {TaskConfigApply, TaskConfigValidate, TaskMarkReady},
}

// NodePlanner encapsulates mode-specific logic for validating a SeiNode
// and building its initialization task plan with fully embedded params.
type NodePlanner interface {
	Validate(node *seiv1alpha1.SeiNode) error
	BuildPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error)
	Mode() string
}

// GroupPlanner encapsulates logic for building a group-level task plan.
type GroupPlanner interface {
	BuildPlan(group *seiv1alpha1.SeiNodeGroup) (*seiv1alpha1.TaskPlan, error)
}

// ForGroup returns the appropriate GroupPlanner based on the group's
// current state and spec. Returns (nil, nil) when no plan is needed.
func ForGroup(group *seiv1alpha1.SeiNodeGroup) (GroupPlanner, error) {
	if needsGenesisPlan(group) {
		return &genesisGroupPlanner{}, nil
	}

	// Deployment: reconcileSeiNodes sets Deployment metadata when it
	// detects a spec change requiring deployment orchestration.
	if group.Status.Deployment != nil && group.Status.Plan == nil {
		return ForDeployment(group)
	}

	return nil, nil
}

// needsGenesisPlan returns true when either GenesisCeremonyNeeded or
// ForkGenesisCeremonyNeeded condition is set with sufficient nodes.
func needsGenesisPlan(group *seiv1alpha1.SeiNodeGroup) bool {
	if group.Spec.Genesis == nil {
		return false
	}
	if group.Status.Plan != nil {
		return false
	}
	genesisNeeded := hasCondition(group, seiv1alpha1.ConditionGenesisCeremonyNeeded)
	forkNeeded := hasCondition(group, seiv1alpha1.ConditionForkGenesisCeremonyNeeded)
	if !genesisNeeded && !forkNeeded {
		return false
	}
	return allReplicasCreated(group)
}

func allReplicasCreated(group *seiv1alpha1.SeiNodeGroup) bool {
	return int32(len(group.Status.IncumbentNodes)) >= group.Spec.Replicas
}

func hasCondition(group *seiv1alpha1.SeiNodeGroup, condType string) bool {
	for _, c := range group.Status.Conditions {
		if c.Type == condType && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// ForNode returns the appropriate NodePlanner based on which mode sub-spec
// is populated on the SeiNode.
func ForNode(node *seiv1alpha1.SeiNode) (NodePlanner, error) {
	switch {
	case node.Spec.FullNode != nil:
		return &fullNodePlanner{}, nil
	case node.Spec.Archive != nil:
		return &archiveNodePlanner{}, nil
	case node.Spec.Replayer != nil:
		return &replayerPlanner{}, nil
	case node.Spec.Validator != nil:
		return &validatorPlanner{}, nil
	default:
		return nil, fmt.Errorf("no mode sub-spec set on SeiNode %s/%s", node.Namespace, node.Name)
	}
}

// insertBefore inserts task into prog immediately before target, unless task
// is already present.
func insertBefore(prog []string, target, taskType string) []string {
	if slices.Contains(prog, taskType) {
		return prog
	}
	for i, t := range prog {
		if t == target {
			return slices.Insert(prog, i, taskType)
		}
	}
	return prog
}

// PeersFor extracts the PeerSource list from whichever node mode is set.
func PeersFor(node *seiv1alpha1.SeiNode) []seiv1alpha1.PeerSource {
	switch {
	case node.Spec.FullNode != nil:
		return node.Spec.FullNode.Peers
	case node.Spec.Validator != nil:
		return node.Spec.Validator.Peers
	case node.Spec.Replayer != nil:
		return node.Spec.Replayer.Peers
	case node.Spec.Archive != nil:
		return node.Spec.Archive.Peers
	default:
		return nil
	}
}

// NeedsBootstrap returns true when the node requires a bootstrap Job to
// populate the PVC before the StatefulSet takes over.
func NeedsBootstrap(node *seiv1alpha1.SeiNode) bool {
	snap := node.Spec.SnapshotSource()
	return snap != nil && snap.BootstrapImage != "" &&
		snap.S3 != nil && snap.S3.TargetHeight > 0
}

// isGenesisCeremonyNode returns true when the node participates in a group genesis ceremony.
func isGenesisCeremonyNode(node *seiv1alpha1.SeiNode) bool {
	return node.Spec.Validator != nil && node.Spec.Validator.GenesisCeremony != nil
}

// SnapshotGeneration extracts the SnapshotGenerationConfig from the populated
// mode sub-spec.
func SnapshotGeneration(node *seiv1alpha1.SeiNode) *seiv1alpha1.SnapshotGenerationConfig {
	switch {
	case node.Spec.FullNode != nil:
		return node.Spec.FullNode.SnapshotGeneration
	case node.Spec.Archive != nil:
		return node.Spec.Archive.SnapshotGeneration
	default:
		return nil
	}
}

// NeedsLongStartup returns true when the node's bootstrap strategy involves
// replaying blocks.
func NeedsLongStartup(node *seiv1alpha1.SeiNode) bool {
	switch {
	case node.Spec.FullNode != nil:
		return node.Spec.FullNode.Snapshot != nil
	case node.Spec.Validator != nil:
		return node.Spec.Validator.Snapshot != nil
	case node.Spec.Replayer != nil:
		return true
	case node.Spec.Archive != nil:
		return true
	default:
		return false
	}
}

func hasS3Snapshot(snap *seiv1alpha1.SnapshotSource) bool {
	return snap != nil && snap.S3 != nil
}

func hasStateSync(snap *seiv1alpha1.SnapshotSource) bool {
	return snap != nil && snap.StateSync != nil
}

func bootstrapMode(snap *seiv1alpha1.SnapshotSource) string {
	if hasS3Snapshot(snap) {
		return "snapshot"
	}
	if hasStateSync(snap) {
		return "state-sync"
	}
	return "genesis"
}

// SidecarURLForNode builds the in-cluster sidecar URL for a node's
// StatefulSet pod (used during Initializing and Running phases).
func SidecarURLForNode(node *seiv1alpha1.SeiNode) string {
	return fmt.Sprintf("http://%s-0.%s.%s.svc.cluster.local:%d",
		node.Name, node.Name, node.Namespace, sidecarPortForNode(node))
}

func sidecarPortForNode(node *seiv1alpha1.SeiNode) int32 {
	if node.Spec.Sidecar != nil && node.Spec.Sidecar.Port != 0 {
		return node.Spec.Sidecar.Port
	}
	return sidecar.DefaultPort
}

// marshalParams serializes a task params struct to apiextensionsv1.JSON.
func marshalParams(v any) (*apiextensionsv1.JSON, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshaling %T: %w", v, err)
	}
	return &apiextensionsv1.JSON{Raw: raw}, nil
}

// buildPlannedTask constructs a PlannedTask with deterministic ID and
// serialized params.
func buildPlannedTask(node *seiv1alpha1.SeiNode, taskType string, attempt int, params any) (seiv1alpha1.PlannedTask, error) {
	id := task.DeterministicTaskID(node.Name, taskType, attempt)
	p, err := marshalParams(params)
	if err != nil {
		return seiv1alpha1.PlannedTask{}, fmt.Errorf("task %s: %w", taskType, err)
	}
	return seiv1alpha1.PlannedTask{
		Type:   taskType,
		ID:     id,
		Status: seiv1alpha1.TaskPending,
		Params: p,
	}, nil
}

// buildBasePlan builds a TaskPlan by starting with the base progression for the
// node's bootstrap mode and inserting optional tasks.
func buildBasePlan(
	node *seiv1alpha1.SeiNode,
	peers []seiv1alpha1.PeerSource,
	snap *seiv1alpha1.SnapshotSource,
	configApplyParams *task.ConfigApplyParams,
) (*seiv1alpha1.TaskPlan, error) {
	mode := bootstrapMode(snap)
	prog := slices.Clone(baseProgression[mode])

	prog = insertBefore(prog, TaskConfigApply, TaskConfigureGenesis)
	if len(peers) > 0 {
		prog = insertBefore(prog, TaskConfigValidate, TaskDiscoverPeers)
	}
	if snap != nil {
		prog = insertBefore(prog, TaskConfigValidate, TaskConfigureStateSync)
	}

	attempt := 0
	tasks := make([]seiv1alpha1.PlannedTask, len(prog))
	for i, taskType := range prog {
		t, err := buildPlannedTask(node, taskType, attempt, paramsForTaskType(node, taskType, peers, snap, configApplyParams))
		if err != nil {
			return nil, err
		}
		tasks[i] = t
	}
	return &seiv1alpha1.TaskPlan{
		Phase: seiv1alpha1.TaskPlanActive,
		Tasks: tasks,
	}, nil
}

// paramsForTaskType constructs the appropriate params struct for a task type.
func paramsForTaskType(
	node *seiv1alpha1.SeiNode,
	taskType string,
	peers []seiv1alpha1.PeerSource,
	snap *seiv1alpha1.SnapshotSource,
	configApplyParams *task.ConfigApplyParams,
) any {
	switch taskType {
	case TaskSnapshotRestore:
		return snapshotRestoreParams(snap)
	case TaskConfigureGenesis:
		return configureGenesisParams(node)
	case TaskConfigApply:
		if configApplyParams != nil {
			return configApplyParams
		}
		return &task.ConfigApplyParams{}
	case TaskDiscoverPeers:
		return discoverPeersParams(peers)
	case TaskConfigureStateSync:
		return configureStateSyncParams(snap)
	case TaskConfigValidate:
		return &task.ConfigValidateParams{}
	case TaskMarkReady:
		return &task.MarkReadyParams{}
	default:
		return nil
	}
}

func snapshotRestoreParams(snap *seiv1alpha1.SnapshotSource) *task.SnapshotRestoreParams {
	if snap == nil || snap.S3 == nil {
		return &task.SnapshotRestoreParams{}
	}
	return &task.SnapshotRestoreParams{
		TargetHeight: snap.S3.TargetHeight,
	}
}

func configureGenesisParams(_ *seiv1alpha1.SeiNode) *task.ConfigureGenesisParams {
	return &task.ConfigureGenesisParams{}
}

func discoverPeersParams(peers []seiv1alpha1.PeerSource) *task.DiscoverPeersParams {
	if len(peers) == 0 {
		return &task.DiscoverPeersParams{}
	}
	var sources []task.PeerSourceParam
	for _, s := range peers {
		if s.EC2Tags != nil {
			sources = append(sources, task.PeerSourceParam{
				Type:   string(sidecar.PeerSourceEC2Tags),
				Region: s.EC2Tags.Region,
				Tags:   s.EC2Tags.Tags,
			})
		}
		if s.Static != nil {
			sources = append(sources, task.PeerSourceParam{
				Type:      string(sidecar.PeerSourceStatic),
				Addresses: s.Static.Addresses,
			})
		}
	}
	return &task.DiscoverPeersParams{Sources: sources}
}

func configureStateSyncParams(snap *seiv1alpha1.SnapshotSource) *task.ConfigureStateSyncParams {
	p := &task.ConfigureStateSyncParams{
		UseLocalSnapshot: hasS3Snapshot(snap),
	}
	if snap != nil {
		if snap.TrustPeriod != "" {
			p.TrustPeriod = snap.TrustPeriod
		}
		p.BackfillBlocks = snap.BackfillBlocks
	}
	return p
}

// mergeOverrides combines controller-generated overrides with user-specified
// overrides. User overrides take precedence.
func mergeOverrides(controllerOverrides, userOverrides map[string]string) map[string]string {
	if len(controllerOverrides) == 0 && len(userOverrides) == 0 {
		return nil
	}
	merged := make(map[string]string, len(controllerOverrides)+len(userOverrides))
	maps.Copy(merged, controllerOverrides)
	maps.Copy(merged, userOverrides)
	return merged
}
