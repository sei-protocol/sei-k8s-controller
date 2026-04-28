package task

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
)

// taskIDNamespace is a fixed UUID v5 namespace for generating deterministic
// task IDs. The controller seeds this with planID/taskType/planIndex to
// produce a stable, collision-free ID for each task instance.
var taskIDNamespace = uuid.MustParse("b7e89c3a-4f12-4d8b-9a6e-1c2d3e4f5a6b")

// Controller-managed task types — the sidecar has no handlers for these.
const (
	TaskTypeDeployBootstrapSvc     = "deploy-bootstrap-service"
	TaskTypeDeployBootstrapJob     = "deploy-bootstrap-job"
	TaskTypeAwaitBootstrapComplete = "await-bootstrap-complete"
	TaskTypeTeardownBootstrap      = "teardown-bootstrap"
)

// fieldOwner is the server-side apply field manager for resources
// owned by the seinode-controller.
var fieldOwner = client.FieldOwner("seinode-controller")

// ExecutionStatus represents the lifecycle state of a task execution.
type ExecutionStatus string

const (
	ExecutionRunning  ExecutionStatus = "Running"
	ExecutionComplete ExecutionStatus = "Complete"
	ExecutionFailed   ExecutionStatus = "Failed"
)

// TaskExecution defines how the plan executor drives a single task.
// Execute is a command (called once to submit — must be idempotent).
// Status is a query (called on every reconcile to poll progress).
// Err returns details when Status reports ExecutionFailed.
//
// Execute error semantics:
//   - Return a plain error for transient failures — the executor retries.
//   - Return Terminal(err) for permanent failures — the executor fails the plan.
//
// Implementations should embed taskBase for terminal-state caching and Err().
type TaskExecution interface {
	Execute(ctx context.Context) error
	Status(ctx context.Context) ExecutionStatus
	Err() error
}

// TerminalError wraps an error to signal that the failure is permanent
// and the task should not be retried. The executor checks for this via
// errors.As to decide whether to retry or fail the plan.
type TerminalError struct {
	Err error
}

func (e *TerminalError) Error() string { return e.Err.Error() }
func (e *TerminalError) Unwrap() error { return e.Err }

// Terminal wraps an error to mark it as non-retryable.
func Terminal(err error) error {
	return &TerminalError{Err: err}
}

// taskBase provides standard lifecycle helpers for TaskExecution
// implementations. Embed it to get terminal-state caching, error
// tracking, and the Err() method.
type taskBase struct {
	id     string
	status ExecutionStatus
	err    error
}

// isTerminal returns the cached status and true if the task has reached
// a terminal state. Call at the top of Status() to short-circuit polling.
func (b *taskBase) isTerminal() (ExecutionStatus, bool) {
	if b.status == ExecutionComplete || b.status == ExecutionFailed {
		return b.status, true
	}
	return "", false
}

// complete marks the task as successfully completed.
func (b *taskBase) complete() {
	b.status = ExecutionComplete
}

// setFailed marks the task as failed with the given error.
// Used by Status() methods to record failures detected during polling.
func (b *taskBase) setFailed(err error) {
	b.status = ExecutionFailed
	b.err = err
}

// DefaultStatus returns the cached status, short-circuiting if terminal.
// Fire-and-forget tasks that complete entirely within Execute can use this
// as their Status implementation directly.
func (b *taskBase) DefaultStatus() ExecutionStatus {
	if s, done := b.isTerminal(); done {
		return s
	}
	return b.status
}

// Err returns the error that caused failure, or nil.
func (b *taskBase) Err() error { return b.err }

// UnknownTaskTypeError is returned by Deserialize for unrecognized task types.
// The executor should treat this as a permanent failure.
type UnknownTaskTypeError struct {
	Type string
}

func (e *UnknownTaskTypeError) Error() string {
	return fmt.Sprintf("unknown task type %q", e.Type)
}

// DeterministicTaskID generates a UUID v5 from plan ID, task type, and
// plan index. Each plan gets a unique planID (UUID v4), so task IDs are
// unique across plan rebuilds while remaining deterministic within a plan.
func DeterministicTaskID(planID, taskType string, planIndex int) string {
	seed := fmt.Sprintf("%s/%s/%d", planID, taskType, planIndex)
	return uuid.NewSHA1(taskIDNamespace, []byte(seed)).String()
}

// SidecarClient abstracts the sidecar HTTP API for task submission and
// status polling. Narrowed from the full seictl client to the two methods
// needed by task execution. Implementations must be safe for concurrent use.
type SidecarClient interface {
	SubmitTask(ctx context.Context, req sidecar.TaskRequest) (uuid.UUID, error)
	GetTask(ctx context.Context, id uuid.UUID) (*sidecar.TaskResult, error)
	Healthz(ctx context.Context) (bool, error)
}

// ExecutionConfig bundles all dependencies needed by task executions:
// external clients, runtime context, and platform configuration. New
// dependencies are added here without changing Deserialize call sites.
//
// Resource is the owning Kubernetes object (SeiNode or SeiNodeDeployment).
// Task executions that need a concrete type should type-assert it.
// Treat as read-only; mutations belong in the reconciler after
// ExecutePlan returns.
type ExecutionConfig struct {
	BuildSidecarClient func() (SidecarClient, error)
	KubeClient         client.Client
	Scheme             *runtime.Scheme
	Resource           client.Object
	Platform           platform.Config
	ObjectStore        platform.ObjectStore
}

// ResourceAs is a generic helper that type-asserts the Resource field.
func ResourceAs[T client.Object](cfg ExecutionConfig) (T, error) {
	r, ok := cfg.Resource.(T)
	if !ok {
		var zero T
		return zero, fmt.Errorf("expected resource type %T, got %T", zero, cfg.Resource)
	}
	return r, nil
}

// taskDeserializer reconstructs a TaskExecution from serialized params.
type taskDeserializer func(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error)

// sidecarTask creates a deserializer for a sidecar-backed task type.
func sidecarTask[T any](fireAndForget bool) taskDeserializer {
	return func(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
		return deserializeSidecar[T](id, params, cfg.BuildSidecarClient, fireAndForget)
	}
}

// registry maps task type strings to their deserializers.
var registry = map[string]taskDeserializer{
	// Sidecar tasks
	sidecar.TaskTypeSnapshotRestore:        sidecarTask[SnapshotRestoreParams](false),
	sidecar.TaskTypeConfigureStateSync:     sidecarTask[ConfigureStateSyncParams](false),
	sidecar.TaskTypeAwaitCondition:         sidecarTask[AwaitConditionParams](false),
	sidecar.TaskTypeConfigApply:            sidecarTask[ConfigApplyParams](false),
	sidecar.TaskTypeConfigValidate:         sidecarTask[ConfigValidateParams](true),
	sidecar.TaskTypeConfigureGenesis:       sidecarTask[ConfigureGenesisParams](false),
	sidecar.TaskTypeDiscoverPeers:          sidecarTask[DiscoverPeersParams](false),
	sidecar.TaskTypeMarkReady:              sidecarTask[MarkReadyParams](true),
	sidecar.TaskTypeSnapshotUpload:         sidecarTask[SnapshotUploadParams](true),
	sidecar.TaskTypeGenerateIdentity:       sidecarTask[GenerateIdentityParams](false),
	sidecar.TaskTypeGenerateGentx:          sidecarTask[GenerateGentxParams](false),
	sidecar.TaskTypeUploadGenesisArtifacts: sidecarTask[UploadGenesisArtifactsParams](false),
	sidecar.TaskTypeAssembleGenesis:        sidecarTask[AssembleAndUploadGenesisParams](false),
	sidecar.TaskTypeSetGenesisPeers:        sidecarTask[SetGenesisPeersParams](false),
	sidecar.TaskTypeAssembleGenesisFork:    sidecarTask[AssembleForkGenesisParams](false),

	// Controller-side group tasks
	TaskTypeAwaitNodesRunning:  deserializeAwaitNodesRunning,
	TaskTypeCollectAndSetPeers: deserializeCollectAndSetPeers,

	// Controller-side infrastructure tasks
	TaskTypeEnsureDataPVC:      deserializeEnsureDataPVC,
	TaskTypeApplyStatefulSet:   deserializeApplyStatefulSet,
	TaskTypeApplyService:       deserializeApplyService,
	TaskTypeObserveImage:       deserializeObserveImage,
	TaskTypeValidateSigningKey: deserializeValidateSigningKey,

	// Controller-side bootstrap tasks
	TaskTypeDeployBootstrapSvc:     deserializeBootstrapService,
	TaskTypeDeployBootstrapJob:     deserializeBootstrapJob,
	TaskTypeAwaitBootstrapComplete: deserializeBootstrapAwait,
	TaskTypeTeardownBootstrap:      deserializeBootstrapTeardown,

	// Controller-side deployment tasks
	TaskTypeUpdateNodeSpecs:    deserializeUpdateNodeSpecs,
	TaskTypeAwaitSpecUpdate:    deserializeAwaitSpecUpdate,
	TaskTypeCreateEntrantNodes: deserializeCreateEntrantNodes,
	TaskTypeSubmitHaltSignal:   deserializeSubmitHaltSignal,
	TaskTypeAwaitNodesAtHeight: deserializeAwaitNodesAtHeight,
	TaskTypeAwaitNodesCaughtUp: deserializeAwaitNodesCaughtUp,
	TaskTypeSwitchTraffic:      deserializeSwitchTraffic,
	TaskTypeTeardownNodes:      deserializeTeardownNodes,

	// Controller-side fork tasks
	TaskTypeCreateExporter:       deserializeCreateExporter,
	TaskTypeAwaitExporterRunning: deserializeAwaitExporterRunning,
	TaskTypeSubmitExportState:    deserializeSubmitExportState,
	TaskTypeTeardownExporter:     deserializeTeardownExporter,
}

// Deserialize reconstructs a TaskExecution from its serialized CRD
// representation. Dependencies are injected via the ExecutionConfig bundle.
// Returns UnknownTaskTypeError for unrecognized types.
func Deserialize(taskType, id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	fn, ok := registry[taskType]
	if !ok {
		return nil, &UnknownTaskTypeError{Type: taskType}
	}
	return fn(id, params, cfg)
}

// deserializeSidecar is a generic helper that unmarshals params into a typed
// struct and wraps it in a sidecarExecution. The sidecar client is built
// lazily on first Execute/Status call via the buildSC factory.
func deserializeSidecar[T any](id string, params json.RawMessage, buildSC func() (SidecarClient, error), fireAndForget bool) (TaskExecution, error) {
	var p T
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing params for task %s: %w", id, err)
		}
	}
	return &sidecarExecution[T]{
		buildSC:       buildSC,
		id:            id,
		params:        p,
		fireAndForget: fireAndForget,
		status:        ExecutionRunning,
	}, nil
}
