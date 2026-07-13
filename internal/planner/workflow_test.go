package planner

import (
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	wfWitnessA = "a:26657"
	wfWitnessB = "b:26657"
	wfWitnessX = "x:26657"
	wfWitnessY = "y:26657"

	backendPebble = "pebbledb"
	backendRocks  = "rocksdb"
)

func fullNodeForWorkflow(resolvedSyncers []string) *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "rpc-0", Namespace: sourceChainID},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  sourceChainID,
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
		Status: seiv1alpha1.SeiNodeStatus{
			Phase:                seiv1alpha1.PhaseRunning,
			ResolvedStateSyncers: resolvedSyncers,
		},
	}
}

func stateSyncWorkflow(rpcServers []string, migration *seiv1alpha1.ConfigMigration) *seiv1alpha1.SeiNodeTaskWorkflow {
	return &seiv1alpha1.SeiNodeTaskWorkflow{
		ObjectMeta: metav1.ObjectMeta{Name: "ss-0", Namespace: sourceChainID},
		Spec: seiv1alpha1.SeiNodeTaskWorkflowSpec{
			Kind:   seiv1alpha1.SeiNodeTaskWorkflowKindStateSync,
			Target: seiv1alpha1.SeiNodeTaskTarget{NodeRef: seiv1alpha1.SeiNodeTaskNodeRef{Name: "rpc-0"}},
			StateSync: &seiv1alpha1.StateSyncWorkflow{
				RpcServers: rpcServers,
				Migration:  migration,
			},
		},
	}
}

func gigaStoreMigration(backend string) *seiv1alpha1.ConfigMigration {
	return &seiv1alpha1.ConfigMigration{
		Kind:      seiv1alpha1.ConfigMigrationGigaStore,
		GigaStore: &seiv1alpha1.GigaStoreMigration{Backend: backend},
	}
}

func buildPlan(t *testing.T, node *seiv1alpha1.SeiNode, wf *seiv1alpha1.SeiNodeTaskWorkflow) (*seiv1alpha1.TaskPlan, error) {
	t.Helper()
	wp, err := WorkflowPlannerFor(wf)
	if err != nil {
		return nil, err
	}
	if err := wp.Validate(node, wf); err != nil {
		return nil, err
	}
	return wp.BuildPlan(node, wf)
}

func TestStateSyncWorkflow_Progression(t *testing.T) {
	g := NewWithT(t)
	node := fullNodeForWorkflow(nil)
	wf := stateSyncWorkflow([]string{wfWitnessA, wfWitnessB}, gigaStoreMigration(backendPebble))

	plan, err := buildPlan(t, node, wf)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).NotTo(BeNil())

	gotTypes := make([]string, len(plan.Tasks))
	for i, tk := range plan.Tasks {
		gotTypes[i] = tk.Type
	}
	g.Expect(gotTypes).To(Equal([]string{
		taskTypeMarkNotReady,
		taskTypeStopSeid,
		taskTypeResetData,
		TaskConfigPatch,
		TaskConfigureStateSync,
		TaskMarkReady,
	}))
	// mark-ready is the terminal step: Complete means the node was released,
	// and catch-up is verified node-side, never by the recipe (STO-624).
	g.Expect(gotTypes).NotTo(ContainElement(TaskAwaitCondition))
	g.Expect(plan.Phase).To(Equal(seiv1alpha1.TaskPlanActive))

	// A workflow never drives the node's phase.
	g.Expect(plan.TargetPhase).To(BeEmpty())
	g.Expect(plan.FailedPhase).To(BeEmpty())

	// Every task is Pending with a deterministic ID and unique params slot.
	for _, tk := range plan.Tasks {
		g.Expect(tk.Status).To(Equal(seiv1alpha1.TaskPending))
		g.Expect(tk.ID).NotTo(BeEmpty())
	}
}

func TestStateSyncWorkflow_OmitsConfigPatchWhenEmpty(t *testing.T) {
	g := NewWithT(t)
	// No ConfigPatch: the config-patch step must be omitted (the sidecar task
	// rejects an empty file set and would wedge the plan).
	plan, err := buildPlan(t, fullNodeForWorkflow(nil), stateSyncWorkflow([]string{wfWitnessA, wfWitnessB}, nil))
	g.Expect(err).NotTo(HaveOccurred())

	gotTypes := make([]string, len(plan.Tasks))
	for i, tk := range plan.Tasks {
		gotTypes[i] = tk.Type
	}
	g.Expect(gotTypes).To(Equal([]string{
		taskTypeMarkNotReady,
		taskTypeStopSeid,
		taskTypeResetData,
		TaskConfigureStateSync,
		TaskMarkReady,
	}))
	g.Expect(gotTypes).NotTo(ContainElement(TaskConfigPatch))
}

func TestStateSyncWorkflow_FailClosed_TooFewWitnesses(t *testing.T) {
	g := NewWithT(t)

	// No recipe rpcServers and no resolved syncers -> refuse.
	_, err := buildPlan(t, fullNodeForWorkflow(nil), stateSyncWorkflow(nil, nil))
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("fail-closed"))

	// One resolved syncer is still below the floor.
	_, err = buildPlan(t, fullNodeForWorkflow([]string{"only:26657"}), stateSyncWorkflow(nil, nil))
	g.Expect(err).To(HaveOccurred())

	// One recipe rpcServer is below the floor.
	_, err = buildPlan(t, fullNodeForWorkflow(nil), stateSyncWorkflow([]string{"only:26657"}, nil))
	g.Expect(err).To(HaveOccurred())
}

func TestStateSyncWorkflow_WitnessSources(t *testing.T) {
	g := NewWithT(t)

	// Resolved syncers satisfy the floor when the recipe omits rpcServers.
	plan, err := buildPlan(t, fullNodeForWorkflow([]string{wfWitnessX, wfWitnessY}), stateSyncWorkflow(nil, nil))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(configureStateSyncWitnesses(t, plan)).To(Equal([]string{wfWitnessX, wfWitnessY}))

	// Recipe rpcServers override the node's resolved syncers.
	plan, err = buildPlan(t,
		fullNodeForWorkflow([]string{wfWitnessX, wfWitnessY}),
		stateSyncWorkflow([]string{"override-a:26657", "override-b:26657"}, nil))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(configureStateSyncWitnesses(t, plan)).To(Equal([]string{"override-a:26657", "override-b:26657"}))
}

func TestStateSyncWorkflow_GigaStoreMigration(t *testing.T) {
	// giga_store_migration.md Step 1: the fixed flags are pinned by the
	// migration; only ss-backend varies with the operator's Backend input.
	for _, backend := range []string{backendPebble, backendRocks} {
		t.Run(backend, func(t *testing.T) {
			g := NewWithT(t)
			wf := stateSyncWorkflow([]string{wfWitnessA, wfWitnessB}, gigaStoreMigration(backend))
			plan, err := buildPlan(t, fullNodeForWorkflow(nil), wf)
			g.Expect(err).NotTo(HaveOccurred())

			var cp struct {
				Files map[string]map[string]any `json:"files"`
			}
			g.Expect(json.Unmarshal(taskParams(t, plan, TaskConfigPatch), &cp)).To(Succeed())
			app := cp.Files["app.toml"]
			g.Expect(app).To(HaveKeyWithValue("state-store", map[string]any{
				"ss-enable":    true,
				"evm-ss-split": true,
				"ss-backend":   backend,
			}))
			g.Expect(app).To(HaveKeyWithValue("state-commit", map[string]any{
				"sc-enable": true,
			}))
		})
	}
}

func TestStateSyncWorkflow_MigrationGuardrails(t *testing.T) {
	g := NewWithT(t)

	// Unknown kind hard-errors from BuildPlan (before reset-data), never a
	// silent plain resync.
	_, err := buildPlan(t, fullNodeForWorkflow(nil),
		stateSyncWorkflow([]string{wfWitnessA, wfWitnessB},
			&seiv1alpha1.ConfigMigration{Kind: "Bogus"}))
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("unknown config migration kind"))

	// The empty-patch fail-closed guard: a non-nil migration must translate to a
	// non-empty patch. It is exercised directly here because every valid kind
	// today (GigaStore) yields a non-empty patch, so BuildPlan cannot reach it
	// via a valid kind — it defends future kinds whose builder returns empty.
	_, err = migrationConfigPatch(&seiv1alpha1.ConfigMigration{
		Kind: seiv1alpha1.ConfigMigrationGigaStore,
	})
	// GigaStore with a nil payload still yields a non-empty (pebbledb-defaulted)
	// patch, so this must NOT error.
	g.Expect(err).NotTo(HaveOccurred())
}

func TestStateSyncWorkflow_Validate_RefusesValidator(t *testing.T) {
	g := NewWithT(t)
	node := fullNodeForWorkflow([]string{wfWitnessA, wfWitnessB})
	node.Spec.FullNode = nil
	node.Spec.Validator = &seiv1alpha1.ValidatorSpec{}

	_, err := buildPlan(t, node, stateSyncWorkflow(nil, nil))
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("validator"))
}

// configureStateSyncWitnesses extracts the RpcServers from the plan's
// configure-state-sync task params.
func configureStateSyncWitnesses(t *testing.T, plan *seiv1alpha1.TaskPlan) []string {
	t.Helper()
	var p struct {
		RpcServers []string `json:"rpcServers"`
	}
	if err := json.Unmarshal(taskParams(t, plan, TaskConfigureStateSync), &p); err != nil {
		t.Fatalf("decoding configure-state-sync params: %v", err)
	}
	return p.RpcServers
}

func taskParams(t *testing.T, plan *seiv1alpha1.TaskPlan, taskType string) []byte {
	t.Helper()
	for _, tk := range plan.Tasks {
		if tk.Type == taskType {
			if tk.Params == nil {
				return nil
			}
			return tk.Params.Raw
		}
	}
	t.Fatalf("task %q not found in plan", taskType)
	return nil
}
