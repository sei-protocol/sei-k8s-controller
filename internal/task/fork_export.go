package task

import (
	"context"
	"encoding/json"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// ExporterPVCName returns the data PVC name for a fork-genesis exporter.
func ExporterPVCName(groupName string) string { return fmt.Sprintf("%s-exporter-data", groupName) }

// ExporterServiceName returns the headless Service name for an exporter.
func ExporterServiceName(groupName string) string { return fmt.Sprintf("%s-exporter", groupName) }

// --- ensure-exporter-pvc ---

// EnsureExporterPVCParams carries the SND-side context needed to ensure the
// exporter PVC. Storage size and class come from cfg.Platform at exec time
// (matching the SeiNode-bootstrap path in internal/noderesource), so the
// planner doesn't need a Platform reference and chain-specific sizing
// (full vs archive) can change via env vars without touching plan params.
type EnsureExporterPVCParams struct {
	PVCName   string `json:"pvcName"`
	Namespace string `json:"namespace"`
}

type ensureExporterPVCExecution struct {
	taskBase
	params EnsureExporterPVCParams
	cfg    ExecutionConfig
}

func deserializeEnsureExporterPVC(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p EnsureExporterPVCParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing ensure-exporter-pvc params: %w", err)
		}
	}
	return &ensureExporterPVCExecution{taskBase: taskBase{id: id, status: ExecutionRunning}, params: p, cfg: cfg}, nil
}

func (e *ensureExporterPVCExecution) Execute(ctx context.Context) error {
	group, err := ResourceAs[*seiv1alpha1.SeiNodeDeployment](e.cfg)
	if err != nil {
		return Terminal(err)
	}

	size, err := resource.ParseQuantity(e.cfg.Platform.StorageSizeDefault)
	if err != nil {
		return Terminal(fmt.Errorf("parsing platform StorageSizeDefault %q: %w", e.cfg.Platform.StorageSizeDefault, err))
	}
	storageClass := e.cfg.Platform.StorageClassPerf

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: e.params.PVCName, Namespace: e.params.Namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &storageClass,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
			},
		},
	}

	if err := ctrl.SetControllerReference(group, pvc, e.cfg.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on exporter PVC: %w", err)
	}
	if err := e.cfg.KubeClient.Create(ctx, pvc); err != nil {
		if apierrors.IsAlreadyExists(err) {
			e.complete()
			return nil
		}
		return fmt.Errorf("creating exporter PVC: %w", err)
	}
	e.complete()
	return nil
}

func (e *ensureExporterPVCExecution) Status(ctx context.Context) ExecutionStatus {
	if s, done := e.isTerminal(); done {
		return s
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := e.cfg.KubeClient.Get(ctx,
		types.NamespacedName{Name: e.params.PVCName, Namespace: e.params.Namespace}, pvc); err == nil {
		e.complete()
	}
	return e.DefaultStatus()
}

// --- apply-bootstrap-job (SND-driven) ---

// ApplyBootstrapJobParams carries the SND-side context needed to build the
// bootstrap Job. Pod-shape inputs (sidecar image/port, mode resources) come
// from cfg.Platform at exec time so the planner doesn't need a Platform
// reference. Mirrors the per-SeiNode bootstrap path.
type ApplyBootstrapJobParams struct {
	Namespace string `json:"namespace"`
}

type applyBootstrapJobExecution struct {
	taskBase
	params ApplyBootstrapJobParams
	cfg    ExecutionConfig
}

func deserializeApplyBootstrapJob(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p ApplyBootstrapJobParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing apply-bootstrap-job params: %w", err)
		}
	}
	return &applyBootstrapJobExecution{taskBase: taskBase{id: id, status: ExecutionRunning}, params: p, cfg: cfg}, nil
}

func (e *applyBootstrapJobExecution) Execute(ctx context.Context) error {
	group, err := ResourceAs[*seiv1alpha1.SeiNodeDeployment](e.cfg)
	if err != nil {
		return Terminal(err)
	}
	inputs := sndToBootstrapInputs(group, e.cfg.Platform)
	job, err := GenerateBootstrapJob(inputs, e.cfg.Platform)
	if err != nil {
		return Terminal(fmt.Errorf("generating bootstrap job: %w", err))
	}
	if err := ctrl.SetControllerReference(group, job, e.cfg.Scheme); err != nil {
		return fmt.Errorf("setting owner reference: %w", err)
	}
	if err := e.cfg.KubeClient.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			e.complete()
			return nil
		}
		return fmt.Errorf("creating bootstrap job: %w", err)
	}
	e.complete()
	return nil
}

func (e *applyBootstrapJobExecution) Status(ctx context.Context) ExecutionStatus {
	if s, done := e.isTerminal(); done {
		return s
	}
	group, err := ResourceAs[*seiv1alpha1.SeiNodeDeployment](e.cfg)
	if err != nil {
		return ExecutionRunning
	}
	existing := &batchv1.Job{}
	if err := e.cfg.KubeClient.Get(ctx,
		types.NamespacedName{Name: BootstrapJobName(exporterRoot(group.Name)), Namespace: e.params.Namespace}, existing); err == nil {
		e.complete()
	}
	return e.DefaultStatus()
}

// --- await-job (shared by await-bootstrap-job and await-export-job) ---

// AwaitJobParams polls a single Job by name+namespace until it reports
// Complete or Failed. Used by both the bootstrap and export Jobs in the
// fork-genesis sub-plan.
type AwaitJobParams struct {
	JobName   string `json:"jobName"`
	Namespace string `json:"namespace"`
}

type awaitJobExecution struct {
	taskBase
	params AwaitJobParams
	cfg    ExecutionConfig
}

func deserializeAwaitJob(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p AwaitJobParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing await-job params: %w", err)
		}
	}
	return &awaitJobExecution{taskBase: taskBase{id: id, status: ExecutionRunning}, params: p, cfg: cfg}, nil
}

func (e *awaitJobExecution) Execute(_ context.Context) error { return nil }

func (e *awaitJobExecution) Status(ctx context.Context) ExecutionStatus {
	if s, done := e.isTerminal(); done {
		return s
	}
	job := &batchv1.Job{}
	key := types.NamespacedName{Name: e.params.JobName, Namespace: e.params.Namespace}
	if err := e.cfg.KubeClient.Get(ctx, key, job); err != nil {
		if apierrors.IsNotFound(err) {
			e.setFailed(fmt.Errorf("job %s not found", e.params.JobName))
			return ExecutionFailed
		}
		return ExecutionRunning
	}
	if IsJobComplete(job) {
		e.complete()
		return ExecutionComplete
	}
	if IsJobFailed(job) {
		e.setFailed(fmt.Errorf("job %s failed: %s", e.params.JobName, JobFailureReason(job)))
		return ExecutionFailed
	}
	return ExecutionRunning
}

// --- apply-export-job ---

// ApplyExportJobParams mirrors ApplyBootstrapJobParams: minimal SND-side
// context, the rest resolved at exec time from cfg.Platform.
type ApplyExportJobParams struct {
	Namespace string `json:"namespace"`
}

type applyExportJobExecution struct {
	taskBase
	params ApplyExportJobParams
	cfg    ExecutionConfig
}

func deserializeApplyExportJob(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p ApplyExportJobParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing apply-export-job params: %w", err)
		}
	}
	return &applyExportJobExecution{taskBase: taskBase{id: id, status: ExecutionRunning}, params: p, cfg: cfg}, nil
}

func (e *applyExportJobExecution) Execute(ctx context.Context) error {
	group, err := ResourceAs[*seiv1alpha1.SeiNodeDeployment](e.cfg)
	if err != nil {
		return Terminal(err)
	}
	inputs := sndToExportInputs(group, e.cfg.Platform)
	job, err := GenerateExportJob(inputs, e.cfg.Platform)
	if err != nil {
		return Terminal(fmt.Errorf("generating export job: %w", err))
	}
	if err := ctrl.SetControllerReference(group, job, e.cfg.Scheme); err != nil {
		return fmt.Errorf("setting owner reference: %w", err)
	}
	if err := e.cfg.KubeClient.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating export job: %w", err)
	}
	// Stamp Status.Fork.ExportJobRef so kubectl describe surfaces the Job
	// to operators. The reconciler's single-patch flush picks this up.
	if group.Status.Fork == nil {
		group.Status.Fork = &seiv1alpha1.ForkStatus{}
	}
	group.Status.Fork.ExportJobRef = fmt.Sprintf("%s/%s", job.Namespace, job.Name)
	e.complete()
	return nil
}

func (e *applyExportJobExecution) Status(ctx context.Context) ExecutionStatus {
	if s, done := e.isTerminal(); done {
		return s
	}
	group, err := ResourceAs[*seiv1alpha1.SeiNodeDeployment](e.cfg)
	if err != nil {
		return ExecutionRunning
	}
	existing := &batchv1.Job{}
	if err := e.cfg.KubeClient.Get(ctx,
		types.NamespacedName{Name: ExportJobName(exporterRoot(group.Name)), Namespace: e.params.Namespace}, existing); err == nil {
		e.complete()
	}
	return e.DefaultStatus()
}

// --- teardown-exporter ---

type TeardownExporterParams struct {
	PVCName      string `json:"pvcName"`
	BootstrapJob string `json:"bootstrapJob"`
	ExportJob    string `json:"exportJob"`
	ServiceName  string `json:"serviceName"`
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
	return &teardownExporterExecution{taskBase: taskBase{id: id, status: ExecutionRunning}, params: p, cfg: cfg}, nil
}

func (e *teardownExporterExecution) Execute(ctx context.Context) error {
	deletes := []client.Object{
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: e.params.BootstrapJob, Namespace: e.params.Namespace}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: e.params.ExportJob, Namespace: e.params.Namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: e.params.ServiceName, Namespace: e.params.Namespace}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: e.params.PVCName, Namespace: e.params.Namespace}},
	}
	for _, obj := range deletes {
		if err := e.cfg.KubeClient.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting %T %s: %w", obj, obj.GetName(), err)
		}
	}
	e.complete()
	return nil
}

func (e *teardownExporterExecution) Status(ctx context.Context) ExecutionStatus {
	if s, done := e.isTerminal(); done {
		return s
	}
	// All four resources must be gone.
	for _, key := range []types.NamespacedName{
		{Name: e.params.BootstrapJob, Namespace: e.params.Namespace},
		{Name: e.params.ExportJob, Namespace: e.params.Namespace},
	} {
		if err := e.cfg.KubeClient.Get(ctx, key, &batchv1.Job{}); err == nil {
			return ExecutionRunning
		}
	}
	if err := e.cfg.KubeClient.Get(ctx,
		types.NamespacedName{Name: e.params.ServiceName, Namespace: e.params.Namespace}, &corev1.Service{}); err == nil {
		return ExecutionRunning
	}
	if err := e.cfg.KubeClient.Get(ctx,
		types.NamespacedName{Name: e.params.PVCName, Namespace: e.params.Namespace}, &corev1.PersistentVolumeClaim{}); err == nil {
		return ExecutionRunning
	}
	e.complete()
	return ExecutionComplete
}
