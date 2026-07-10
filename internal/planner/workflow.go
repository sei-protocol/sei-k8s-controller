package planner

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

// Task-type strings for the three new sidecar tasks the StateSync recipe
// introduces, bound to seictl's wire constants so a rename on either side is a
// compile error rather than a silent wire mismatch.
const (
	taskTypeMarkNotReady = sidecar.TaskTypeMarkNotReady
	taskTypeStopSeid     = sidecar.TaskTypeStopSeid
	taskTypeResetData    = sidecar.TaskTypeResetData

	// minStateSyncWitnesses restores the init path's fail-closed floor
	// (CometBFT needs >=2 rpc-servers; a single reachable witness is padded by
	// duplication in the sidecar, which the Running-phase path would otherwise
	// let through).
	minStateSyncWitnesses = 2
)

// WorkflowPlanner compiles a SeiNodeTaskWorkflow recipe into a TaskPlan for a
// resolved target node. It mirrors the NodePlanner Validate/BuildPlan shape:
// one reviewed builder per recipe arm.
type WorkflowPlanner interface {
	// Validate rejects a recipe that can never compile against this target,
	// independent of transient state.
	Validate(node *seiv1alpha1.SeiNode, wf *seiv1alpha1.SeiNodeTaskWorkflow) error
	// BuildPlan compiles the recipe into an Active TaskPlan, or returns an
	// error if it cannot be built safely (e.g. fail-closed witness floor).
	BuildPlan(node *seiv1alpha1.SeiNode, wf *seiv1alpha1.SeiNodeTaskWorkflow) (*seiv1alpha1.TaskPlan, error)
}

// WorkflowPlannerFor selects the builder for a workflow's recipe kind.
func WorkflowPlannerFor(wf *seiv1alpha1.SeiNodeTaskWorkflow) (WorkflowPlanner, error) {
	switch wf.Spec.Kind {
	case seiv1alpha1.SeiNodeTaskWorkflowKindStateSync:
		return &stateSyncWorkflowPlanner{}, nil
	default:
		return nil, fmt.Errorf("unknown workflow kind %q", wf.Spec.Kind)
	}
}

type stateSyncWorkflowPlanner struct{}

func (p *stateSyncWorkflowPlanner) Validate(node *seiv1alpha1.SeiNode, wf *seiv1alpha1.SeiNodeTaskWorkflow) error {
	if wf.Spec.StateSync == nil {
		return fmt.Errorf("stateSync recipe params are nil")
	}
	// Validators are excluded (wipe-and-resync is double-sign-adjacent). CEL
	// cannot see the target, so this is the belt-and-braces check behind the
	// adoption-time refusal.
	if node.Spec.Validator != nil {
		return fmt.Errorf("stateSync workflow refuses validator target %s/%s", node.Namespace, node.Name)
	}
	return nil
}

// BuildPlan compiles the StateSync progression:
//
//	mark-not-ready -> stop-seid -> reset-data -> config-patch ->
//	configure-state-sync -> mark-ready -> await-condition(catchingUp)
//
// TargetPhase and FailedPhase are left empty: a workflow never drives the
// node's phase (a failure parks the node held, it does not fail the node).
func (p *stateSyncWorkflowPlanner) BuildPlan(node *seiv1alpha1.SeiNode, wf *seiv1alpha1.SeiNodeTaskWorkflow) (*seiv1alpha1.TaskPlan, error) {
	ss := wf.Spec.StateSync

	witnesses := ss.RpcServers
	if len(witnesses) == 0 {
		witnesses = node.Status.ResolvedStateSyncers
	}
	if len(witnesses) < minStateSyncWitnesses {
		return nil, fmt.Errorf(
			"stateSync workflow fail-closed: need >=%d resolved witnesses (recipe rpcServers or node.status.resolvedStateSyncers), have %d",
			minStateSyncWitnesses, len(witnesses))
	}

	configPatch, err := configPatchFiles(ss.ConfigPatch)
	if err != nil {
		return nil, fmt.Errorf("stateSync workflow config patch: %w", err)
	}

	cfgSS := configureStateSyncTask(node)
	cfgSS.RpcServers = witnesses
	// A network resync configures its trust point from live witnesses, not a
	// local S3 snapshot artifact.
	cfgSS.UseLocalSnapshot = false

	planID := uuid.New().String()
	// Ordering is load-bearing, not advisory: mark-not-ready re-arms the start
	// gate and purges mark-ready records; stop-seid brings seid down; only then
	// does reset-data run (it refuses if seid's RPC still answers). The release
	// step's mark-ready always executes fresh because mark-not-ready purged the
	// prior record. The final catchingUp await-condition carries no
	// targetHeight (the live head is unknown at plan-build time).
	type step struct {
		taskType string
		params   any
	}
	steps := []step{
		{taskTypeMarkNotReady, sidecar.MarkNotReadyTask{}},
		{taskTypeStopSeid, sidecar.StopSeidTask{}},
		{taskTypeResetData, sidecar.ResetDataTask{}},
	}
	// config-patch is included only when the recipe carries a patch: the
	// sidecar task rejects an empty file set, so an always-present step would
	// wedge a resync that changes no config.
	if len(configPatch) > 0 {
		steps = append(steps, step{TaskConfigPatch, task.ConfigPatchTask{Files: configPatch}})
	}
	steps = append(steps,
		step{TaskConfigureStateSync, cfgSS},
		step{TaskMarkReady, sidecar.MarkReadyTask{}},
		step{TaskAwaitCondition, sidecar.AwaitConditionTask{Condition: sidecar.ConditionCatchingUp}},
	)

	tasks := make([]seiv1alpha1.PlannedTask, 0, len(steps))
	for i, s := range steps {
		t, err := buildPlannedTask(planID, s.taskType, i, s.params)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}

	return &seiv1alpha1.TaskPlan{
		ID:    planID,
		Phase: seiv1alpha1.TaskPlanActive,
		Tasks: tasks,
		// TargetPhase / FailedPhase intentionally empty — see method doc.
	}, nil
}

// configPatchFiles converts the CRD's map[string]map[string]JSON into the
// config-patch task's map[string]map[string]any wire shape, decoding each
// raw JSON value into its natural Go type.
func configPatchFiles(in map[string]map[string]apiextensionsv1.JSON) (map[string]map[string]any, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]map[string]any, len(in))
	for file, section := range in {
		sec := make(map[string]any, len(section))
		for key, raw := range section {
			var v any
			if len(raw.Raw) > 0 {
				if err := json.Unmarshal(raw.Raw, &v); err != nil {
					return nil, fmt.Errorf("file %q key %q: %w", file, key, err)
				}
			}
			sec[key] = v
		}
		out[file] = sec
	}
	return out, nil
}
