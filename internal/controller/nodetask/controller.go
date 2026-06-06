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
	appsv1 "k8s.io/api/apps/v1"
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

	sidecar "github.com/sei-protocol/seictl/sidecar/client"

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
	// stamping anything. UnsupportedKind is reserved for a genuinely unwired
	// kind (the default case); a payload/param-build failure for a wired kind
	// surfaces as ParamsBuildFailed, matching driveTask.
	if fatal == nil && cr.Status.Task == nil {
		if _, _, err := taskParamsForKind(cr, nil); err != nil {
			reason := "ParamsBuildFailed"
			if errors.Is(err, errUnsupportedKind) {
				reason = "UnsupportedKind"
			}
			r.markFailed(cr, now, reason, err.Error())
		} else {
			exec := &seiv1alpha1.SeiNodeTaskExecution{
				ID:     task.DeterministicTaskID(string(cr.UID), string(cr.Spec.Kind), 0),
				Status: seiv1alpha1.TaskPending,
			}
			// Content-address the restart: capture the target pod's UID once,
			// at synthesis, so the restart-pod task deletes exactly that pod
			// and completes on a different (replacement) UID. Keying on UID
			// avoids the same-second creationTimestamp race a clock epoch has.
			if cr.Spec.Kind == seiv1alpha1.SeiNodeTaskKindRestartPod {
				uid, err := r.targetPodUID(ctx, target)
				if err != nil {
					return r.flush(ctx, cr, before, statusBase, observedPhase, ctrl.Result{}, err)
				}
				exec.RestartedPodUID = string(uid)
			}
			cr.Status.Task = exec
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

// targetPodUID returns the UID of the pod currently owned by the target's
// StatefulSet, or "" when the StatefulSet or pod does not exist yet (the
// StatefulSet may still be scheduling). A missing StatefulSet is not an error:
// an empty UID means "nothing to re-delete", and the restart-pod task completes
// on the first owned Ready pod. Only a real API error is returned (caller
// requeues — we must not synthesize the task without resolving the UID).
func (r *SeiNodeTaskReconciler) targetPodUID(ctx context.Context, target *seiv1alpha1.SeiNode) (types.UID, error) {
	sts := &appsv1.StatefulSet{}
	key := types.NamespacedName{Name: target.Name, Namespace: target.Namespace}
	if err := r.Get(ctx, key, sts); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("getting statefulset %q for restart-pod synthesis: %w", target.Name, err)
	}
	if sts.Spec.Selector == nil || len(sts.Spec.Selector.MatchLabels) == 0 {
		return "", nil
	}

	pods := &corev1.PodList{}
	if err := r.List(ctx, pods,
		client.InNamespace(target.Namespace),
		client.MatchingLabels(sts.Spec.Selector.MatchLabels),
	); err != nil {
		return "", fmt.Errorf("listing pods for statefulset %q: %w", target.Name, err)
	}
	for i := range pods.Items {
		for _, ref := range pods.Items[i].OwnerReferences {
			if ref.Kind == "StatefulSet" && ref.UID == sts.UID {
				return pods.Items[i].UID, nil
			}
		}
	}
	return "", nil
}

// driveTask executes/polls the synthesized task and mutates status fields
// in-memory. Returns a transient error only when the task's Execute
// returned a non-Terminal error (controller-runtime backs off).
func (r *SeiNodeTaskReconciler) driveTask(ctx context.Context, cr *seiv1alpha1.SeiNodeTask, target *seiv1alpha1.SeiNode, now time.Time) error {
	taskType, params, err := taskParamsForKind(cr, target)
	if err != nil {
		r.markFailed(cr, now, "ParamsBuildFailed", err.Error())
		return nil
	}

	cfg := r.ConfigFor(ctx, cr, target)
	exec, err := task.Deserialize(taskType, cr.Status.Task.ID, params, cfg)
	if err != nil {
		r.markFailed(cr, now, "DeserializeFailed", err.Error())
		return nil
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
		// Still running. Honor the effective timeout against StartedAt.
		if timeout := effectiveTimeout(cr); timeout > 0 && cr.Status.StartedAt != nil {
			deadline := cr.Status.StartedAt.Add(timeout)
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

// Per-kind default timeouts applied when spec.timeoutSeconds is 0 (unset).
// These bound kinds whose completion depends on a pod becoming Ready or a quick
// disk write — without a bound, a wedged seid (e.g. DiscoverPeers wrote peers
// that crash-loop the node) would requeue forever with no Failed transition.
// Sidecar-backed gov/await kinds keep the unbounded default: they poll the
// chain and an operator chooses the bound via spec.timeoutSeconds.
const (
	defaultRestartPodTimeout    = 10 * time.Minute
	defaultDiscoverPeersTimeout = 2 * time.Minute
)

// effectiveTimeout returns the timeout that drives a Failed(reason=Timeout)
// transition. An explicit spec.timeoutSeconds always wins; otherwise a per-kind
// default applies. 0 means unbounded.
func effectiveTimeout(cr *seiv1alpha1.SeiNodeTask) time.Duration {
	if cr.Spec.TimeoutSeconds > 0 {
		return time.Duration(cr.Spec.TimeoutSeconds) * time.Second
	}
	switch cr.Spec.Kind {
	case seiv1alpha1.SeiNodeTaskKindRestartPod:
		return defaultRestartPodTimeout
	case seiv1alpha1.SeiNodeTaskKindDiscoverPeers:
		return defaultDiscoverPeersTimeout
	default:
		return 0
	}
}

// taskParamsForKind dispatches CR.spec.kind to the internal/task registry.
// For sidecar-backed kinds we marshal the sidecar TaskBuilder struct
// directly: the registry's sidecarTask[T] helper unmarshals back into the
// same typed struct, then calls ToTaskRequest(). Field names on the
// sidecar structs (PascalCase, no JSON tags) are the wire keys.
//
// target is the resolved SeiNode the task runs against. Pass nil from the
// early-validation path where target hasn't been fetched yet — KeyName
// derivation is deferred to the driveTask call site, which has target.
func taskParamsForKind(cr *seiv1alpha1.SeiNodeTask, target *seiv1alpha1.SeiNode) (taskType string, params json.RawMessage, err error) {
	switch cr.Spec.Kind {
	case seiv1alpha1.SeiNodeTaskKindUpdateNodeImage:
		if cr.Spec.UpdateNodeImage == nil {
			return "", nil, errors.New("spec.updateNodeImage is required for kind=UpdateNodeImage")
		}
		return marshalTaskParams(task.TaskTypeUpdateNodeImage, task.UpdateNodeImageParams{
			NodeName:  cr.Spec.Target.NodeRef.Name,
			Namespace: cr.Namespace,
			Image:     cr.Spec.UpdateNodeImage.Image,
		})

	case seiv1alpha1.SeiNodeTaskKindGovVote:
		p := cr.Spec.GovVote
		if p == nil {
			return "", nil, errors.New("spec.govVote is required for kind=GovVote")
		}
		return marshalTaskParams(sidecar.TaskTypeGovVote, sidecar.GovVoteTask{
			ChainID:    p.ChainID,
			KeyName:    resolveSigningUID(p.KeyName, target),
			ProposalID: p.ProposalID,
			Option:     p.Option,
			Memo:       p.Memo,
			Fees:       p.Fees,
			Gas:        p.Gas,
		})

	case seiv1alpha1.SeiNodeTaskKindGovSoftwareUpgrade:
		p := cr.Spec.GovSoftwareUpgrade
		if p == nil {
			return "", nil, errors.New("spec.govSoftwareUpgrade is required for kind=GovSoftwareUpgrade")
		}
		return marshalTaskParams(sidecar.TaskTypeGovSoftwareUpgrade, sidecar.GovSoftwareUpgradeTask{
			ChainID:        p.ChainID,
			KeyName:        resolveSigningUID(p.KeyName, target),
			Title:          p.Title,
			Description:    p.Description,
			UpgradeName:    p.UpgradeName,
			UpgradeHeight:  p.UpgradeHeight,
			UpgradeInfo:    p.UpgradeInfo,
			InitialDeposit: p.InitialDeposit,
			Memo:           p.Memo,
			Fees:           p.Fees,
			Gas:            p.Gas,
		})

	case seiv1alpha1.SeiNodeTaskKindAwaitCondition:
		p := cr.Spec.AwaitCondition
		if p == nil {
			return "", nil, errors.New("spec.awaitCondition is required for kind=AwaitCondition")
		}
		if p.Height == nil {
			return "", nil, errors.New("spec.awaitCondition.height is required (height is the only condition wired in MVP)")
		}
		return marshalTaskParams(sidecar.TaskTypeAwaitCondition, sidecar.AwaitConditionTask{
			Condition:    sidecar.ConditionHeight,
			TargetHeight: p.Height.TargetHeight,
			Action:       p.Action,
		})

	case seiv1alpha1.SeiNodeTaskKindDiscoverPeers:
		if cr.Spec.DiscoverPeers == nil {
			return "", nil, errors.New("spec.discoverPeers is required for kind=DiscoverPeers")
		}
		// Re-resolve the target's current spec.peers into sidecar peer sources.
		// target is nil on the early-validation path (before the SeiNode is
		// fetched); defer source-building to the driveTask call site, which
		// has the target. The empty-peers check also runs there.
		if target == nil {
			return sidecar.TaskTypeDiscoverPeers, nil, nil
		}
		sources := discoverPeerSources(target)
		if len(sources) == 0 {
			return "", nil, fmt.Errorf("target SeiNode %q has no spec.peers to discover", cr.Spec.Target.NodeRef.Name)
		}
		return marshalTaskParams(sidecar.TaskTypeDiscoverPeers, sidecar.DiscoverPeersTask{Sources: sources})

	case seiv1alpha1.SeiNodeTaskKindRestartPod:
		if cr.Spec.RestartPod == nil {
			return "", nil, errors.New("spec.restartPod is required for kind=RestartPod")
		}
		// RestartedPodUID is the content-addressed restart signal, captured
		// once at synthesis (controller.targetPodUID) and persisted on
		// status.task. Empty is valid: no pod existed at synthesis, so the
		// task completes on the first owned Ready pod. On the nil-target
		// early-validation path status.task is nil — pass through empty; the
		// real value is threaded at the driveTask call site.
		var podUID types.UID
		if cr.Status.Task != nil {
			podUID = types.UID(cr.Status.Task.RestartedPodUID)
		}
		return marshalTaskParams(task.TaskTypeRestartPod, task.RestartPodParams{
			NodeName:        cr.Spec.Target.NodeRef.Name,
			Namespace:       cr.Namespace,
			RestartedPodUID: podUID,
		})

	case seiv1alpha1.SeiNodeTaskKindAwaitNodesAtHeight:
		// Single-node target: map to the sidecar's per-node primitive
		// (await-condition height=H) rather than the deployment-scoped
		// controller-side awaitNodesAtHeight task. This keeps the CR's
		// status.task.id equal to the sidecar task ID and avoids the
		// fan-out fixture for a fan-of-one.
		p := cr.Spec.AwaitNodesAtHeight
		if p == nil {
			return "", nil, errors.New("spec.awaitNodesAtHeight is required for kind=AwaitNodesAtHeight")
		}
		return marshalTaskParams(sidecar.TaskTypeAwaitCondition, sidecar.AwaitConditionTask{
			Condition:    sidecar.ConditionHeight,
			TargetHeight: p.TargetHeight,
		})

	default:
		return "", nil, fmt.Errorf("%w: kind %q is not wired in this build", errUnsupportedKind, cr.Spec.Kind)
	}
}

// errUnsupportedKind sentinels the default case in taskParamsForKind: a kind
// the CRD enum admits but this build does not wire. The synthesis site routes
// it to reason=UnsupportedKind; every other (payload/param) failure is
// ParamsBuildFailed. Reasons are a public enum (CLAUDE.md), so UnsupportedKind
// must mean only that.
var errUnsupportedKind = errors.New("unsupported kind")

// discoverPeerSources translates the target SeiNode's spec.peers into sidecar
// peer-discovery sources. Mirrors planner.discoverPeersTask — duplicated rather
// than imported because the nodetask controller deliberately does not depend on
// the planner package. Label sources resolve to the controller-composed
// `<node_id>@<host>:<port>` entries already in target.status.resolvedPeers and
// are routed as static so the sidecar writes them verbatim. Returns an empty
// slice when the target declares no peers (caller fails the task).
func discoverPeerSources(target *seiv1alpha1.SeiNode) []sidecar.PeerSource {
	var sources []sidecar.PeerSource
	for _, s := range target.Spec.Peers {
		switch {
		case s.EC2Tags != nil:
			sources = append(sources, sidecar.PeerSource{
				Type:   sidecar.PeerSourceEC2Tags,
				Region: s.EC2Tags.Region,
				Tags:   s.EC2Tags.Tags,
			})
		case s.Static != nil:
			sources = append(sources, sidecar.PeerSource{
				Type:      sidecar.PeerSourceStatic,
				Addresses: s.Static.Addresses,
			})
		case s.Label != nil:
			// A label source resolves to controller-composed entries in
			// status.resolvedPeers. With none resolved yet, appending a
			// zero-address static source would mask the empty-peers fail-fast
			// (len(sources) would be >0) and submit a discover-peers that
			// silently wipes persistent-peers. Skip it; the caller re-checks
			// len(sources)==0 and fails fast.
			if len(target.Status.ResolvedPeers) == 0 {
				continue
			}
			sources = append(sources, sidecar.PeerSource{
				Type:      sidecar.PeerSourceStatic,
				Addresses: target.Status.ResolvedPeers,
			})
		}
	}
	return sources
}

// resolveSigningUID returns explicit when set; otherwise derives from target
// via seiv1alpha1.ResolveOperatorKeyringUID. With nil target (early-
// validation path), returns the empty string — the final marshal happens
// later from driveTask, which has target.
func resolveSigningUID(explicit string, target *seiv1alpha1.SeiNode) string {
	if explicit != "" {
		return explicit
	}
	if target == nil {
		return ""
	}
	return seiv1alpha1.ResolveOperatorKeyringUID(target)
}

func marshalTaskParams(taskType string, v any) (string, json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", nil, fmt.Errorf("marshaling %s params: %w", taskType, err)
	}
	return taskType, raw, nil
}

// populateOutputs stamps the typed per-kind outputs on Complete.
//
// Sidecar-backed kinds (GovVote, GovSoftwareUpgrade, AwaitCondition,
// AwaitNodesAtHeight) intentionally do NOT populate status.outputs in this
// PR. The sidecar's TaskResult shape carries the values, but extracting
// them would require structural changes on the sidecar side (typed
// per-task result payloads). We defer that work and leave the typed
// output fields on the CRD unset and forward-compatible. Downstream
// consumers coordinate via chain queries (chain-as-medium), not
// task-to-task currying. See conversation history / PR 3 scope notes.
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
