package task

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	TaskTypeAwaitExporterRunning = "await-exporter-running"
	TaskTypeSubmitExportState    = "submit-export-state"
	TaskTypeTeardownExporter     = "teardown-exporter"
)

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
		e.complete()
		return ExecutionComplete
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
	params    SubmitExportStateParams
	cfg       ExecutionConfig
	submitted bool
	taskID    string
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

func (e *submitExportStateExecution) Execute(ctx context.Context) error {
	node := &seiv1alpha1.SeiNode{}
	if err := e.cfg.KubeClient.Get(ctx, types.NamespacedName{
		Name: e.params.ExporterName, Namespace: e.params.Namespace,
	}, node); err != nil {
		return fmt.Errorf("getting exporter node: %w", err)
	}

	sc, err := e.cfg.BuildSidecarClient()
	if err != nil {
		return fmt.Errorf("building sidecar client for exporter: %w", err)
	}

	// Build the export-state sidecar task request.
	exportParams := map[string]any{
		"height":   e.params.ExportHeight,
		"chainId":  e.params.SourceChainID,
		"s3Bucket": e.cfg.Platform.GenesisBucket,
		"s3Key":    fmt.Sprintf("%s/exported-state.json", e.params.SourceChainID),
		"s3Region": e.cfg.Platform.GenesisRegion,
	}

	taskID := DeterministicTaskID(e.params.ExporterName, "export-state", 0)

	// TODO: Submit via sidecar client once we have the sidecar client
	// built for the exporter node (not the group's resource).
	// For now, store the task ID for polling.
	_ = sc
	_ = exportParams
	_ = taskID

	e.submitted = true
	e.taskID = taskID
	return nil
}

func (e *submitExportStateExecution) Status(ctx context.Context) ExecutionStatus {
	if s, done := e.isTerminal(); done {
		return s
	}
	if !e.submitted {
		return ExecutionRunning
	}
	// TODO: Poll sidecar for export task completion.
	// For now, return Running (the task will be re-submitted on restart).
	return ExecutionRunning
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
