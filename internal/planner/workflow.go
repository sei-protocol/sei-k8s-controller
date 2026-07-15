package planner

import (
	"fmt"

	"github.com/google/uuid"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"

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
	// Only a full/RPC node is a valid stateSync target; the recipe wipes and
	// resyncs, which is invalid for a validator (double-sign-adjacent), an archive
	// (block-syncs, never restores from a snapshot), or a replayer (ephemeral
	// restore workload). CEL cannot see the target, so this allowlist is the
	// belt-and-braces check behind the adoption-time refusal
	// (SeiNodeReconciler.ineligibleWorkflowRole), kept in lockstep with it.
	if node.Spec.FullNode == nil {
		return fmt.Errorf("stateSync workflow refuses non-full/RPC target %s/%s", node.Namespace, node.Name)
	}
	return nil
}

// BuildPlan compiles the StateSync progression:
//
//	mark-not-ready -> stop-seid -> reset-data -> config-patch ->
//	configure-state-sync -> mark-ready
//
// The recipe ends at mark-ready: Complete means every mutation was performed
// and the node was released to re-bootstrap. Catch-up is verified node-side
// (sdk WaitCaughtUp, RPC, alerts) by whoever triggered the workflow. Because
// mark-ready is the terminal step, a Failed workflow always leaves the node
// held, and the adoption slot frees at release.
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

	// Translate the typed migration into the config-patch step's TOML tree here,
	// in BuildPlan, so a bad migration aborts the build BEFORE any step runs
	// (in particular before reset-data wipes the node). A non-nil migration
	// that produces an empty patch is a hard error: silently dropping the
	// config-patch step would give a plain resync on a wiped node while the
	// operator believes a migration ran.
	configPatch, err := migrationConfigPatch(ss.Migration)
	if err != nil {
		return nil, fmt.Errorf("stateSync workflow config migration: %w", err)
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
	// prior record, and it is the terminal step, so the workflow completes at
	// release.
	type step struct {
		taskType string
		params   any
	}
	steps := []step{
		{taskTypeMarkNotReady, sidecar.MarkNotReadyTask{}},
		{taskTypeStopSeid, sidecar.StopSeidTask{}},
		{taskTypeResetData, sidecar.ResetDataTask{}},
	}
	// config-patch is included only when the recipe carries a migration (a
	// nil Migration yields an empty patch): the sidecar task rejects an empty
	// file set, so an always-present step would wedge a plain resync that
	// changes no config.
	if len(configPatch) > 0 {
		steps = append(steps, step{TaskConfigPatch, task.ConfigPatchTask{Files: configPatch}})
	}
	steps = append(steps,
		step{TaskConfigureStateSync, cfgSS},
		step{TaskMarkReady, sidecar.MarkReadyTask{}},
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

// migrationConfigPatch maps a typed ConfigMigration into the config-patch
// task's file -> section -> value TOML tree (the same shape p2pConfigPatch
// produces — the task deep-merges the raw tree). A nil migration returns an
// empty patch, which the caller's `len > 0` gate turns into "no config-patch
// step" (a plain re-bootstrap). A non-nil migration MUST yield a non-empty
// patch, and an unknown kind is a hard error — both fail the plan build rather
// than silently produce a plain resync on a wiped node.
func migrationConfigPatch(m *seiv1alpha1.ConfigMigration) (map[string]map[string]any, error) {
	if m == nil {
		return nil, nil
	}
	var files map[string]map[string]any
	switch m.Kind {
	case seiv1alpha1.ConfigMigrationGigaStore:
		files = gigaStoreConfigPatch(m.GigaStore)
	default:
		return nil, fmt.Errorf("unknown config migration kind %q", m.Kind)
	}
	// Fail-closed: a non-nil migration that translates to nothing would silently
	// become a plain resync (config-patch step dropped) on a wiped node.
	if len(files) == 0 {
		return nil, fmt.Errorf("config migration kind %q produced an empty patch", m.Kind)
	}
	return files, nil
}

// gigaStoreConfigPatch builds the app.toml patch for the giga SS-store
// migration. The section/key names and the fixed enabling flags are pinned
// against sei-chain docs/migration/giga_store_migration.md (Step 1): a
// chain-side key rename is a controller code change, not a CRD change. Only
// Backend is an operator input; ss-enable/evm-ss-split/sc-enable are set by the
// migration itself. Backend is "" only if defaulting did not run (a raw
// controller-internal caller); the CRD default is pebbledb.
func gigaStoreConfigPatch(g *seiv1alpha1.GigaStoreMigration) map[string]map[string]any {
	backend := "pebbledb"
	if g != nil && g.Backend != "" {
		backend = g.Backend
	}
	return map[string]map[string]any{
		"app.toml": {
			"state-store": map[string]any{
				"ss-enable":    true,
				"evm-ss-split": true,
				"ss-backend":   backend,
			},
			"state-commit": map[string]any{
				"sc-enable": true,
			},
		},
	}
}
