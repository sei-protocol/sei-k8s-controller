package planner

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/google/uuid"
	seiconfig "github.com/sei-protocol/sei-config"
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
	BuildPlan(group *seiv1alpha1.SeiNodeDeployment) (*seiv1alpha1.TaskPlan, error)
}

// ForGroup returns the appropriate GroupPlanner based on the group's
// current state and spec. Returns (nil, nil) when no plan is needed.
func ForGroup(group *seiv1alpha1.SeiNodeDeployment) (GroupPlanner, error) {
	if needsGenesisPlan(group) {
		return &genesisGroupPlanner{}, nil
	}

	// Deployment: reconcileSeiNodes sets Rollout metadata when it
	// detects a spec change requiring deployment orchestration.
	if group.Status.Rollout != nil && group.Status.Plan == nil {
		return ForDeployment(group)
	}

	return nil, nil
}

// needsGenesisPlan returns true when either GenesisCeremonyNeeded or
// ForkGenesisCeremonyNeeded condition is set with sufficient nodes.
func needsGenesisPlan(group *seiv1alpha1.SeiNodeDeployment) bool {
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

func allReplicasCreated(group *seiv1alpha1.SeiNodeDeployment) bool {
	return int32(len(group.Status.IncumbentNodes)) >= group.Spec.Replicas
}

func hasCondition(group *seiv1alpha1.SeiNodeDeployment, condType string) bool {
	for _, c := range group.Status.Conditions {
		if c.Type == condType && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// ResolvePlan ensures node.Status.Plan is set and ready for execution.
// If an active plan exists, it is left in place (resume). Otherwise a
// new plan is built from the node's current phase and spec.
//
// ResolvePlan mutates the node in place: it sets Status.Plan and may
// transition Status.Phase from Pending to Initializing. The caller must
// capture a MergeFrom patch base before calling ResolvePlan, and persist
// the status change if a new plan was built (check planAlreadyActive).
func ResolvePlan(node *seiv1alpha1.SeiNode) error {
	if node.Status.Plan != nil && node.Status.Plan.Phase == seiv1alpha1.TaskPlanActive {
		return nil
	}

	p, err := plannerForMode(node)
	if err != nil {
		return err
	}
	if err := p.Validate(node); err != nil {
		return err
	}

	plan, err := p.BuildPlan(node)
	if err != nil {
		return err
	}
	if plan == nil {
		return nil
	}

	node.Status.Plan = plan
	if node.Status.Phase == "" || node.Status.Phase == seiv1alpha1.PhasePending {
		node.Status.Phase = seiv1alpha1.PhaseInitializing
	}
	return nil
}

// plannerForMode returns the appropriate NodePlanner based on which mode
// sub-spec is populated on the SeiNode.
func plannerForMode(node *seiv1alpha1.SeiNode) (NodePlanner, error) {
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
func buildPlannedTask(planID, taskType string, planIndex int, params any) (seiv1alpha1.PlannedTask, error) {
	id := task.DeterministicTaskID(planID, taskType, planIndex)
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

// buildBasePlan builds a TaskPlan by starting with infrastructure tasks,
// then the base sidecar progression for the node's bootstrap mode.
func buildBasePlan(
	node *seiv1alpha1.SeiNode,
	peers []seiv1alpha1.PeerSource,
	snap *seiv1alpha1.SnapshotSource,
	configApplyParams *task.ConfigApplyParams,
) (*seiv1alpha1.TaskPlan, error) {
	mode := bootstrapMode(snap)
	sidecarProg := slices.Clone(baseProgression[mode])

	sidecarProg = insertBefore(sidecarProg, TaskConfigApply, TaskConfigureGenesis)
	if len(peers) > 0 {
		sidecarProg = insertBefore(sidecarProg, TaskConfigValidate, TaskDiscoverPeers)
	}
	if snap != nil {
		sidecarProg = insertBefore(sidecarProg, TaskConfigValidate, TaskConfigureStateSync)
	}

	// Infrastructure tasks run before sidecar tasks.
	prog := make([]string, 0, 3+len(sidecarProg))
	prog = append(prog, task.TaskTypeEnsureDataPVC, task.TaskTypeApplyStatefulSet, task.TaskTypeApplyService)
	prog = append(prog, sidecarProg...)

	planID := uuid.New().String()
	tasks := make([]seiv1alpha1.PlannedTask, len(prog))
	for i, taskType := range prog {
		t, err := buildPlannedTask(planID, taskType, i, paramsForTaskType(node, taskType, snap, configApplyParams))
		if err != nil {
			return nil, err
		}
		tasks[i] = t
	}
	return &seiv1alpha1.TaskPlan{
		ID:          planID,
		Phase:       seiv1alpha1.TaskPlanActive,
		Tasks:       tasks,
		TargetPhase: seiv1alpha1.PhaseRunning,
		FailedPhase: seiv1alpha1.PhaseFailed,
	}, nil
}

// paramsForTaskType constructs the appropriate params struct for a task type.
// This is the single factory for all task params — every plan builder uses it.
func paramsForTaskType(
	node *seiv1alpha1.SeiNode,
	taskType string,
	snap *seiv1alpha1.SnapshotSource,
	configApplyParams *task.ConfigApplyParams,
) any {
	switch taskType {
	// Infrastructure tasks
	case task.TaskTypeEnsureDataPVC:
		return &task.EnsureDataPVCParams{NodeName: node.Name, Namespace: node.Namespace}
	case task.TaskTypeApplyStatefulSet:
		return &task.ApplyStatefulSetParams{NodeName: node.Name, Namespace: node.Namespace}
	case task.TaskTypeApplyService:
		return &task.ApplyServiceParams{NodeName: node.Name, Namespace: node.Namespace}

	// Sidecar tasks
	case TaskSnapshotRestore:
		return snapshotRestoreParams(snap)
	case TaskConfigureGenesis:
		return &task.ConfigureGenesisParams{}
	case TaskConfigApply:
		if configApplyParams != nil {
			return configApplyParams
		}
		return &task.ConfigApplyParams{}
	case TaskDiscoverPeers:
		return discoverPeersParams(node)
	case TaskConfigureStateSync:
		return configureStateSyncParams(snap)
	case TaskConfigValidate:
		return &task.ConfigValidateParams{}
	case TaskMarkReady:
		return &task.MarkReadyParams{}

	// Genesis ceremony tasks — only valid when Validator.GenesisCeremony is set.
	case TaskGenerateIdentity, TaskGenerateGentx, TaskUploadGenesisArtifacts, TaskSetGenesisPeers:
		return genesisCeremonyTaskParams(node, taskType)

	default:
		return nil
	}
}

func genesisCeremonyTaskParams(node *seiv1alpha1.SeiNode, taskType string) any {
	if node.Spec.Validator == nil || node.Spec.Validator.GenesisCeremony == nil {
		return nil
	}
	gc := node.Spec.Validator.GenesisCeremony
	switch taskType {
	case TaskGenerateIdentity:
		return &task.GenerateIdentityParams{ChainID: gc.ChainID, Moniker: node.Name}
	case TaskGenerateGentx:
		return &task.GenerateGentxParams{
			ChainID:        gc.ChainID,
			StakingAmount:  gc.StakingAmount,
			AccountBalance: gc.AccountBalance,
			GenesisParams:  gc.GenesisParams,
		}
	case TaskUploadGenesisArtifacts:
		return &task.UploadGenesisArtifactsParams{NodeName: node.Name}
	case TaskSetGenesisPeers:
		return &task.SetGenesisPeersParams{}
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

func discoverPeersParams(node *seiv1alpha1.SeiNode) *task.DiscoverPeersParams {
	if len(node.Spec.Peers) == 0 {
		return &task.DiscoverPeersParams{}
	}
	var sources []task.PeerSourceParam
	for _, s := range node.Spec.Peers {
		switch {
		case s.EC2Tags != nil:
			sources = append(sources, task.PeerSourceParam{
				Type:   string(sidecar.PeerSourceEC2Tags),
				Region: s.EC2Tags.Region,
				Tags:   s.EC2Tags.Tags,
			})
		case s.Static != nil:
			sources = append(sources, task.PeerSourceParam{
				Type:      string(sidecar.PeerSourceStatic),
				Addresses: s.Static.Addresses,
			})
		case s.Label != nil:
			sources = append(sources, task.PeerSourceParam{
				Type:      string(sidecar.PeerSourceDNSEndpoints),
				Endpoints: node.Status.ResolvedPeers,
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

// commonOverrides returns controller overrides derived from node status
// that apply to all node modes (e.g., external P2P address from the LB).
func commonOverrides(node *seiv1alpha1.SeiNode) map[string]string {
	if node.Status.ExternalAddress == "" {
		return nil
	}
	return map[string]string{
		seiconfig.KeyP2PExternalAddress: node.Status.ExternalAddress,
	}
}

// buildRunningPlan builds a convergence plan for a Running node.
// It ensures the StatefulSet and Service match the current spec.
//
// FailedPhase is deliberately empty: a convergence failure should not
// transition the node out of Running. The executor still sets a PlanFailed
// condition for observability, and the next reconcile will build a fresh
// convergence plan to retry.
func buildRunningPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
	prog := []string{task.TaskTypeApplyStatefulSet, task.TaskTypeApplyService}

	planID := uuid.New().String()
	tasks := make([]seiv1alpha1.PlannedTask, len(prog))
	for i, taskType := range prog {
		t, err := buildPlannedTask(planID, taskType, i, paramsForTaskType(node, taskType, nil, nil))
		if err != nil {
			return nil, err
		}
		tasks[i] = t
	}
	return &seiv1alpha1.TaskPlan{
		ID:          planID,
		Phase:       seiv1alpha1.TaskPlanActive,
		Tasks:       tasks,
		TargetPhase: seiv1alpha1.PhaseRunning,
	}, nil
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
