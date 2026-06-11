package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	seiconfig "github.com/sei-protocol/sei-config"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	"go.opentelemetry.io/otel/metric"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ctrl "sigs.k8s.io/controller-runtime"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/observability"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const unknownValue = "unknown"

const (
	TaskSnapshotRestore    = sidecar.TaskTypeSnapshotRestore
	TaskDiscoverPeers      = sidecar.TaskTypeDiscoverPeers
	TaskConfigureGenesis   = sidecar.TaskTypeConfigureGenesis
	TaskConfigureStateSync = sidecar.TaskTypeConfigureStateSync
	TaskConfigApply        = sidecar.TaskTypeConfigApply
	TaskConfigPatch        = sidecar.TaskTypeConfigPatch
	TaskConfigValidate     = sidecar.TaskTypeConfigValidate
	TaskMarkReady          = sidecar.TaskTypeMarkReady
	TaskSnapshotUpload     = sidecar.TaskTypeSnapshotUpload
	TaskAwaitCondition     = sidecar.TaskTypeAwaitCondition

	TaskGenerateIdentity       = sidecar.TaskTypeGenerateIdentity
	TaskGenerateGentx          = sidecar.TaskTypeGenerateGentx
	TaskUploadGenesisArtifacts = sidecar.TaskTypeUploadGenesisArtifacts
	TaskAssembleGenesis        = sidecar.TaskTypeAssembleGenesis
	TaskSetGenesisPeers        = sidecar.TaskTypeSetGenesisPeers
	TaskAwaitNodesRunning      = task.TaskTypeAwaitNodesRunning
)

const (
	overrideKeyLoggingLevel = "logging.level"
	enforcedLoggingLevel    = "info"

	// keyP2PPersistentPeers is the sei-config enrichment key for
	// network.p2p.persistent_peers (WithHotReload). sei-config v0.0.19
	// exposes no exported constant for it, so it is named here.
	keyP2PPersistentPeers = "network.p2p.persistent_peers"
)

// baseProgression defines the ordered task sequence for each bootstrap mode.
var baseProgression = map[string][]string{
	"snapshot":   {TaskSnapshotRestore, TaskConfigApply, TaskConfigValidate, TaskMarkReady},
	"state-sync": {TaskConfigApply, TaskConfigValidate, TaskMarkReady},
	"genesis":    {TaskConfigApply, TaskConfigValidate, TaskMarkReady},
}

// NodePlanner encapsulates mode-specific plan construction. BuildPlan
// dispatches internally between startup (init) and an existing
// running resource — callers don't see the distinction.
//
// Convention: init writes the whole config via TaskConfigApply; an
// existing resource patches only controller-owned keys via TaskConfigPatch.
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

// needsGenesisPlan returns true when a genesis ceremony has not yet
// completed for an SND that requires one. Reads
// ConditionGenesisCeremonyComplete: any value other than True/Complete
// means "ceremony still needs to run."
func needsGenesisPlan(group *seiv1alpha1.SeiNodeDeployment) bool {
	if group.Spec.Genesis == nil {
		return false
	}
	if group.Status.Plan != nil {
		return false
	}
	if isConditionTrue(group, seiv1alpha1.ConditionGenesisCeremonyComplete) {
		return false
	}
	return allReplicasCreated(group)
}

func allReplicasCreated(group *seiv1alpha1.SeiNodeDeployment) bool {
	return int32(len(group.Status.IncumbentNodes)) >= group.Spec.Replicas
}

// isConditionTrue returns whether the named condition is present with
// Status=True. The name distinguishes this from a presence check —
// always-present conditions (per CLAUDE.md `### Conditions`) require
// callers to assert on Status, not on presence.
func isConditionTrue(group *seiv1alpha1.SeiNodeDeployment, condType string) bool {
	for _, c := range group.Status.Conditions {
		if c.Type == condType && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

type NodeResolver struct {
	// Nil factory skips the sidecar probe; used by tests.
	BuildSidecarClient func(node *seiv1alpha1.SeiNode) (task.SidecarClient, error)
	// Platform supplies the controller-wide sidecar image fallback used
	// by sidecarImageDrifted when Spec.Sidecar.Image is unset.
	Platform platform.Config
}

func (p *NodeResolver) ResolvePlan(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	// Skip the probe during Initializing — the init plan owns the sidecar there.
	if p.BuildSidecarClient != nil && node.Status.Phase == seiv1alpha1.PhaseRunning {
		client, err := p.BuildSidecarClient(node)
		if err == nil {
			probeSidecarHealth(ctx, node, client)
		}
	}

	if node.Status.Plan != nil && node.Status.Plan.Phase == seiv1alpha1.TaskPlanActive {
		return nil
	}

	handleTerminalPlan(ctx, node)

	mode, err := p.plannerForMode(node)
	if err != nil {
		return err
	}
	if err := mode.Validate(node); err != nil {
		return err
	}

	plan, err := mode.BuildPlan(node)
	if err != nil {
		return err
	}
	if plan == nil {
		return nil
	}

	node.Status.Plan = plan
	if node.Status.Phase == "" || node.Status.Phase == seiv1alpha1.PhasePending {
		node.Status.Phase = seiv1alpha1.PhaseInitializing
		now := metav1.Now()
		node.Status.PhaseTransitionTime = &now
	}
	return nil
}

// handleTerminalPlan handles completed or failed plans: clears conditions
// and nils the plan so the planner can build the next one if needed.
func handleTerminalPlan(ctx context.Context, node *seiv1alpha1.SeiNode) {
	plan := node.Status.Plan
	if plan == nil {
		return
	}

	cn := "seinode"
	planType := classifyPlan(plan)

	switch plan.Phase {
	case seiv1alpha1.TaskPlanComplete:
		if hasNodeUpdateCondition(node) {
			setNodeUpdateCondition(node, metav1.ConditionFalse, "UpdateComplete",
				fmt.Sprintf("plan %s completed", plan.ID))
		}
		emitPlanDuration(ctx, cn, node.Namespace, planType, "complete", plan)
		node.Status.Plan = nil

	case seiv1alpha1.TaskPlanFailed:
		if hasNodeUpdateCondition(node) {
			setNodeUpdateCondition(node, metav1.ConditionFalse, "UpdateFailed",
				fmt.Sprintf("plan %s failed: %s", plan.ID, planFailureMessage(plan)))
		}
		emitPlanDuration(ctx, cn, node.Namespace, planType, "failed", plan)
		node.Status.Plan = nil
	}
}

// hasNodeUpdateCondition returns true if NodeUpdateInProgress is currently True.
func hasNodeUpdateCondition(node *seiv1alpha1.SeiNode) bool {
	cond := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
	return cond != nil && cond.Status == metav1.ConditionTrue
}

// setNodeUpdateCondition sets or updates the NodeUpdateInProgress condition.
func setNodeUpdateCondition(node *seiv1alpha1.SeiNode, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               seiv1alpha1.ConditionNodeUpdateInProgress,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: node.Generation,
	})
}

// classifyPlan returns the plan type for metrics.
func classifyPlan(plan *seiv1alpha1.TaskPlan) string {
	for _, t := range plan.Tasks {
		switch t.Type {
		case task.TaskTypeObserveImage:
			return "node-update"
		case task.TaskTypeEnsureDataPVC:
			return "init"
		}
	}
	if len(plan.Tasks) == 1 && plan.Tasks[0].Type == sidecar.TaskTypeMarkReady {
		return "mark-ready-reapply"
	}
	return unknownValue
}

// emitPlanDuration records the wall-clock time from the first task's
// submission to now. This approximates plan duration.
func emitPlanDuration(ctx context.Context, controller, namespace, planType, outcome string, plan *seiv1alpha1.TaskPlan) {
	if len(plan.Tasks) == 0 {
		return
	}
	first := plan.Tasks[0]
	if first.SubmittedAt == nil {
		return
	}
	dur := time.Since(first.SubmittedAt.Time).Seconds()
	planDuration.Record(ctx, dur,
		metric.WithAttributes(
			observability.AttrController.String(controller),
			observability.AttrNamespace.String(namespace),
			observability.AttrPlanType.String(planType),
			observability.AttrOutcome.String(outcome),
		),
	)
}

func planFailureMessage(plan *seiv1alpha1.TaskPlan) string {
	if plan.FailedTaskDetail != nil {
		return fmt.Sprintf("task %s: %s", plan.FailedTaskDetail.Type, plan.FailedTaskDetail.Error)
	}
	return unknownValue
}

// plannerForMode returns the appropriate NodePlanner for the SeiNode's
// mode sub-spec, threaded with the resolver's Platform config so each
// planner can resolve the effective sidecar image for drift detection.
func (r *NodeResolver) plannerForMode(node *seiv1alpha1.SeiNode) (NodePlanner, error) {
	switch {
	case node.Spec.FullNode != nil:
		return &fullNodePlanner{platform: r.Platform}, nil
	case node.Spec.Archive != nil:
		return &archiveNodePlanner{platform: r.Platform}, nil
	case node.Spec.Replayer != nil:
		return &replayerPlanner{platform: r.Platform}, nil
	case node.Spec.Validator != nil:
		return &validatorPlanner{platform: r.Platform}, nil
	default:
		return nil, fmt.Errorf("no mode sub-spec set on SeiNode %s/%s", node.Namespace, node.Name)
	}
}

// insertBefore inserts taskType into prog immediately before target.
// Returns an error if the target is not found — this catches plan
// construction bugs rather than producing silently incomplete plans.
// No-op if taskType is already present.
func insertBefore(prog []string, target, taskType string) ([]string, error) {
	if slices.Contains(prog, taskType) {
		return prog, nil
	}
	for i, t := range prog {
		if t == target {
			return slices.Insert(prog, i, taskType), nil
		}
	}
	return nil, fmt.Errorf("insertBefore: target %q not found in progression %v", target, prog)
}

// buildSidecarProgression constructs the sidecar task sequence for the given
// bootstrap mode, inserting optional tasks (genesis, state-sync) at the
// correct positions. Used by both buildBasePlan and buildBootstrapPlan to
// ensure they produce consistent sidecar progressions. persistent_peers is no
// longer a sidecar task — the controller writes it via the config-apply
// override (see commonOverrides).
func buildSidecarProgression(snap *seiv1alpha1.SnapshotSource) ([]string, error) {
	mode := bootstrapMode(snap)
	prog := slices.Clone(baseProgression[mode])

	var err error
	if prog, err = insertBefore(prog, TaskConfigApply, TaskConfigureGenesis); err != nil {
		return nil, err
	}
	if snap != nil {
		if prog, err = insertBefore(prog, TaskConfigValidate, TaskConfigureStateSync); err != nil {
			return nil, err
		}
	}
	return prog, nil
}

// NeedsBootstrap returns true when the node requires a bootstrap Job to
// populate the PVC before the StatefulSet takes over.
func NeedsBootstrap(node *seiv1alpha1.SeiNode) bool {
	snap := node.Spec.SnapshotSource()
	return snap != nil && snap.BootstrapImage != "" &&
		snap.S3 != nil && snap.S3.TargetHeight > 0
}

func needsValidateSigningKey(node *seiv1alpha1.SeiNode) bool {
	if node.Spec.Validator == nil || node.Spec.Validator.SigningKey == nil {
		return false
	}
	return node.Spec.Validator.SigningKey.Secret != nil &&
		node.Spec.Validator.SigningKey.Secret.SecretName != ""
}

func validateSigningKeyParams(node *seiv1alpha1.SeiNode) any {
	if !needsValidateSigningKey(node) {
		return nil
	}
	return &task.ValidateSigningKeyParams{
		SecretName: node.Spec.Validator.SigningKey.Secret.SecretName,
		Namespace:  node.Namespace,
	}
}

func needsValidateNodeKey(node *seiv1alpha1.SeiNode) bool {
	if node.Spec.Validator == nil || node.Spec.Validator.NodeKey == nil {
		return false
	}
	return node.Spec.Validator.NodeKey.Secret != nil &&
		node.Spec.Validator.NodeKey.Secret.SecretName != ""
}

func validateNodeKeyParams(node *seiv1alpha1.SeiNode) any {
	if !needsValidateNodeKey(node) {
		return nil
	}
	return &task.ValidateNodeKeyParams{
		SecretName: node.Spec.Validator.NodeKey.Secret.SecretName,
		Namespace:  node.Namespace,
	}
}

func needsValidateOperatorKeyring(node *seiv1alpha1.SeiNode) bool {
	if node.Spec.Validator == nil || node.Spec.Validator.OperatorKeyring == nil {
		return false
	}
	s := node.Spec.Validator.OperatorKeyring.Secret
	return s != nil && s.SecretName != ""
}

func validateOperatorKeyringParams(node *seiv1alpha1.SeiNode) any {
	if !needsValidateOperatorKeyring(node) {
		return nil
	}
	s := node.Spec.Validator.OperatorKeyring.Secret
	// KeyName falls back to the CRD default for in-memory specs that
	// haven't been through admission defaulting; PassphraseSecretRef.Key
	// is required (no fallback).
	keyName := s.KeyName
	if keyName == "" {
		keyName = seiv1alpha1.DefaultOperatorKeyName
	}
	return &task.ValidateOperatorKeyringParams{
		SecretName:           s.SecretName,
		KeyName:              keyName,
		PassphraseSecretName: s.PassphraseSecretRef.SecretName,
		PassphraseSecretKey:  s.PassphraseSecretRef.Key,
		Namespace:            node.Namespace,
	}
}

// isGenesisCeremonyNode returns true when the node participates in a group genesis ceremony.
func isGenesisCeremonyNode(node *seiv1alpha1.SeiNode) bool {
	return node.Spec.Validator != nil && node.Spec.Validator.GenesisCeremony != nil
}

// SnapshotGeneration extracts the SnapshotGenerationConfig from the populated
// mode sub-spec. Callers reach through .Tendermint for mode-specific fields
// (KeepRecent, Publish).
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

// validateSnapshotGeneration returns errors without a mode prefix; callers
// wrap with their own (e.g., fmt.Errorf("fullNode: %w", err)).
func validateSnapshotGeneration(sg *seiv1alpha1.SnapshotGenerationConfig) error {
	if sg == nil {
		return nil
	}
	if sg.Tendermint == nil {
		return fmt.Errorf("snapshotGeneration is set but has no sub-struct (e.g., tendermint); omit it to disable snapshot generation")
	}
	if sg.Tendermint.Publish != nil && sg.Tendermint.KeepRecent < 2 {
		return fmt.Errorf("snapshotGeneration.tendermint.keepRecent must be >= 2 when publish is set (upload algorithm requires the second-to-latest snapshot)")
	}
	return nil
}

// validateResultExport returns errors without a mode prefix; callers
// wrap with their own (e.g., fmt.Errorf("replayer: %w", err)).
func validateResultExport(re *seiv1alpha1.ResultExportConfig) error {
	if re == nil {
		return nil
	}
	if re.ShadowResult == nil {
		return fmt.Errorf("resultExport is set but has no sub-struct (e.g., shadowResult); omit it to disable result export")
	}
	return nil
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

// SidecarURLForNode re-exports noderesource.SidecarURLForNode for callers
// that already depend on the planner package.
var SidecarURLForNode = noderesource.SidecarURLForNode

// marshalParams serializes a task params struct to apiextensionsv1.JSON.
func marshalParams(v any) (*apiextensionsv1.JSON, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshaling %T: %w", v, err)
	}
	return &apiextensionsv1.JSON{Raw: raw}, nil
}

// buildPlannedTask constructs a PlannedTask with deterministic ID,
// serialized params, and the task type's intrinsic retry budget.
func buildPlannedTask(planID, taskType string, planIndex int, params any) (seiv1alpha1.PlannedTask, error) {
	id := task.DeterministicTaskID(planID, taskType, planIndex)
	p, err := marshalParams(params)
	if err != nil {
		return seiv1alpha1.PlannedTask{}, fmt.Errorf("task %s: %w", taskType, err)
	}
	return seiv1alpha1.PlannedTask{
		Type:       taskType,
		ID:         id,
		Status:     seiv1alpha1.TaskPending,
		Params:     p,
		MaxRetries: taskMaxRetries(taskType),
	}, nil
}

// taskMaxRetries is the executor's retry budget per task type. Default 0
// makes the first ExecutionFailed terminal. Wall-clock per N retries
// (RetryCount is incremented before retryBackoff is called):
// 10s for N=1; 10s + 20s + 30s*(N-2) for N>=2.
func taskMaxRetries(taskType string) int {
	switch taskType {
	case TaskConfigureGenesis:
		return genesisConfigureMaxRetries
	case TaskAssembleGenesis:
		return groupAssemblyMaxRetries
	case task.TaskTypeReplacePod:
		return 3
	default:
		return 0
	}
}

// buildBasePlan builds a TaskPlan by starting with infrastructure tasks,
// then the base sidecar progression for the node's bootstrap mode.
func buildBasePlan(
	node *seiv1alpha1.SeiNode,
	snap *seiv1alpha1.SnapshotSource,
	configIntent *seiconfig.ConfigIntent,
) (*seiv1alpha1.TaskPlan, error) {
	sidecarProg, err := buildSidecarProgression(snap)
	if err != nil {
		return nil, err
	}
	if sg := SnapshotGeneration(node); sg != nil && sg.Tendermint != nil && sg.Tendermint.Publish != nil {
		sidecarProg, err = insertBefore(sidecarProg, TaskMarkReady, TaskSnapshotUpload)
		if err != nil {
			return nil, err
		}
	}

	// Infrastructure tasks run before sidecar tasks.
	prog := make([]string, 0, 4+len(sidecarProg))
	prog = append(prog, task.TaskTypeEnsureDataPVC)
	if needsValidateSigningKey(node) {
		// Surfaces missing/malformed key material via SigningKeyReady before
		// kubelet attempts the volume mount.
		prog = append(prog, task.TaskTypeValidateSigningKey)
	}
	if needsValidateNodeKey(node) {
		prog = append(prog, task.TaskTypeValidateNodeKey)
	}
	if needsValidateOperatorKeyring(node) {
		prog = append(prog, task.TaskTypeValidateOperatorKeyring)
	}
	prog = append(prog, task.TaskTypeApplyRBACProxyConfig)
	prog = append(prog, task.TaskTypeApplyStatefulSet, task.TaskTypeApplyService)
	prog = append(prog, sidecarProg...)

	planID := uuid.New().String()
	tasks := make([]seiv1alpha1.PlannedTask, len(prog))
	for i, taskType := range prog {
		t, err := buildPlannedTask(planID, taskType, i, paramsForTaskType(node, taskType, snap, configIntent))
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
// Sidecar tasks return seictl client.*Task wrappers directly; config-apply
// returns *seiconfig.ConfigIntent — see task.configApplyTask for why.
func paramsForTaskType(
	node *seiv1alpha1.SeiNode,
	taskType string,
	snap *seiv1alpha1.SnapshotSource,
	configIntent *seiconfig.ConfigIntent,
) any {
	switch taskType {
	// Infrastructure tasks
	case task.TaskTypeEnsureDataPVC:
		return &task.EnsureDataPVCParams{NodeName: node.Name, Namespace: node.Namespace}
	case task.TaskTypeApplyStatefulSet:
		return &task.ApplyStatefulSetParams{NodeName: node.Name, Namespace: node.Namespace}
	case task.TaskTypeApplyService:
		return &task.ApplyServiceParams{NodeName: node.Name, Namespace: node.Namespace}
	case task.TaskTypeApplyRBACProxyConfig:
		return &task.ApplyRBACProxyConfigParams{NodeName: node.Name, Namespace: node.Namespace}
	case task.TaskTypeReplacePod:
		return &task.ReplacePodParams{NodeName: node.Name, Namespace: node.Namespace}
	case task.TaskTypeObserveImage:
		return &task.ObserveImageParams{NodeName: node.Name, Namespace: node.Namespace}
	case task.TaskTypeValidateSigningKey:
		return validateSigningKeyParams(node)
	case task.TaskTypeValidateNodeKey:
		return validateNodeKeyParams(node)
	case task.TaskTypeValidateOperatorKeyring:
		return validateOperatorKeyringParams(node)

	// Sidecar tasks
	case TaskSnapshotRestore:
		return snapshotRestoreTask(snap)
	case TaskConfigureGenesis:
		return sidecar.ConfigureGenesisTask{}
	case TaskConfigApply:
		if configIntent != nil {
			return configIntent
		}
		return &seiconfig.ConfigIntent{}
	case TaskConfigureStateSync:
		return configureStateSyncTask(node)
	case TaskConfigValidate:
		return sidecar.ConfigValidateTask{}
	case TaskMarkReady:
		return sidecar.MarkReadyTask{}
	case TaskSnapshotUpload:
		return sidecar.SnapshotUploadTask{}

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
		return sidecar.GenerateIdentityTask{ChainID: gc.ChainID, Moniker: node.Name}
	case TaskGenerateGentx:
		return sidecar.GenerateGentxTask{
			ChainID:        gc.ChainID,
			StakingAmount:  gc.StakingAmount,
			AccountBalance: gc.AccountBalance,
		}
	case TaskUploadGenesisArtifacts:
		return sidecar.UploadGenesisArtifactsTask{NodeName: node.Name}
	case TaskSetGenesisPeers:
		return sidecar.SetGenesisPeersTask{}
	default:
		return nil
	}
}

func snapshotRestoreTask(snap *seiv1alpha1.SnapshotSource) sidecar.SnapshotRestoreTask {
	if snap == nil || snap.S3 == nil {
		return sidecar.SnapshotRestoreTask{}
	}
	return sidecar.SnapshotRestoreTask{TargetHeight: snap.S3.TargetHeight}
}

func configureStateSyncTask(node *seiv1alpha1.SeiNode) sidecar.ConfigureStateSyncTask {
	snap := node.Spec.SnapshotSource()
	t := sidecar.ConfigureStateSyncTask{
		UseLocalSnapshot: hasS3Snapshot(snap),
		RpcServers:       node.Status.ResolvedRPCWitnesses,
	}
	if snap != nil {
		if snap.TrustPeriod != "" {
			t.TrustPeriod = snap.TrustPeriod
		}
		t.BackfillBlocks = snap.BackfillBlocks
	}
	return t
}

// commonOverrides returns controller overrides that apply to all node modes.
// sei-config defaults logging.level to "info"; we baseline it to "error" here.
// spec.Overrides still wins via mergeOverrides for per-node verbosity.
//
// persistent_peers is written unconditionally from the controller-resolved
// set: an empty set stamps "" (symmetric, like external_address), which is a
// valid no-peers config — the controller is now the sole owner of peering, so
// the sidecar DiscoverPeers round-trip (and its empty-source deadlock) is gone.
func commonOverrides(node *seiv1alpha1.SeiNode) map[string]string {
	out := map[string]string{
		overrideKeyLoggingLevel: "error",
		keyP2PPersistentPeers:   strings.Join(node.Status.ResolvedPeers, ","),
	}
	if node.Spec.ExternalAddress != "" {
		out[seiconfig.KeyP2PExternalAddress] = node.Spec.ExternalAddress
	}
	return out
}

// sidecarNeedsReapproval reports whether the sidecar has been observed
// to have lost readiness. Mode-agnostic.
func sidecarNeedsReapproval(node *seiv1alpha1.SeiNode) bool {
	cond := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionSidecarReady)
	return cond != nil && cond.Status == metav1.ConditionFalse && cond.Reason == "NotReady"
}

// buildMarkReadyPlan is the single-task plan that re-marks sidecar readiness.
func buildMarkReadyPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
	planID := uuid.New().String()
	t, err := buildPlannedTask(planID, sidecar.TaskTypeMarkReady, 0, paramsForTaskType(node, sidecar.TaskTypeMarkReady, nil, nil))
	if err != nil {
		return nil, err
	}
	return &seiv1alpha1.TaskPlan{
		ID:          planID,
		Phase:       seiv1alpha1.TaskPlanActive,
		Tasks:       []seiv1alpha1.PlannedTask{t},
		TargetPhase: seiv1alpha1.PhaseRunning,
	}, nil
}

func imageDrifted(node *seiv1alpha1.SeiNode) bool {
	return node.Spec.Image != node.Status.CurrentImage
}

// sidecarImageDrifted reports whether the effective sidecar image diverges
// from what was last observed. Empty Status.CurrentSidecarImage means
// "not yet observed" and is treated as no-drift so a controller upgrade
// doesn't fleet-roll every node before ObserveImage backfills the field.
func sidecarImageDrifted(node *seiv1alpha1.SeiNode, p platform.Config) bool {
	if node.Status.CurrentSidecarImage == "" {
		return false
	}
	return task.EffectiveSidecarImage(node, p) != node.Status.CurrentSidecarImage
}

// imageDriftMessage formats the NodeUpdateInProgress message every mode
// planner stamps before an update plan. Names which image(s) drifted so an
// operator reading the condition can tell seid bumps from sidecar bumps.
func imageDriftMessage(node *seiv1alpha1.SeiNode, p platform.Config) string {
	seid := imageDrifted(node)
	sc := sidecarImageDrifted(node, p)
	switch {
	case seid && sc:
		return fmt.Sprintf("image drift detected: seid spec=%s current=%s; sidecar spec=%s current=%s",
			node.Spec.Image, node.Status.CurrentImage,
			task.EffectiveSidecarImage(node, p), node.Status.CurrentSidecarImage)
	case sc:
		return fmt.Sprintf("sidecar image drift detected: spec=%s current=%s",
			task.EffectiveSidecarImage(node, p), node.Status.CurrentSidecarImage)
	default:
		return fmt.Sprintf("image drift detected: spec=%s current=%s",
			node.Spec.Image, node.Status.CurrentImage)
	}
}

// p2pConfigPatch is the config.toml patch a running-node update plan applies
// to the controller-owned [p2p] keys. It stamps the publishable external
// address AND folds in the latest controller-resolved persistent_peers, so an
// already-firing update plan (image drift) carries the freshest observed peer
// set. Peer churn alone does NOT trigger a plan — this only piggybacks the
// latest valid state onto a deployment we are doing anyway. Both values stamp
// empty when unset/none, so opt-out and no-peers reach the pod symmetrically.
//
// config.toml uses hyphenated keys (external-address, persistent-peers), unlike
// the dotted enrichment keys used on the init-path config-apply override.
func p2pConfigPatch(node *seiv1alpha1.SeiNode) map[string]map[string]any {
	return map[string]map[string]any{
		"config.toml": {
			"p2p": map[string]any{
				"external-address": node.Spec.ExternalAddress,
				"persistent-peers": strings.Join(node.Status.ResolvedPeers, ","),
			},
		},
	}
}

// assembleUpdatePlan composes a per-mode task progression into a TaskPlan.
// Callers own the NodeUpdateInProgress condition — stamp it before calling
// so the reason/message reflects the actual trigger. FailedPhase stays
// empty so a failure retries on next reconcile.
func assembleUpdatePlan(node *seiv1alpha1.SeiNode, prog []string, patch map[string]map[string]any) (*seiv1alpha1.TaskPlan, error) {
	planID := uuid.New().String()
	tasks := make([]seiv1alpha1.PlannedTask, len(prog))
	for i, taskType := range prog {
		params := paramsForUpdateTask(node, taskType, patch)
		t, err := buildPlannedTask(planID, taskType, i, params)
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

// paramsForUpdateTask returns ConfigPatchTask params for TaskConfigPatch
// and delegates everything else to paramsForTaskType. Update plans never
// carry a ConfigIntent — those are init-path only.
func paramsForUpdateTask(node *seiv1alpha1.SeiNode, taskType string, patch map[string]map[string]any) any {
	if taskType == TaskConfigPatch {
		return task.ConfigPatchTask{Files: patch}
	}
	return paramsForTaskType(node, taskType, nil, nil)
}

// mergeOverrides combines controller-generated overrides with user-specified
// overrides. User overrides take precedence.
func mergeOverrides(controllerOverrides, userOverrides map[string]string) map[string]string {
	merged := make(map[string]string, len(controllerOverrides)+len(userOverrides)+1)
	maps.Copy(merged, controllerOverrides)
	maps.Copy(merged, userOverrides)
	applyForcedOverrides(merged, userOverrides)
	return merged
}

func applyForcedOverrides(merged, userOverrides map[string]string) {
	if got, ok := userOverrides[overrideKeyLoggingLevel]; ok && got != enforcedLoggingLevel {
		ctrl.Log.WithName("planner").Info(
			"rejecting user override",
			"key", overrideKeyLoggingLevel,
			"user_value", got,
			"forced_value", enforcedLoggingLevel,
		)
	}
	merged[overrideKeyLoggingLevel] = enforcedLoggingLevel
}
