package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
)

// taskIDNamespace is a fixed UUID v5 namespace for generating deterministic
// task IDs. The controller seeds this with nodeName/taskType/attempt to
// produce a stable, collision-free ID for each task instance.
var taskIDNamespace = uuid.MustParse("b7e89c3a-4f12-4d8b-9a6e-1c2d3e4f5a6b")

// Controller-managed task types — the sidecar has no handlers for these.
const (
	TaskTypeAwaitGenesisAssembly   = "await-genesis-assembly"
	TaskTypeDeployBootstrapSvc     = "deploy-bootstrap-service"
	TaskTypeDeployBootstrapJob     = "deploy-bootstrap-job"
	TaskTypeAwaitBootstrapComplete = "await-bootstrap-complete"
	TaskTypeTeardownBootstrap      = "teardown-bootstrap"
)

// ExecutionStatus represents the lifecycle state of a task execution.
type ExecutionStatus string

const (
	ExecutionRunning  ExecutionStatus = "Running"
	ExecutionComplete ExecutionStatus = "Complete"
	ExecutionFailed   ExecutionStatus = "Failed"
)

// ErrTaskNotFound is returned by Status when the sidecar has no record of
// the task. The executor uses this sentinel to distinguish "not yet submitted"
// from "submitted and running".
var ErrTaskNotFound = errors.New("task not found on sidecar")

// TaskExecution defines how the plan executor drives a single task.
// Execute is a command (called once to submit). Status is a query
// (called on every reconcile to poll progress). Err returns details
// when Status reports ExecutionFailed.
type TaskExecution interface {
	Execute(ctx context.Context) error
	Status(ctx context.Context) ExecutionStatus
	Err() error
}

// UnknownTaskTypeError is returned by Deserialize for unrecognized task types.
// The executor should treat this as a permanent failure.
type UnknownTaskTypeError struct {
	Type string
}

func (e *UnknownTaskTypeError) Error() string {
	return fmt.Sprintf("unknown task type %q", e.Type)
}

// DeterministicTaskID generates a UUID v5 from node name, task type, and
// attempt counter. The attempt counter prevents ID collisions when a failed
// plan is rebuilt and the same task type runs again.
func DeterministicTaskID(nodeName, taskType string, attempt int) string {
	seed := fmt.Sprintf("%s/%s/%d", nodeName, taskType, attempt)
	return uuid.NewSHA1(taskIDNamespace, []byte(seed)).String()
}

// SidecarClient abstracts the sidecar HTTP API for task submission and
// status polling. Narrowed from the full seictl client to the two methods
// needed by task execution. Implementations must be safe for concurrent use.
type SidecarClient interface {
	SubmitTask(ctx context.Context, req sidecar.TaskRequest) (uuid.UUID, error)
	GetTask(ctx context.Context, id uuid.UUID) (*sidecar.TaskResult, error)
}

// ExecutionConfig bundles all dependencies needed by task executions:
// external clients, runtime context, and platform configuration. New
// dependencies are added here without changing Deserialize call sites.
type ExecutionConfig struct {
	BuildSidecarClient func() (SidecarClient, error)
	KubeClient         client.Client
	Scheme             *runtime.Scheme
	Node               *seiv1alpha1.SeiNode
	Platform           platform.Config
}

// Deserialize reconstructs a TaskExecution from its serialized CRD
// representation. Dependencies are injected via the ExecutionConfig bundle.
// Returns UnknownTaskTypeError for unrecognized types.
func Deserialize(taskType, id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	buildSC := cfg.BuildSidecarClient
	switch taskType {
	// Bootstrap tasks
	case sidecar.TaskTypeSnapshotRestore:
		return deserializeSidecar[SnapshotRestoreParams](id, params, buildSC, false)
	case sidecar.TaskTypeConfigureStateSync:
		return deserializeSidecar[ConfigureStateSyncParams](id, params, buildSC, false)
	case sidecar.TaskTypeAwaitCondition:
		return deserializeSidecar[AwaitConditionParams](id, params, buildSC, false)

	// Config tasks
	case sidecar.TaskTypeConfigApply:
		return deserializeSidecar[ConfigApplyParams](id, params, buildSC, false)
	case sidecar.TaskTypeConfigValidate:
		return deserializeSidecar[ConfigValidateParams](id, params, buildSC, true)
	case sidecar.TaskTypeConfigureGenesis:
		return deserializeSidecar[ConfigureGenesisParams](id, params, buildSC, false)
	case sidecar.TaskTypeDiscoverPeers:
		return deserializeSidecar[DiscoverPeersParams](id, params, buildSC, false)
	case sidecar.TaskTypeMarkReady:
		return deserializeSidecar[MarkReadyParams](id, params, buildSC, true)

	// Genesis ceremony tasks
	case sidecar.TaskTypeGenerateIdentity:
		return deserializeSidecar[GenerateIdentityParams](id, params, buildSC, false)
	case sidecar.TaskTypeGenerateGentx:
		return deserializeSidecar[GenerateGentxParams](id, params, buildSC, false)
	case sidecar.TaskTypeUploadGenesisArtifacts:
		return deserializeSidecar[UploadGenesisArtifactsParams](id, params, buildSC, false)
	case TaskTypeAwaitGenesisAssembly:
		return deserializeAwaitGenesisAssembly(id, params)

	// Controller-side bootstrap tasks
	case TaskTypeDeployBootstrapSvc:
		return deserializeBootstrapService(id, params, cfg)
	case TaskTypeDeployBootstrapJob:
		return deserializeBootstrapJob(id, params, cfg)
	case TaskTypeAwaitBootstrapComplete:
		return deserializeBootstrapAwait(id, params, cfg)
	case TaskTypeTeardownBootstrap:
		return deserializeBootstrapTeardown(id, params, cfg)

	default:
		return nil, &UnknownTaskTypeError{Type: taskType}
	}
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

func deserializeAwaitGenesisAssembly(id string, params json.RawMessage) (TaskExecution, error) {
	var p AwaitGenesisAssemblyParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing await-genesis-assembly params: %w", err)
		}
	}
	return &awaitGenesisAssemblyExecution{
		id:     id,
		params: p,
		status: ExecutionRunning,
	}, nil
}
