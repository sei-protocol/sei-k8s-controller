package planner

import (
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	wfWitnessA = "a:26657"
	wfWitnessB = "b:26657"
	wfWitnessX = "x:26657"
	wfWitnessY = "y:26657"
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

func stateSyncWorkflow(rpcServers []string, patch map[string]map[string]apiextensionsv1.JSON) *seiv1alpha1.SeiNodeTaskWorkflow {
	return &seiv1alpha1.SeiNodeTaskWorkflow{
		ObjectMeta: metav1.ObjectMeta{Name: "ss-0", Namespace: sourceChainID},
		Spec: seiv1alpha1.SeiNodeTaskWorkflowSpec{
			Kind:   seiv1alpha1.SeiNodeTaskWorkflowKindStateSync,
			Target: seiv1alpha1.SeiNodeTaskTarget{NodeRef: seiv1alpha1.SeiNodeTaskNodeRef{Name: "rpc-0"}},
			StateSync: &seiv1alpha1.StateSyncWorkflow{
				RpcServers:  rpcServers,
				ConfigPatch: patch,
			},
		},
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
	patch := map[string]map[string]apiextensionsv1.JSON{
		"app.toml": {"state-store.evm-ss-split": {Raw: []byte(`true`)}},
	}
	wf := stateSyncWorkflow([]string{wfWitnessA, wfWitnessB}, patch)

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
		TaskAwaitCondition,
	}))
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
		TaskAwaitCondition,
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

func TestStateSyncWorkflow_ConfigPatchDecoded(t *testing.T) {
	g := NewWithT(t)
	patch := map[string]map[string]apiextensionsv1.JSON{
		"app.toml": {"state-store.evm-ss-split": {Raw: []byte(`true`)}},
	}
	plan, err := buildPlan(t, fullNodeForWorkflow(nil), stateSyncWorkflow([]string{wfWitnessA, wfWitnessB}, patch))
	g.Expect(err).NotTo(HaveOccurred())

	var cp struct {
		Files map[string]map[string]any `json:"files"`
	}
	g.Expect(json.Unmarshal(taskParams(t, plan, TaskConfigPatch), &cp)).To(Succeed())
	g.Expect(cp.Files["app.toml"]["state-store.evm-ss-split"]).To(BeTrue())
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
