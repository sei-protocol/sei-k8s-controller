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
//  2. Persist: The controller patches the plan into the resource's status.
//  3. Execute: Executor.ExecutePlan drives one task per reconcile, patching
//     status after each task completes.
//  4. Complete: When all tasks finish, the executor sets TargetPhase on the
//     resource and marks the plan Complete. Convergence plans (where the
//     target phase equals the current phase) are nilled out to avoid stale
//     data in etcd.
//  5. Fail: If a task fails terminally, the executor sets FailedPhase and a
//     PlanFailed condition on the resource for operator observability.
//
// # Plan Types
//
// Init plans transition a node from Pending to Running. They include
// infrastructure tasks (ensure-data-pvc, apply-statefulset, apply-service)
// followed by sidecar tasks (configure-genesis, config-apply, etc.).
// Init plans set both TargetPhase (Running) and FailedPhase (Failed).
//
// Convergence plans keep a Running node's owned resources in sync with the
// spec. They contain only apply-statefulset and apply-service. FailedPhase
// is deliberately empty: a convergence failure retries on the next reconcile
// rather than transitioning the node to Failed.
//
// # Task Types
//
// Sidecar tasks are submitted to the sidecar HTTP API and polled for
// completion. Controller-side tasks (ensure-data-pvc, apply-statefulset,
// apply-service, deploy-bootstrap-job, etc.) execute inline against the
// Kubernetes API.
package planner
