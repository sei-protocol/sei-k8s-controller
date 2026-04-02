package task

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

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
		e.complete()
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("checking exporter existence: %w", err)
	}

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

	sc, err := sidecarClientForNode(node)
	if err != nil {
		return fmt.Errorf("building sidecar client for exporter: %w", err)
	}

	taskID := uuid.MustParse(DeterministicTaskID(e.params.ExporterName, "export-state", 0))
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

	e.submitted = true
	e.taskID = taskID.String()
	return nil
}

func (e *submitExportStateExecution) Status(ctx context.Context) ExecutionStatus {
	if s, done := e.isTerminal(); done {
		return s
	}
	if !e.submitted {
		return ExecutionRunning
	}

	node := &seiv1alpha1.SeiNode{}
	if err := e.cfg.KubeClient.Get(ctx, types.NamespacedName{
		Name: e.params.ExporterName, Namespace: e.params.Namespace,
	}, node); err != nil {
		return ExecutionRunning
	}

	sc, err := sidecarClientForNode(node)
	if err != nil {
		return ExecutionRunning
	}

	taskID, err := uuid.Parse(e.taskID)
	if err != nil {
		return ExecutionRunning
	}

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
