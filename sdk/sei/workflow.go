package sei

import (
	"context"
	"strings"
	"time"
)

// Workflow support. A SeiNodeTaskWorkflow is a named, multi-step recipe against
// a single SeiNode — the composition surface SeiNodeTask deliberately omits. The
// SDK exposes it as thin typed CRUD (CreateWorkflow / GetWorkflow / WaitTerminal
// / Delete), mirroring the task surface: an SDK-native spec, translated to the
// CRD in provider/k8s so core stays apimachinery-free.
//
// Unlike a task, a workflow has no controller of its own — the SeiNode controller
// adopts and drives it. That is invisible here: a caller creates the request and
// waits for its terminal phase, exactly as with a task.

// Workflow recipe kinds the SDK exposes. The value strings match the CRD enum
// exactly. Only StateSync exists today; a new recipe arm is added here when a
// suite needs it.
const (
	WorkflowStateSync = "StateSync"
)

// Workflow phase values, matching the CRD status.phase enum. Phase() returns one
// of these; WaitTerminal blocks until Complete (nil) or Failed (error).
const (
	WorkflowPhasePending  = "Pending"
	WorkflowPhaseRunning  = "Running"
	WorkflowPhaseComplete = "Complete"
	WorkflowPhaseFailed   = "Failed"
)

// WorkflowSpec is the typed input to CreateWorkflow — one SeiNodeTaskWorkflow
// against one target SeiNode in the same namespace. Exactly one recipe payload
// must be set and it must match Kind; CreateWorkflow validates this before apply.
type WorkflowSpec struct {
	Name      string // metadata.name
	Namespace string // "" => client default (kubeconfig context / SA namespace)
	Node      string // target SeiNode name, same namespace -> spec.target.nodeRef.name
	Kind      string // -> spec.kind; one of the Workflow* recipe consts

	// Labels are extra labels on the workflow object — e.g. a caller run-id/GC
	// selector (workflows are not children of any provisioned resource, so a
	// sweep reaps them by label the same way it reaps tasks).
	Labels map[string]string

	// RequirePhase is the SeiNode phase the target must be in before adoption;
	// "" leaves the CRD default (Running). Exact-equality gate, not a floor.
	// -> spec.target.requirePhase.
	RequirePhase string
	// RequirePhaseTimeout bounds the requirePhase wait; 0 leaves the CRD default
	// (5m). -> spec.target.requirePhaseTimeout.
	RequirePhaseTimeout time.Duration

	// Exactly one recipe payload, matching Kind.
	StateSync *StateSyncWorkflow
}

// StateSyncWorkflow is the payload for WorkflowStateSync — re-bootstrap the
// target through CometBFT state sync (quiesce -> wipe data/ -> re-configure ->
// resync -> await caught-up). The recipe is a paved road; its step ordering and
// guardrails live in the controller, not here.
type StateSyncWorkflow struct {
	// ConfigPatch is merged into the node's seid config files before the resync,
	// keyed file -> section-or-key -> JSON-encodable value (the config-patch
	// task's wire shape). For the giga-SS migration this carries
	// app.toml [state-store] evm-ss-split = true. Values are marshaled to JSON by
	// the k8s provider, so a bool stays a bool (not the string "true").
	ConfigPatch map[string]map[string]any

	// RpcServers overrides witness resolution. Empty uses the node's resolved
	// state-syncers (the two-witness fail-closed floor holds either way); when
	// set it must itself carry >= 2 entries or the controller refuses the plan.
	RpcServers []string
}

// CreateWorkflow creates a SeiNodeTaskWorkflow and returns a handle immediately —
// it does NOT wait for terminal. Call Workflow.WaitTerminal to block on the
// outcome.
//
// The workflow and its target SeiNode (Node) resolve in the same namespace; set
// Namespace to the target node's namespace (cross-namespace targeting is out of
// scope). Apply-once by name, like a task: re-creating with the same Name is a
// no-op, and the recipe parameters are immutable once created.
func (c *Client) CreateWorkflow(ctx context.Context, spec WorkflowSpec) (*Workflow, error) {
	if err := validateWorkflowSpec(spec); err != nil {
		return nil, err
	}
	h, err := c.provider.CreateWorkflow(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &Workflow{handle: h}, nil
}

// GetWorkflow reads an existing SeiNodeTaskWorkflow into a handle.
func (c *Client) GetWorkflow(ctx context.Context, name, namespace string) (*Workflow, error) {
	h, err := c.provider.GetWorkflow(ctx, name, namespace)
	if err != nil {
		return nil, err
	}
	return &Workflow{handle: h}, nil
}

// Workflow is a handle to a SeiNodeTaskWorkflow.
type Workflow struct{ handle WorkflowHandle }

// Name is the workflow resource name.
func (w *Workflow) Name() string { return w.handle.Name() }

// Namespace is where the workflow lives.
func (w *Workflow) Namespace() string { return w.handle.Namespace() }

// Phase is the last-observed status.phase (one of the WorkflowPhase* consts, or
// "" before the controller first writes status). A point read; WaitTerminal is
// the blocking gate.
func (w *Workflow) Phase() string { return w.handle.Phase() }

// WaitTerminal blocks until the workflow reaches a terminal phase: nil when it
// completes (phase Complete, Ready condition True), or an error carrying the
// recorded failure reason when it fails (phase Failed, Failed condition True).
// The caller's ctx is the only budget — there are no timeout fields;
// sei.IsTimeout(err) reports a deadline. Mirrors WaitReady/WaitCaughtUp.
func (w *Workflow) WaitTerminal(ctx context.Context) error { return w.handle.WaitTerminal(ctx) }

// Delete removes the workflow. Caller-invoked; idempotent (a not-found is
// success). Workflows are not auto-pruned, so a harness deletes them in cleanup
// the same way it deletes networks and nodes.
//
// A workflow whose recipe placed a durable node hold carries a controller
// finalizer: the delete is accepted, but the object lingers Terminating until
// the controller verifies a safe data state (or an operator sets the
// sei.io/force-delete-workflow annotation). WaitTerminal-then-Delete on a
// completed workflow avoids that path entirely.
func (w *Workflow) Delete(ctx context.Context) error { return w.handle.Delete(ctx) }

// Object returns the mode-specific raw resource (k8s: *v1alpha1.SeiNodeTaskWorkflow).
func (w *Workflow) Object() any { return w.handle.Object() }

func validateWorkflowSpec(s WorkflowSpec) error {
	if strings.TrimSpace(s.Name) == "" {
		return usageErr("WorkflowSpec.Name is required")
	}
	if strings.TrimSpace(s.Node) == "" {
		return usageErr("WorkflowSpec.Node is required (the target SeiNode)")
	}
	// Exactly one recipe payload, matching Kind.
	set := 0
	if s.StateSync != nil {
		set++
	}
	if set != 1 {
		return usageErr("WorkflowSpec requires exactly one recipe payload set, got %d", set)
	}
	switch s.Kind {
	case WorkflowStateSync:
		if s.StateSync == nil {
			return usageErr("WorkflowSpec.Kind %q requires StateSync payload", s.Kind)
		}
	default:
		return usageErr("WorkflowSpec.Kind %q is not supported by the SDK", s.Kind)
	}
	return nil
}
