package planner

import (
	"fmt"
	"slices"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
	sidecar "github.com/sei-protocol/sei-k8s-controller/sidecarapi/client"
)

// Spec 010 (maintenance hold). spec.maintenance.hold is the request;
// status.maintenanceHold is the hold in effect. Every plan that changes what
// the hold does to seid ends with record-maintenance-hold, so the two compare
// the same way spec 009 compares its counters.

// planStep is one task of a plan under construction.
type planStep struct {
	taskType string
	params   any
}

// buildHoldPlan returns the plan that moves the hold in effect from have to
// want, or nil when they agree.
//
//	want=Immediate             mark-not-ready -> stop-seid -> record(Immediate)
//	want=AfterExit, have=""    mark-not-ready -> record(AfterExit)
//	want=AfterExit, have=Imm.  observe-image -> start-seid-once ->
//	                           await-seid-start -> mark-not-ready -> record(AfterExit)
//	want="" (release)          observe-image -> mark-ready -> record("")
//
// start-seid-once starts a parked seid once; mark-not-ready closes the gate
// again a few seconds after seid starts, long before it loads its state, so it
// parks at its next exit. observe-image waits for any rollout, so the sidecar
// tasks reach the pod that runs the current template.
func buildHoldPlan(node *seiv1alpha1.SeiNode, want, have seiv1alpha1.MaintenanceHold) (*seiv1alpha1.TaskPlan, error) {
	if want == have {
		return nil, nil
	}
	observe := planStep{task.TaskTypeObserveImage, task.ObserveImageParams{NodeName: node.Name, Namespace: node.Namespace}}
	stopUpCheck := noderesource.UpCheckForNode(node)

	steps := make([]planStep, 0, 5)
	switch {
	case want == seiv1alpha1.MaintenanceHoldImmediate:
		steps = append(steps,
			planStep{taskTypeMarkNotReady, sidecar.MarkNotReadyTask{}},
			planStep{taskTypeStopSeid, sidecar.StopSeidTask{UpCheck: &stopUpCheck}},
		)
	case want == seiv1alpha1.MaintenanceHoldAfterExit && have == seiv1alpha1.MaintenanceHoldImmediate:
		steps = append(steps,
			observe,
			planStep{task.TaskTypeStartSeidOnce, sidecar.MarkReadyTask{}},
			planStep{sidecar.TaskTypeAwaitSeidStart, sidecar.AwaitSeidStartTask{}},
			planStep{taskTypeMarkNotReady, sidecar.MarkNotReadyTask{}},
		)
	case want == seiv1alpha1.MaintenanceHoldAfterExit:
		steps = append(steps, planStep{taskTypeMarkNotReady, sidecar.MarkNotReadyTask{}})
	default: // release
		steps = append(steps, observe, planStep{TaskMarkReady, sidecar.MarkReadyTask{}})
	}
	steps = append(steps, recordHoldStep(want))
	return assembleSteps(steps)
}

func recordHoldStep(hold seiv1alpha1.MaintenanceHold) planStep {
	return planStep{task.TaskTypeRecordMaintenanceHold, task.RecordMaintenanceHoldParams{Hold: hold}}
}

// assembleSteps turns steps into an Active Running-phase plan. FailedPhase
// stays empty: a failed hold or release keeps the node Running and the planner
// builds the plan again.
func assembleSteps(steps []planStep) (*seiv1alpha1.TaskPlan, error) {
	planID := uuid.New().String()
	tasks := make([]seiv1alpha1.PlannedTask, 0, len(steps))
	for i, s := range steps {
		t, err := buildPlannedTask(planID, s.taskType, i, s.params)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return &seiv1alpha1.TaskPlan{
		ID:          planID,
		Phase:       seiv1alpha1.TaskPlanActive,
		Tasks:       tasks,
		TargetPhase: seiv1alpha1.PhaseRunning,
	}, nil
}

// parkInsteadOfRelease ends a plan with seid parked rather than started, for a
// node whose hold is requested: the trailing mark-ready becomes
// record-maintenance-hold(Immediate). The reset plan and the init plan use it.
func parkInsteadOfRelease(plan *seiv1alpha1.TaskPlan) error {
	last := len(plan.Tasks) - 1
	if last < 0 || plan.Tasks[last].Type != TaskMarkReady {
		return fmt.Errorf("plan %s does not end in %s; cannot park it", plan.ID, TaskMarkReady)
	}
	step := recordHoldStep(seiv1alpha1.MaintenanceHoldImmediate)
	t, err := buildPlannedTask(plan.ID, step.taskType, last, step.params)
	if err != nil {
		return err
	}
	plan.Tasks[last] = t
	return nil
}

// withoutMarkReady drops mark-ready from a plan built for a held node, so an
// image roll under a hold leaves the new pod parked.
func withoutMarkReady(plan *seiv1alpha1.TaskPlan) {
	plan.Tasks = slices.DeleteFunc(plan.Tasks, func(t seiv1alpha1.PlannedTask) bool {
		return t.Type == TaskMarkReady
	})
}

// isMaintenancePlan reports whether plan changes the hold in effect.
func isMaintenancePlan(plan *seiv1alpha1.TaskPlan) bool {
	return plan != nil && slices.ContainsFunc(plan.Tasks, func(t seiv1alpha1.PlannedTask) bool {
		return t.Type == task.TaskTypeRecordMaintenanceHold
	})
}

// ResolveMaintenance keeps the always-present MaintenanceInProgress condition
// current from the requested hold and the hold in effect. The reconciler calls
// it on every path, before the Failed and Paused early returns.
func ResolveMaintenance(node *seiv1alpha1.SeiNode) {
	if node.Spec.NodeConfig == nil {
		setMaintenanceCondition(node, metav1.ConditionFalse, seiv1alpha1.ReasonMaintenanceNotApplicable,
			"spec.nodeConfig is not set; the maintenance hold applies only to ConfigMap-configured nodes")
		return
	}
	want, have := node.Spec.HoldRequested(), node.Status.MaintenanceHold
	switch {
	case want == "" && have == "":
		setMaintenanceCondition(node, metav1.ConditionFalse, seiv1alpha1.ReasonNotHeld, "no maintenance hold")
	case want != "" && want != have:
		setMaintenanceCondition(node, metav1.ConditionTrue, seiv1alpha1.ReasonHoldPending,
			fmt.Sprintf("hold %s requested; in effect: %s", want, holdOrNone(have)))
	default:
		message := fmt.Sprintf("hold %s in effect", have)
		if want == "" {
			message += "; release pending"
		}
		reason := seiv1alpha1.ReasonHeld
		if have == seiv1alpha1.MaintenanceHoldAfterExit {
			reason = seiv1alpha1.ReasonArmed
		}
		setMaintenanceCondition(node, metav1.ConditionTrue, reason, message)
	}
}

func holdOrNone(h seiv1alpha1.MaintenanceHold) string {
	if h == "" {
		return "none"
	}
	return string(h)
}

func setMaintenanceCondition(node *seiv1alpha1.SeiNode, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               seiv1alpha1.ConditionMaintenanceInProgress,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: node.Generation,
	})
}
