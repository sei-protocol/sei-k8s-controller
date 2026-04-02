package task

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// exportStateTimeout is the maximum time the export-state task can run
// before the controller fails the plan. Generous because seid export
// on large chains can take hours.
const exportStateTimeout = 6 * time.Hour

const (
	TaskTypeCreateExporter       = "create-exporter"
	TaskTypeAwaitExporterRunning = "await-exporter-running"
	TaskTypeSubmitExportState    = "submit-export-state"
	TaskTypeTeardownExporter     = "teardown-exporter"
)

// CreateExporterParams holds parameters for creating the temporary
// exporter SeiNode.
type CreateExporterParams struct {
	GroupName     string `json:"groupName"`
	ExporterName  string `json:"exporterName"`
	Namespace     string `json:"namespace"`
	SourceChainID string `json:"sourceChainId"`
	SourceImage   string `json:"sourceImage"`
	ExportHeight  int64  `json:"exportHeight"`
}

type createExporterExecution struct {
	taskBase
	params CreateExporterParams
	cfg    ExecutionConfig
}

func deserializeCreateExporter(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p CreateExporterParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing create-exporter params: %w", err)
		}
	}
	return &createExporterExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

func (e *createExporterExecution) Execute(ctx context.Context) error {
	group, err := ResourceAs[*seiv1alpha1.SeiNodeGroup](e.cfg)
	if err != nil {
		return Terminal(err)
	}

	existing := &seiv1alpha1.SeiNode{}
	err = e.cfg.KubeClient.Get(ctx, types.NamespacedName{
		Name: e.params.ExporterName, Namespace: e.params.Namespace,
	}, existing)
	if err == nil {
		// If a previous attempt left a failed exporter, delete it so we
		// can create a fresh one on the next reconcile.
		if existing.Status.Phase == seiv1alpha1.PhaseFailed {
			if delErr := e.cfg.KubeClient.Delete(ctx, existing); delErr != nil && !apierrors.IsNotFound(delErr) {
				return fmt.Errorf("deleting failed exporter: %w", delErr)
			}
			return nil // next reconcile will re-enter Execute and create a new exporter
		}
		e.complete()
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("checking exporter existence: %w", err)
	}

	// The exporter uses the source chain's binary (SourceImage) paired
	// with the group's sidecar config. The sidecar is chain-agnostic —
	// it only needs to talk to seid's RPC and manage files on disk.
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      e.params.ExporterName,
			Namespace: e.params.Namespace,
			Labels: map[string]string{
				"sei.io/nodegroup": e.params.GroupName,
				"sei.io/role":      "exporter",
			},
		},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: e.params.SourceChainID,
			Image:   e.params.SourceImage,
			Sidecar: group.Spec.Template.Spec.Sidecar,
			Peers:   group.Spec.Template.Spec.Peers,
			FullNode: &seiv1alpha1.FullNodeSpec{
				Snapshot: &seiv1alpha1.SnapshotSource{
					S3: &seiv1alpha1.S3SnapshotSource{
						TargetHeight: e.params.ExportHeight,
					},
					BootstrapImage: e.params.SourceImage,
				},
			},
		},
	}

	if err := ctrl.SetControllerReference(group, node, e.cfg.Scheme); err != nil {
		return Terminal(fmt.Errorf("setting owner reference: %w", err))
	}

	if err := e.cfg.KubeClient.Create(ctx, node); err != nil {
		if apierrors.IsAlreadyExists(err) {
			e.complete()
			return nil
		}
		return fmt.Errorf("creating exporter: %w", err)
	}

	e.complete()
	return nil
}

func (e *createExporterExecution) Status(_ context.Context) ExecutionStatus {
	return e.status
}

// AwaitExporterRunningParams holds parameters for polling the exporter
// SeiNode until it reaches Running phase.
type AwaitExporterRunningParams struct {
	ExporterName string `json:"exporterName"`
	Namespace    string `json:"namespace"`
}

type awaitExporterRunningExecution struct {
	taskBase
	params AwaitExporterRunningParams
	cfg    ExecutionConfig
}

func deserializeAwaitExporterRunning(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p AwaitExporterRunningParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing await-exporter-running params: %w", err)
		}
	}
	return &awaitExporterRunningExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

func (e *awaitExporterRunningExecution) Execute(_ context.Context) error { return nil }

func (e *awaitExporterRunningExecution) Status(ctx context.Context) ExecutionStatus {
	if s, done := e.isTerminal(); done {
		return s
	}
	node := &seiv1alpha1.SeiNode{}
	err := e.cfg.KubeClient.Get(ctx, types.NamespacedName{
		Name: e.params.ExporterName, Namespace: e.params.Namespace,
	}, node)
	if apierrors.IsNotFound(err) {
		e.setFailed(fmt.Errorf("exporter %s not found — create-exporter may have failed", e.params.ExporterName))
		return ExecutionFailed
	}
	if err != nil {
		return ExecutionRunning
	}
	switch node.Status.Phase {
	case seiv1alpha1.PhaseRunning:
		e.complete()
		return ExecutionComplete
	case seiv1alpha1.PhaseFailed:
		e.setFailed(fmt.Errorf("exporter %s failed", e.params.ExporterName))
		return ExecutionFailed
	default:
		return ExecutionRunning
	}
}

// SubmitExportStateParams holds parameters for submitting the export-state
// task to the exporter's sidecar.
type SubmitExportStateParams struct {
	ExporterName  string `json:"exporterName"`
	Namespace     string `json:"namespace"`
	ExportHeight  int64  `json:"exportHeight"`
	SourceChainID string `json:"sourceChainId"`
}

type submitExportStateExecution struct {
	taskBase
	params SubmitExportStateParams
	cfg    ExecutionConfig
}

func deserializeSubmitExportState(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p SubmitExportStateParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing submit-export-state params: %w", err)
		}
	}
	return &submitExportStateExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

// sidecarTaskID returns the deterministic UUID used for the export-state
// submission to the exporter's sidecar. Recomputed on every call so it
// survives re-deserialization across reconcile boundaries.
func (e *submitExportStateExecution) sidecarTaskID() uuid.UUID {
	return uuid.MustParse(DeterministicTaskID(e.params.ExporterName, "export-state", 0))
}

func (e *submitExportStateExecution) Execute(ctx context.Context) error {
	node := &seiv1alpha1.SeiNode{}
	if err := e.cfg.KubeClient.Get(ctx, types.NamespacedName{
		Name: e.params.ExporterName, Namespace: e.params.Namespace,
	}, node); err != nil {
		return fmt.Errorf("getting exporter node: %w", err)
	}

	sc, err := sidecarClientForNode(node)
	if err != nil {
		return fmt.Errorf("building sidecar client for exporter: %w", err)
	}

	taskID := e.sidecarTaskID()
	exportParams := map[string]any{
		"height":   e.params.ExportHeight,
		"chainId":  e.params.SourceChainID,
		"s3Bucket": e.cfg.Platform.GenesisBucket,
		"s3Key":    fmt.Sprintf("%s/exported-state.json", e.params.SourceChainID),
		"s3Region": e.cfg.Platform.GenesisRegion,
	}

	req := sidecar.TaskRequest{
		Id:     &taskID,
		Type:   sidecar.TaskTypeExportState,
		Params: &exportParams,
	}
	if _, err := sc.SubmitTask(ctx, req); err != nil {
		return fmt.Errorf("submitting export-state to exporter: %w", err)
	}

	return nil
}

func (e *submitExportStateExecution) Status(ctx context.Context) ExecutionStatus {
	if s, done := e.isTerminal(); done {
		return s
	}

	node := &seiv1alpha1.SeiNode{}
	if err := e.cfg.KubeClient.Get(ctx, types.NamespacedName{
		Name: e.params.ExporterName, Namespace: e.params.Namespace,
	}, node); err != nil {
		return ExecutionRunning
	}

	// Timeout: fail if the exporter has been alive longer than the limit.
	if !node.CreationTimestamp.IsZero() && time.Since(node.CreationTimestamp.Time) > exportStateTimeout {
		e.setFailed(fmt.Errorf("export-state timed out after %s", exportStateTimeout))
		return ExecutionFailed
	}

	sc, err := sidecarClientForNode(node)
	if err != nil {
		return ExecutionRunning
	}

	taskID := e.sidecarTaskID()
	result, err := sc.GetTask(ctx, taskID)
	if err != nil {
		return ExecutionRunning
	}

	switch result.Status {
	case sidecar.Completed:
		e.complete()
		return ExecutionComplete
	case sidecar.Failed:
		errMsg := "unknown error"
		if result.Error != nil {
			errMsg = *result.Error
		}
		e.setFailed(fmt.Errorf("export-state failed: %s", errMsg))
		return ExecutionFailed
	default:
		return ExecutionRunning
	}
}

// TeardownExporterParams holds parameters for deleting the exporter SeiNode.
type TeardownExporterParams struct {
	ExporterName string `json:"exporterName"`
	Namespace    string `json:"namespace"`
}

type teardownExporterExecution struct {
	taskBase
	params TeardownExporterParams
	cfg    ExecutionConfig
}

func deserializeTeardownExporter(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p TeardownExporterParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing teardown-exporter params: %w", err)
		}
	}
	return &teardownExporterExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

func (e *teardownExporterExecution) Execute(ctx context.Context) error {
	node := &seiv1alpha1.SeiNode{}
	err := e.cfg.KubeClient.Get(ctx, types.NamespacedName{
		Name: e.params.ExporterName, Namespace: e.params.Namespace,
	}, node)
	if apierrors.IsNotFound(err) {
		e.complete()
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting exporter for deletion: %w", err)
	}
	if err := e.cfg.KubeClient.Delete(ctx, node); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting exporter: %w", err)
	}
	return nil
}

func (e *teardownExporterExecution) Status(ctx context.Context) ExecutionStatus {
	if s, done := e.isTerminal(); done {
		return s
	}
	err := e.cfg.KubeClient.Get(ctx, types.NamespacedName{
		Name: e.params.ExporterName, Namespace: e.params.Namespace,
	}, &seiv1alpha1.SeiNode{})
	if apierrors.IsNotFound(err) {
		e.complete()
		return ExecutionComplete
	}
	return ExecutionRunning
}
