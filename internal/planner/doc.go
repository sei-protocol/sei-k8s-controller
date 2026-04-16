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
// # Plan Types
//
// Init plans transition a node from Pending to Running. They include
// infrastructure tasks (ensure-data-pvc, apply-statefulset, apply-service)
// followed by sidecar tasks (configure-genesis, config-apply, etc.).
// Init plans set both TargetPhase (Running) and FailedPhase (Failed).
//
// NodeUpdate plans roll out image changes on Running nodes. They are built
// when spec.image != status.currentImage (drift detection). The task sequence
// is: apply-statefulset, apply-service, observe-image, mark-ready. The planner
// sets NodeUpdateInProgress=True on creation and clears it when the plan
// completes or fails. FailedPhase is empty — failures retry on the next
// reconcile rather than transitioning to Failed.
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
