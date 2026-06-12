// Package nodetask reconciles SeiNodeTask resources. Each SeiNodeTask
// drives one synthesized task to a terminal state through the existing
// internal/task TaskExecution machinery. See docs/design/seinode-task-lld.md
// for the reconciler topology.
package nodetask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/metric"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/observability"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const (
	controllerName = "seinodetask"

	// statusPollInterval is the steady-state requeue cadence for a Running
	// task. Spec changes are immediate via GenerationChangedPredicate;
	// owned-SeiNode status changes wake us via the seiNodeTargetHandler.
	statusPollInterval = 15 * time.Second

	// targetWaitInterval is the requeue cadence while waiting for the
	// target SeiNode to reach RequirePhase.
	targetWaitInterval = 5 * time.Second

	defaultRequirePhaseTimeout = 5 * time.Minute

	// sidecarExecuteTimeout bounds the SubmitTask HTTP call. The shared
	// HTTP client has no Timeout (cmd/main.go); without an explicit ctx
	// deadline, a wedged sidecar that TCP-accepts but stalls could pin
	// a reconcile worker until the OS-level socket timeout (~75s).
	sidecarExecuteTimeout = 30 * time.Second

	// sidecarStatusTimeout bounds the GetTask HTTP call. Same reasoning.
	sidecarStatusTimeout = 15 * time.Second

	// Per-kind execution-timeout defaults applied when spec.timeoutSeconds is 0.
	// These bound kinds whose completion depends on seid coming back up or a
	// quick disk write; sidecar-backed gov/await kinds stay unbounded (an
	// operator sets spec.timeoutSeconds). RestartSeid gets a generous 10m: the
	// sidecar SIGTERMs seid (up to ~90s graceful), then polls seid's RPC back up.
	defaultRestartSeidTimeout = 10 * time.Minute
	defaultMarkReadyTimeout   = 2 * time.Minute
)

// resultRequeueImmediate mirrors planner.ResultRequeueImmediate without
// the import (we deliberately do not depend on the planner package).
var resultRequeueImmediate = ctrl.Result{RequeueAfter: 1 * time.Millisecond}

// ExecutionConfigFor builds a task.ExecutionConfig for a given (CR, target)
// pair. Wired by cmd/main.go using the same factories as the SeiNode
// reconciler so sidecar/object-store/platform plumbing is shared.
type ExecutionConfigFor func(ctx context.Context, t *seiv1alpha1.SeiNodeTask, target *seiv1alpha1.SeiNode) task.ExecutionConfig

// SeiNodeTaskReconciler reconciles a SeiNodeTask. Thin adapter that wraps
// internal/task TaskExecution — does NOT reuse planner.Executor. One CR
// equals one synthesized task with a deterministic ID derived from the
// CR's UID + spec.kind.
type SeiNodeTaskReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
	Platform  platform.Config
	ConfigFor ExecutionConfigFor

	// Now is overridable for tests. Defaults to time.Now.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=sei.io,resources=seinodetasks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sei.io,resources=seinodetasks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sei.io,resources=seinodetasks/finalizers,verbs=update
// +kubebuilder:rbac:groups=sei.io,resources=seinodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives the SeiNodeTask through Pending → Running → Complete|Failed.
// All status mutations accumulate in-memory and flush in a single
// optimistic-lock-protected Status().Patch at the end.
func (r *SeiNodeTaskReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	cr := &seiv1alpha1.SeiNodeTask{}
	if err := r.Get(ctx, req.NamespacedName, cr); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Terminal CRs are no-ops.
	if cr.Status.Phase == seiv1alpha1.SeiNodeTaskPhaseComplete ||
		cr.Status.Phase == seiv1alpha1.SeiNodeTaskPhaseFailed {
		emitTaskPhase(cr.Namespace, cr.Name, cr.Status.Phase)
		return ctrl.Result{}, nil
	}

	before := cr.DeepCopy()
	statusBase := client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})
	observedPhase := cr.Status.Phase

	cr.Status.ObservedGeneration = cr.Generation
	now := r.now()
	if cr.Status.StartedAt == nil {
		t := metav1.NewTime(now)
		cr.Status.StartedAt = &t
	}

	// Step 2: resolve target SeiNode and gate on RequirePhase.
	target, waitResult, fatal := r.resolveTarget(ctx, cr, now)
	if fatal != nil {
		r.markFailed(cr, now, "TargetResolveFailed", fatal.Error())
	}
	if fatal == nil && target == nil {
		// Still waiting for target to reach RequirePhase.
		return r.flush(ctx, cr, before, statusBase, observedPhase, waitResult, nil)
	}

	// Step 3+4: synthesize the one-shot task if missing. Validate the
	// kind/payload mapping first so we fail-fast on a malformed spec before
	// stamping anything. The reason travels with the error (task.FailureReason):
	// an unwired kind yields UnsupportedKind, any wired-kind param-build failure
	// yields ParamsBuildFailed — no call-site conditional.
	if fatal == nil && cr.Status.Task == nil {
		if _, _, err := taskParamsForKind(cr, nil); err != nil {
			r.markFailed(cr, now, task.FailureReason(err), err.Error())
		} else {
			execStart := metav1.NewTime(now)
			cr.Status.Task = &seiv1alpha1.SeiNodeTaskExecution{
				ID:                 task.DeterministicTaskID(string(cr.UID), string(cr.Spec.Kind), 0),
				Status:             seiv1alpha1.TaskPending,
				ExecutionStartedAt: &execStart,
			}
			r.setPhase(cr, seiv1alpha1.SeiNodeTaskPhaseRunning, now)
			// Persist synthesis before any side effects — mirrors the
			// "atomic plan creation" pattern from CLAUDE.md.
			return r.flush(ctx, cr, before, statusBase, observedPhase, resultRequeueImmediate, nil)
		}
	}

	// Step 5: drive the task.
	var execResult ctrl.Result
	var execErr error
	if cr.Status.Phase != seiv1alpha1.SeiNodeTaskPhaseFailed && cr.Status.Task != nil {
		execErr = r.driveTask(ctx, cr, target, now)
	}

	if cr.Status.Phase == seiv1alpha1.SeiNodeTaskPhaseRunning {
		execResult.RequeueAfter = statusPollInterval
	}

	if execErr != nil {
		logger.Error(execErr, "task execution failed",
			"kind", cr.Spec.Kind, "task", cr.Status.Task.ID)
	}
	return r.flush(ctx, cr, before, statusBase, observedPhase, execResult, execErr)
}

// resolveTarget loads the target SeiNode and enforces spec.target.requirePhase.
// Returns (target, requeueResult, fatalErr):
//   - target!=nil and fatal==nil: dispatch.
//   - target==nil and fatal==nil: still waiting; requeueResult is set.
//   - fatal!=nil: terminal failure; caller marks Failed.
func (r *SeiNodeTaskReconciler) resolveTarget(ctx context.Context, cr *seiv1alpha1.SeiNodeTask, now time.Time) (*seiv1alpha1.SeiNode, ctrl.Result, error) {
	target := &seiv1alpha1.SeiNode{}
	key := types.NamespacedName{Name: cr.Spec.Target.NodeRef.Name, Namespace: cr.Namespace}
	if err := r.Get(ctx, key, target); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, ctrl.Result{}, fmt.Errorf("target SeiNode %q not found in namespace %q", key.Name, key.Namespace)
		}
		return nil, ctrl.Result{}, err
	}

	requirePhase := cr.Spec.Target.RequirePhase
	if requirePhase == "" {
		requirePhase = seiv1alpha1.PhaseRunning
	}
	if target.Status.Phase == requirePhase {
		cr.Status.TargetFirstObservedAt = nil
		setCondition(cr, metav1.Condition{
			Type:    seiv1alpha1.ConditionSeiNodeTaskTargetReady,
			Status:  metav1.ConditionTrue,
			Reason:  "PhaseMet",
			Message: fmt.Sprintf("target %s is %s", key.Name, requirePhase),
		})
		return target, ctrl.Result{}, nil
	}

	if cr.Status.TargetFirstObservedAt == nil {
		t := metav1.NewTime(now)
		cr.Status.TargetFirstObservedAt = &t
	}
	timeout := defaultRequirePhaseTimeout
	if cr.Spec.Target.RequirePhaseTimeout != nil {
		timeout = cr.Spec.Target.RequirePhaseTimeout.Duration
	}
	if now.Sub(cr.Status.TargetFirstObservedAt.Time) > timeout {
		return nil, ctrl.Result{}, fmt.Errorf("target %q did not reach phase %q within %s (current: %q)",
			key.Name, requirePhase, timeout, target.Status.Phase)
	}
	setCondition(cr, metav1.Condition{
		Type:    seiv1alpha1.ConditionSeiNodeTaskTargetReady,
		Status:  metav1.ConditionFalse,
		Reason:  "PhaseNotMet",
		Message: fmt.Sprintf("waiting for target %s to reach %s (current: %s)", key.Name, requirePhase, target.Status.Phase),
	})
	return nil, ctrl.Result{RequeueAfter: targetWaitInterval}, nil
}

// driveTask executes/polls the synthesized task and mutates status fields
// in-memory. Returns a transient error only when the task's Execute
// returned a non-Terminal error (controller-runtime backs off).
func (r *SeiNodeTaskReconciler) driveTask(ctx context.Context, cr *seiv1alpha1.SeiNodeTask, target *seiv1alpha1.SeiNode, now time.Time) error {
	taskType, params, err := taskParamsForKind(cr, target)
	if err != nil {
		r.markFailed(cr, now, task.FailureReason(err), err.Error())
		return nil
	}

	cfg := r.ConfigFor(ctx, cr, target)
	exec, err := task.Deserialize(taskType, cr.Status.Task.ID, params, cfg)
	if err != nil {
		r.markFailed(cr, now, "DeserializeFailed", err.Error())
		return nil
	}

	// Backward-compat: a task synthesized by a pre-ExecutionStartedAt controller
	// has a populated status.task but a nil anchor. Lazy-stamp it to now so the
	// timeout branch applies; this gives a bounded fresh budget from the upgrade
	// moment rather than running unbounded. Anchoring to status.startedAt could
	// put the deadline in the past for a long-running task and spuriously fail it.
	if cr.Status.Task.ExecutionStartedAt == nil {
		t := metav1.NewTime(now)
		cr.Status.Task.ExecutionStartedAt = &t
	}

	if cr.Status.Task.Status == seiv1alpha1.TaskPending {
		execCtx, cancel := context.WithTimeout(ctx, sidecarExecuteTimeout)
		err := exec.Execute(execCtx)
		cancel()
		if err != nil {
			var terminal *task.TerminalError
			if errors.As(err, &terminal) {
				cr.Status.Task.Status = seiv1alpha1.TaskFailed
				cr.Status.Task.Err = terminal.Err.Error()
				r.markFailed(cr, now, "TaskTerminalError", terminal.Err.Error())
				return nil
			}
			return err
		}
		if cr.Status.Task.SubmittedAt == nil {
			t := metav1.NewTime(now)
			cr.Status.Task.SubmittedAt = &t
		}
	}

	statusCtx, cancel := context.WithTimeout(ctx, sidecarStatusTimeout)
	defer cancel()
	switch exec.Status(statusCtx) {
	case task.ExecutionComplete:
		cr.Status.Task.Status = seiv1alpha1.TaskComplete
		populateOutputs(cr, target)
		r.setPhase(cr, seiv1alpha1.SeiNodeTaskPhaseComplete, now)
		setCondition(cr, metav1.Condition{
			Type:   seiv1alpha1.ConditionSeiNodeTaskReady,
			Status: metav1.ConditionTrue, Reason: "TaskComplete",
		})
	case task.ExecutionFailed:
		msg := ""
		if e := exec.Err(); e != nil {
			msg = e.Error()
		}
		cr.Status.Task.Status = seiv1alpha1.TaskFailed
		cr.Status.Task.Err = msg
		r.markFailed(cr, now, "TaskFailed", msg)
	default:
		// Still running; enforce the execution timeout against
		// ExecutionStartedAt (not status.startedAt — keeps the requirePhase wait
		// off the budget).
		if timeout := effectiveTimeout(cr); timeout > 0 && cr.Status.Task.ExecutionStartedAt != nil {
			deadline := cr.Status.Task.ExecutionStartedAt.Add(timeout)
			if now.After(deadline) {
				r.markFailed(cr, now, "Timeout",
					fmt.Sprintf("task did not complete within %s", timeout))
				cr.Status.Task.Status = seiv1alpha1.TaskFailed
				cr.Status.Task.Err = "timeout"
			}
		}
	}
	return nil
}

// effectiveTimeout returns the execution timeout (measured from
// status.task.executionStartedAt) that drives a Failed(reason=Timeout)
// transition. An explicit spec.timeoutSeconds wins; otherwise a per-kind
// default applies. 0 means unbounded.
func effectiveTimeout(cr *seiv1alpha1.SeiNodeTask) time.Duration {
	if cr.Spec.TimeoutSeconds > 0 {
		return time.Duration(cr.Spec.TimeoutSeconds) * time.Second
	}
	switch cr.Spec.Kind {
	case seiv1alpha1.SeiNodeTaskKindRestartSeid:
		return defaultRestartSeidTimeout
	case seiv1alpha1.SeiNodeTaskKindMarkReady:
		return defaultMarkReadyTimeout
	default:
		return 0
	}
}

// taskParamsForKind resolves CR.spec.kind to a task type and serialized
// payload. It is a thin dispatcher: the per-kind constructors live in the task
// package (task.SeiNodeTaskParamsFor). target is the resolved SeiNode; pass nil
// from the early-validation path (before the SeiNode is fetched). A nil payload
// marshals to nil params.
func taskParamsForKind(cr *seiv1alpha1.SeiNodeTask, target *seiv1alpha1.SeiNode) (taskType string, params json.RawMessage, err error) {
	p, err := task.SeiNodeTaskParamsFor(cr, target)
	if err != nil {
		return "", nil, err
	}
	if p.Payload == nil {
		return p.Type, nil, nil
	}
	raw, err := json.Marshal(p.Payload)
	if err != nil {
		return "", nil, fmt.Errorf("marshaling %s params: %w", p.Type, err)
	}
	return p.Type, raw, nil
}

// populateOutputs stamps the typed per-kind outputs on Complete.
//
// Sidecar-backed kinds (GovVote, GovSoftwareUpgrade, AwaitCondition,
// AwaitNodesAtHeight) leave their typed output fields unset for now: surfacing
// the values the sidecar's TaskResult carries needs typed per-task result
// payloads on the sidecar side, which is deferred. The CRD fields stay
// forward-compatible, and downstream consumers coordinate via chain queries
// (chain-as-medium) rather than task-to-task currying.
func populateOutputs(cr *seiv1alpha1.SeiNodeTask, target *seiv1alpha1.SeiNode) {
	switch cr.Spec.Kind {
	case seiv1alpha1.SeiNodeTaskKindUpdateNodeImage:
		if cr.Status.Outputs == nil {
			cr.Status.Outputs = &seiv1alpha1.SeiNodeTaskOutputs{}
		}
		cr.Status.Outputs.UpdateNodeImage = &seiv1alpha1.UpdateNodeImageOutputs{
			AppliedImage: target.Status.CurrentImage,
		}
	}
}

func (r *SeiNodeTaskReconciler) setPhase(cr *seiv1alpha1.SeiNodeTask, phase seiv1alpha1.SeiNodeTaskPhase, now time.Time) {
	if cr.Status.Phase == phase {
		return
	}
	cr.Status.Phase = phase
	t := metav1.NewTime(now)
	cr.Status.PhaseTransitionTime = &t
}

func (r *SeiNodeTaskReconciler) markFailed(cr *seiv1alpha1.SeiNodeTask, now time.Time, reason, msg string) {
	r.setPhase(cr, seiv1alpha1.SeiNodeTaskPhaseFailed, now)
	setCondition(cr, metav1.Condition{
		Type:    seiv1alpha1.ConditionSeiNodeTaskFailed,
		Status:  metav1.ConditionTrue,
		Reason:  reason,
		Message: msg,
	})
	if r.Recorder != nil {
		r.Recorder.Event(cr, corev1.EventTypeWarning, reason, msg)
	}
}

// setCondition is the single condition-write helper for this controller. It
// stamps ObservedGeneration = cr.Generation per the CLAUDE.md discipline (so
// consumers can tell whether a condition reflects the current spec) before
// delegating to apimeta.SetStatusCondition, which handles lastTransitionTime
// and idempotent upserts. All condition writes in this file route through here.
func setCondition(cr *seiv1alpha1.SeiNodeTask, cond metav1.Condition) {
	cond.ObservedGeneration = cr.Generation
	apimeta.SetStatusCondition(&cr.Status.Conditions, cond)
}

func (r *SeiNodeTaskReconciler) flush(
	ctx context.Context, cr, before *seiv1alpha1.SeiNodeTask,
	statusBase client.Patch, observedPhase seiv1alpha1.SeiNodeTaskPhase,
	result ctrl.Result, execErr error,
) (ctrl.Result, error) {
	if !apiequality.Semantic.DeepEqual(before.Status, cr.Status) {
		if err := r.Status().Patch(ctx, cr, statusBase); err != nil {
			if execErr != nil {
				log.FromContext(ctx).Error(execErr, "task execution error lost due to status flush failure")
			}
			return ctrl.Result{}, fmt.Errorf("flushing status: %w", err)
		}
	}
	if cr.Status.Phase != observedPhase {
		emitTaskPhase(cr.Namespace, cr.Name, cr.Status.Phase)
		taskPhaseTransitions.Add(ctx, 1,
			metric.WithAttributes(
				observability.AttrController.String(controllerName),
				observability.AttrNamespace.String(cr.Namespace),
				observability.AttrFromPhase.String(string(observedPhase)),
				observability.AttrToPhase.String(string(cr.Status.Phase)),
			),
		)
	}
	if execErr != nil {
		return result, execErr
	}
	return result, nil
}

func (r *SeiNodeTaskReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// SetupWithManager wires the reconciler. We Watch SeiNodes (not Own) — the
// task doesn't own its target; a target status change is just an event we
// use to wake up.
func (r *SeiNodeTaskReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&seiv1alpha1.SeiNodeTask{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&seiv1alpha1.SeiNode{}, &seiNodeTargetHandler{client: r.Client}).
		Named(controllerName).
		Complete(r)
}
