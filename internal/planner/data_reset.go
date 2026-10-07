package planner

import (
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
	sidecar "github.com/sei-protocol/sei-k8s-controller/sidecarapi/client"
)

// dataResetMaxRetries is reset-data's retry budget inside a reset plan. The
// sidecar refuses a wipe while seid's RPC answers or an operator's seid or seidb
// process runs; retrying with backoff rides out a short overlap before the plan
// fails and the planner builds it again.
const dataResetMaxRetries = 5

// buildDataResetPlan builds the plan that performs one declarative data reset
// (spec 009). One sequence serves every case, whether or not the pod rolled:
//
//	observe-image -> mark-not-ready -> stop-seid ->
//	reset-data-keep-sign-state -> record-data-reset -> mark-ready
//
// observe-image waits for the StatefulSet rollout, so the sidecar tasks reach
// the pod that runs the current template. On a pod already parked at the start
// gate, mark-not-ready and stop-seid are no-ops. record-data-reset stamps the
// counter the plan was built for before mark-ready, so the start guard sees the
// reset as done only for that value. The wipe is submitted as
// reset-data-keep-sign-state, a type only a sidecar that keeps the sign state
// accepts: against an older sidecar the submission fails and retries, with seid
// held, rather than zeroing a validator's sign state.
func buildDataResetPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
	stopUpCheck := noderesource.UpCheckForNode(node)
	steps := []struct {
		taskType string
		params   any
	}{
		{task.TaskTypeObserveImage, task.ObserveImageParams{NodeName: node.Name, Namespace: node.Namespace}},
		{taskTypeMarkNotReady, sidecar.MarkNotReadyTask{}},
		{taskTypeStopSeid, sidecar.StopSeidTask{UpCheck: &stopUpCheck}},
		{sidecar.TaskTypeResetDataKeepSignState, sidecar.ResetDataKeepSignStateTask{}},
		{task.TaskTypeRecordDataReset, task.RecordDataResetParams{Generation: node.Spec.DataResetGeneration}},
		{TaskMarkReady, sidecar.MarkReadyTask{}},
	}

	planID := uuid.New().String()
	tasks := make([]seiv1alpha1.PlannedTask, 0, len(steps))
	for i, s := range steps {
		t, err := buildPlannedTask(planID, s.taskType, i, s.params)
		if err != nil {
			return nil, err
		}
		if s.taskType == sidecar.TaskTypeResetDataKeepSignState {
			t.MaxRetries = dataResetMaxRetries
		}
		tasks = append(tasks, t)
	}
	return &seiv1alpha1.TaskPlan{
		ID:          planID,
		Phase:       seiv1alpha1.TaskPlanActive,
		Tasks:       tasks,
		TargetPhase: seiv1alpha1.PhaseRunning,
		// FailedPhase stays empty: a failed reset leaves the gate closed and the
		// node Running, and the planner builds the reset plan again.
	}, nil
}

// isDataResetPlan reports whether plan is a reset plan. record-data-reset
// appears in no other plan.
func isDataResetPlan(plan *seiv1alpha1.TaskPlan) bool {
	return plan != nil && slices.ContainsFunc(plan.Tasks, func(t seiv1alpha1.PlannedTask) bool {
		return t.Type == task.TaskTypeRecordDataReset
	})
}

// ResolveDataReset keeps the handled counter and the always-present
// DataResetInProgress condition current. The reconciler calls it on every path,
// before the Failed and Paused early returns, so the condition is never absent.
//
// While the node is not Running it moves the handled counter up to the spec
// value: no plan resets a node that is still initializing, so creating a node
// with a non-zero counter never wipes it.
//
// It writes only the states no plan transition owns. The planner sets
// ResetRunning when it builds a first reset plan (markDataResetStarted), and
// ResetComplete or ResetFailed when it observes a reset plan end
// (observeTerminalDataResetPlan). ResetFailed stays through later attempts
// until one succeeds, so the last failure stays readable while the controller
// retries.
func ResolveDataReset(node *seiv1alpha1.SeiNode) {
	if node.Spec.NodeConfig == nil {
		setDataResetCondition(node, metav1.ConditionFalse, seiv1alpha1.ReasonDataResetNotApplicable,
			"spec.nodeConfig is not set; the declarative data reset applies only to ConfigMap-configured nodes")
		return
	}
	if node.Status.Phase != seiv1alpha1.PhaseRunning &&
		node.Status.DataResetGeneration < node.Spec.DataResetGeneration {
		node.Status.DataResetGeneration = node.Spec.DataResetGeneration
	}

	reason := dataResetReason(node)
	switch {
	case isDataResetPlan(node.Status.Plan):
		// The plan's own transitions own the condition until the planner
		// clears it.
	case !task.DataResetPending(node):
		if reason != seiv1alpha1.ReasonResetComplete {
			setDataResetCondition(node, metav1.ConditionFalse, seiv1alpha1.ReasonNoResetRequested,
				fmt.Sprintf("no data reset requested (dataResetGeneration=%d)", node.Status.DataResetGeneration))
		}
	case reason == seiv1alpha1.ReasonResetFailed:
		// Keep the last failure readable until a reset succeeds.
	default:
		setDataResetCondition(node, metav1.ConditionTrue, seiv1alpha1.ReasonResetPending,
			fmt.Sprintf("data reset pending for dataResetGeneration=%d (handled %d)",
				node.Spec.DataResetGeneration, node.Status.DataResetGeneration))
	}
}

// markDataResetStarted records a new reset plan on the condition. A retry after
// a failure keeps ResetFailed, so the condition keeps naming the last error.
func markDataResetStarted(node *seiv1alpha1.SeiNode) {
	if dataResetReason(node) == seiv1alpha1.ReasonResetFailed {
		return
	}
	setDataResetCondition(node, metav1.ConditionTrue, seiv1alpha1.ReasonResetRunning, dataResetRunningMessage(node))
}

func dataResetReason(node *seiv1alpha1.SeiNode) string {
	if c := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionDataResetInProgress); c != nil {
		return c.Reason
	}
	return ""
}

func dataResetRunningMessage(node *seiv1alpha1.SeiNode) string {
	return fmt.Sprintf("resetting data for dataResetGeneration=%d", node.Spec.DataResetGeneration)
}

// observeTerminalDataResetPlan records the end of a reset plan on the
// condition. Called from handleTerminalPlan before the plan is cleared.
func observeTerminalDataResetPlan(node *seiv1alpha1.SeiNode, plan *seiv1alpha1.TaskPlan) {
	if !isDataResetPlan(plan) {
		return
	}
	switch plan.Phase {
	case seiv1alpha1.TaskPlanComplete:
		setDataResetCondition(node, metav1.ConditionFalse, seiv1alpha1.ReasonResetComplete,
			fmt.Sprintf("data reset complete for dataResetGeneration=%d", node.Status.DataResetGeneration))
	case seiv1alpha1.TaskPlanFailed:
		if startDeferred(plan) {
			// The wipe ran and the counter was recorded; only the final start
			// was refused. A counter that rose again folds into one more reset,
			// so the condition stays pending for the newer value. Otherwise a
			// hold arrived, and the reset itself is complete.
			if task.DataResetPending(node) {
				setDataResetCondition(node, metav1.ConditionTrue, seiv1alpha1.ReasonResetPending,
					fmt.Sprintf("data reset recorded for dataResetGeneration=%d; reset for %d pending",
						node.Status.DataResetGeneration, node.Spec.DataResetGeneration))
				return
			}
			setDataResetCondition(node, metav1.ConditionFalse, seiv1alpha1.ReasonResetComplete,
				fmt.Sprintf("data reset complete for dataResetGeneration=%d; start deferred: %s",
					node.Status.DataResetGeneration, plan.FailedTaskDetail.Error))
			return
		}
		setDataResetCondition(node, metav1.ConditionTrue, seiv1alpha1.ReasonResetFailed,
			fmt.Sprintf("data reset for dataResetGeneration=%d failed, seid stays held and the controller retries: %s",
				node.Spec.DataResetGeneration, planFailureMessage(plan)))
	}
}

// startDeferred reports whether plan failed only because the start guard
// refused its mark-ready, after every task before it completed.
func startDeferred(plan *seiv1alpha1.TaskPlan) bool {
	d := plan.FailedTaskDetail
	if d == nil || d.Type != TaskMarkReady || !strings.Contains(d.Error, task.StartGuardRefusal) {
		return false
	}
	for _, t := range plan.Tasks {
		if t.Type == TaskMarkReady {
			return true
		}
		if t.Status != seiv1alpha1.TaskComplete {
			return false
		}
	}
	return false
}

func setDataResetCondition(node *seiv1alpha1.SeiNode, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               seiv1alpha1.ConditionDataResetInProgress,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: node.Generation,
	})
}
