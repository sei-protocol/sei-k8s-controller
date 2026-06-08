// Package planner builds and executes ordered task plans that drive SeiNode
// and SeiNodeDeployment resources through their lifecycle.
//
// # Plan Lifecycle
//
// A plan is an ordered list of tasks stored in .status.plan on the owning
// resource. The lifecycle is:
//
//  1. Build: ResolvePlan (for nodes) or ForGroup (for deployments) inspects the
//     resource's current phase and spec, then builds an appropriate plan.
//  2. Persist: The controller flushes the plan into the resource's status.
//     Execution does not start until the plan is persisted (atomic creation).
//  3. Execute: Executor.ExecutePlan drives tasks in-memory. Synchronous tasks
//     advance in a loop; async tasks return Running for the next reconcile.
//     The controller flushes all mutations in a single status patch.
//  4. Complete: When all tasks finish, the executor marks the plan Complete and
//     sets TargetPhase. On the next reconcile, the planner observes the terminal
//     plan, updates conditions, and builds the next plan if needed.
//  5. Fail: If a task fails terminally, the executor marks the plan Failed and
//     sets FailedPhase. On the next reconcile, the planner observes the failure,
//     updates conditions, and decides whether to retry.
//
// # Condition Ownership
//
// The planner owns all condition management on the owning resource. It sets
// conditions when creating plans and when observing terminal plans. The executor
// does not set conditions — it only mutates plan/task state and phase transitions.
//
// # Type Roles
//
// TaskPlan: the structure this package owns. Constructed by the plan builders
//
//	(buildBasePlan, buildNodeUpdatePlan, buildMarkReadyPlan, the group
//	builders); persisted by the controller into .status.plan; read by the
//	planner on subsequent reconciles; mutated in-memory by Executor. Carries
//	Phase, the ordered Tasks, TargetPhase, FailedPhase, and on failure
//	FailedTaskIndex/FailedTaskDetail.
//
// PlannedTask: one step of a TaskPlan. Built by buildPlannedTask with a
//
//	deterministic ID (task.DeterministicTaskID), serialized params, and a
//	per-type retry budget (taskMaxRetries). Lives inside TaskPlan.Tasks in
//	.status; advanced in-memory by advanceTask.
//
// NodeResolver / NodePlanner / GroupPlanner: the builders. NodeResolver.ResolvePlan
//
//	is the node entry point; ForGroup selects a GroupPlanner. Stateless apart
//	from the optional BuildSidecarClient factory (nil in tests to skip the probe).
//
// Executor[T client.Object]: drives a persisted plan. Stateless per reconcile —
//
//	re-deserializes the current task each call. Mutates only plan/task state
//	and, for *SeiNode, .Status.Phase. It never writes to the cluster and never
//	persists status; the controller owns the single flushing patch.
//
// # Invariants
//
// Assertions that must always hold. Each names its guarding test, or is marked
// unguarded.
//
//   - Atomic plan creation: a newly built plan is persisted before any task
//     executes. The controller requeues immediately after the creating
//     reconcile (planAlreadyActive gate in controller/node/controller.go);
//     execution begins on the next reconcile, so external observers see the
//     plan before any side effect. Guarded for persistence by
//     TestReconcile_CreatesPlanOnFirstRun; the persist-BEFORE-execute ordering
//     (no task submitted on the creating reconcile) is documented but unguarded.
//   - Single-patch model: each reconcile snapshots obj.DeepCopy() once,
//     accumulates all status mutations (plan state, conditions, phase,
//     currentImage) in-memory, and flushes one Status().Patch at the end. The
//     executor and the planner mutate only the in-memory object. Enforced at the
//     call site (controller/node/controller.go); patch-count-per-path is
//     documented but unguarded.
//   - Optimistic-lock writes: every .status patch base is built with
//     client.MergeFromWithOptimisticLock{} so a stale reconcile cannot silently
//     clobber a fresher one and corrupt plan-creation idempotency. Enforced at
//     the call site; the stale-write race itself is unguarded (would require an
//     envtest concurrency harness).
//   - FailedPhase == "" means retry, not terminal: a NodeUpdate plan leaves
//     FailedPhase empty, so on failure setTargetPhase is a no-op and the node
//     stays Running; the planner rebuilds the plan on the next reconcile. An
//     Init plan sets FailedPhase=Failed and transitions out. The empty value is
//     guarded by node_update_test.go; the resulting retry-not-terminal phase
//     behavior (node remains Running after a NodeUpdate failure) is documented
//     but unguarded.
//   - nil plan == steady state: when no drift is detected for a Running node,
//     buildRunningPlan returns nil and .status.plan stays nil. Absence of a plan
//     is the steady state, not an error. Guarded by
//     TestBuildRunningPlan_SidecarReady_NoPlan / _SidecarUnknown_NoPlan.
//   - Terminal-plan cleanup: when the planner observes a Complete or Failed
//     plan it updates conditions and nils .status.plan; an Active plan is left
//     untouched. Guarded by the TestHandleTerminalPlan_* tests.
//   - Conditions are always-present: condition state is expressed as
//     True/False/Unknown with a stable Reason, never by removal (see
//     CLAUDE.md "Conditions"). isConditionTrue asserts on Status, not presence.
//
// # Zero-Value & Sentinel Semantics
//
//   - .status.plan == nil: no active plan — steady state for a Running node, or
//     a resource that has not yet been resolved. Not an error.
//   - TaskPlan.FailedPhase == "": failures retry rather than transitioning the
//     node to a terminal phase. A non-empty FailedPhase (e.g. PhaseFailed on Init
//     plans) is the terminal target.
//   - TaskPlan.TargetPhase == "": no phase transition on completion. Group plans
//     (SeiNodeDeployment) leave it empty; setTargetPhase is a no-op for any
//     object that is not a *SeiNode, so only SeiNode plans drive .Status.Phase.
//   - PlannedTask.SubmittedAt == nil: the task has not been submitted yet (or was
//     reset for retry). Set once on first submission; used as the plan-duration
//     start. Cleared on retry so resubmission re-stamps it.
//   - PlannedTask.MaxRetries == 0: the first ExecutionFailed is terminal.
//   - FailedTaskIndex / FailedTaskDetail == nil: set only on a Failed plan;
//     nil on Active/Complete plans.
//
// # Concurrency & Staleness
//
// Reconciles for the same object run serially within the controller, but two
// near-simultaneous reconciles (e.g. across a leader change or a watch + requeue)
// can both observe .status.plan == nil and both build a plan. To prevent the
// second from silently overwriting the first and corrupting plan-creation
// idempotency, all status writes use a base built with
// client.MergeFromWithOptimisticLock{} (resourceVersion-checked); a stale patch
// is rejected and that reconcile retries. The planner and executor perform no
// I/O against status themselves — they mutate the in-memory object only, and the
// controller flushes exactly one optimistic-lock-protected Status().Patch per
// reconcile (the single-patch model). See sei-k8s-controller/CLAUDE.md
// "Status patches" and "Single-patch model" for the repo-wide rule.
//
// # Plan Types
//
// Init plans transition a node from Pending to Running. They include
// infrastructure tasks (ensure-data-pvc, apply-statefulset, apply-service)
// followed by sidecar tasks (configure-genesis, config-apply, etc.).
// Init plans set both TargetPhase (Running) and FailedPhase (Failed).
//
// NodeUpdate plans roll out image changes on Running nodes. They are built
// when spec.image != status.currentImage (drift detection). The task sequence
// is: apply-statefulset, apply-service, replace-pod, observe-image, mark-ready.
// replace-pod proactively deletes pods at the old StatefulSet revision so the
// rollout proceeds even when seid is intentionally unready (e.g. halted at a
// chain upgrade height). The planner sets NodeUpdateInProgress=True on
// creation and clears it when the plan completes or fails. FailedPhase is
// empty — failures retry on the next reconcile rather than transitioning to
// Failed.
//
// When no drift is detected for a Running node, no plan is built. The node
// sits in steady state with no active plan.
//
// # Task Types
//
// Sidecar tasks are submitted to the sidecar HTTP API and polled for
// completion. Controller-side tasks (ensure-data-pvc, apply-statefulset,
// apply-service, observe-image, deploy-bootstrap-job, etc.) execute inline
// against the Kubernetes API.
package planner
