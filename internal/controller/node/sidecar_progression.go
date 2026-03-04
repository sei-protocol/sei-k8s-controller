package node

import (
	"context"
	"fmt"
	"slices"
	"time"

	sidecar "github.com/sei-protocol/sei-sidecar-client-go"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-node-controller/api/v1alpha1"
)

const (
	taskSnapshotRestore    = sidecar.TaskTypeSnapshotRestore
	taskDiscoverPeers      = sidecar.TaskTypeDiscoverPeers
	taskConfigureGenesis   = sidecar.TaskTypeConfigureGenesis
	taskConfigureStateSync = sidecar.TaskTypeConfigureStateSync
	taskConfigPatch        = sidecar.TaskTypeConfigPatch
	taskMarkReady          = sidecar.TaskTypeMarkReady
	taskSnapshotUpload     = sidecar.TaskTypeSnapshotUpload
	taskUpdatePeers        = sidecar.TaskTypeUpdatePeers

	sidecarInitializing = string(sidecar.Initializing)
	sidecarRunning      = string(sidecar.Running)
	sidecarReady        = string(sidecar.Ready)

	bootstrapPollInterval = 5 * time.Second
	defaultMaxRetries     = 3
	baseRetryBackoff      = 5 * time.Second

	reasonBootstrapTaskFailed = "BootstrapTaskFailed"
)

// baseTaskProgression defines the ordered task sequence for each bootstrap mode.
// taskProgressionForNode may extend these when spec.peers is configured.
var baseTaskProgression = map[string][]string{
	"snapshot":  {taskSnapshotRestore, taskDiscoverPeers, taskConfigPatch, taskMarkReady},
	"peer-sync": {taskDiscoverPeers, taskConfigPatch, taskMarkReady},
	"genesis":   {taskConfigPatch, taskMarkReady},
}

// taskProgressionForNode returns the task progression for a node, dynamically
// inserting optional tasks before config-patch:
//   - discover-peers when spec.peers is configured
//   - configure-genesis when spec.genesis.s3 is configured
func taskProgressionForNode(node *seiv1alpha1.SeiNode) []string {
	mode := bootstrapMode(node)
	prog := slices.Clone(baseTaskProgression[mode])

	insertBeforeConfigPatch := func(task string) {
		if slices.Contains(prog, task) {
			return
		}
		for i, t := range prog {
			if t == taskConfigPatch {
				prog = slices.Insert(prog, i, task)
				return
			}
		}
	}

	if node.Spec.Peers != nil {
		insertBeforeConfigPatch(taskDiscoverPeers)
	}
	if node.Spec.Genesis.S3 != nil {
		insertBeforeConfigPatch(taskConfigureGenesis)
	}
	if needsStateSync(node) {
		insertBeforeConfigPatch(taskConfigureStateSync)
	}

	return prog
}

func needsStateSync(node *seiv1alpha1.SeiNode) bool {
	return !node.Spec.Genesis.Fresh && node.Spec.Snapshot == nil
}

// bootstrapMode returns the bootstrap mode string based on the node's spec.
func bootstrapMode(node *seiv1alpha1.SeiNode) string {
	switch {
	case node.Spec.Snapshot != nil:
		return "snapshot"
	case node.Spec.Genesis.PVC != nil:
		return "genesis"
	default:
		return "peer-sync"
	}
}

// retryInfo tracks per-task retry state for bootstrap failure handling.
type retryInfo struct {
	Count int
}

// SidecarStatusClient abstracts the sidecar HTTP API for testability.
type SidecarStatusClient interface {
	Status(ctx context.Context) (*sidecar.StatusResponse, error)
	SubmitTask(ctx context.Context, task sidecar.TaskBuilder) error
}

// buildSidecarClient constructs a SidecarClient from the node's sidecar config.
func (r *SeiNodeReconciler) buildSidecarClient(node *seiv1alpha1.SeiNode) SidecarStatusClient {
	if r.BuildSidecarClientFn != nil {
		return r.BuildSidecarClientFn(node)
	}
	port := node.Spec.Sidecar.Port
	if port == 0 {
		port = sidecar.DefaultPort
	}
	c, err := sidecar.NewSidecarClientFromPodDNS(node.Name, node.Namespace, port)
	if err != nil {
		return nil
	}
	return c
}

// writeSidecarStatus patches the sidecar-related fields on SeiNodeStatus,
// deriving CRD-friendly values from the sidecar's status + lastTask.
func (r *SeiNodeReconciler) writeSidecarStatus(ctx context.Context, node *seiv1alpha1.SeiNode, status *sidecar.StatusResponse) error {
	patch := client.MergeFrom(node.DeepCopy())
	node.Status.SidecarPhase = string(status.Status)
	node.Status.SidecarCurrentTask = ""
	if status.LastTask != nil {
		node.Status.SidecarLastTask = status.LastTask.Type
		if status.LastTask.Error != nil && *status.LastTask.Error != "" {
			node.Status.SidecarLastTaskResult = "error"
		} else {
			node.Status.SidecarLastTaskResult = "success"
		}
	} else {
		node.Status.SidecarLastTask = ""
		node.Status.SidecarLastTaskResult = ""
	}
	return r.Status().Patch(ctx, node, patch)
}

// reconcileSidecarProgression polls the sidecar and drives bootstrap/runtime
// task progression. The controller derives its phase model from the sidecar's
// status + lastTask fields:
//
//	Initializing + nil lastTask  → fresh start, issue first task
//	Running                      → task in flight, requeue
//	Initializing + lastTask set  → task complete, check error / issue next
//	Ready                        → bootstrap done, runtime tasks
func (r *SeiNodeReconciler) reconcileSidecarProgression(ctx context.Context, node *seiv1alpha1.SeiNode) (ctrl.Result, error) {
	sc := r.buildSidecarClient(node)

	status, err := sc.Status(ctx)
	if err != nil {
		return ctrl.Result{RequeueAfter: bootstrapPollInterval}, nil
	}

	if writeErr := r.writeSidecarStatus(ctx, node, status); writeErr != nil {
		return ctrl.Result{}, fmt.Errorf("writing sidecar status: %w", writeErr)
	}

	statusStr := string(status.Status)

	switch {
	case statusStr == sidecarInitializing && status.LastTask == nil:
		key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}
		r.resetRetryState(key)
		return r.issueFirstTask(ctx, node, sc)

	case statusStr == sidecarRunning:
		return ctrl.Result{RequeueAfter: bootstrapPollInterval}, nil

	case statusStr == sidecarInitializing && status.LastTask != nil:
		if status.LastTask.Error != nil && *status.LastTask.Error != "" {
			return r.handleTaskFailure(ctx, node, status)
		}
		return r.issueNextTask(ctx, node, sc, status.LastTask.Type)

	case statusStr == sidecarReady:
		return r.reconcileRuntimeTasks(ctx, node, sc)

	default:
		return ctrl.Result{RequeueAfter: statusPollInterval}, nil
	}
}

// reconcileRuntimeTasks handles post-bootstrap runtime tasks.
// When a snapshot destination is configured, submits snapshot-upload tasks --
// the sidecar is idempotent and no-ops when there is no new snapshot to upload.
func (r *SeiNodeReconciler) reconcileRuntimeTasks(ctx context.Context, node *seiv1alpha1.SeiNode, sc SidecarStatusClient) (ctrl.Result, error) {
	if task := snapshotUploadTask(node); task != nil {
		if err := sc.SubmitTask(ctx, task); err != nil {
			log.FromContext(ctx).Info("snapshot-upload submission failed, will retry", "error", err)
		}
		return ctrl.Result{RequeueAfter: statusPollInterval}, nil
	}

	return ctrl.Result{RequeueAfter: statusPollInterval}, nil
}

// getRetryState returns the retry info for a specific node+task combination.
func (r *SeiNodeReconciler) getRetryState(key types.NamespacedName, task string) *retryInfo {
	r.ensureRetryState()
	nodeRetries, ok := r.retryState[key]
	if !ok {
		nodeRetries = make(map[string]*retryInfo)
		r.retryState[key] = nodeRetries
	}
	info, ok := nodeRetries[task]
	if !ok {
		info = &retryInfo{}
		nodeRetries[task] = info
	}
	return info
}

// resetRetryState clears all retry counters for a node, called on
// Initialized phase regression (sidecar restart).
func (r *SeiNodeReconciler) resetRetryState(key types.NamespacedName) {
	r.ensureRetryState()
	delete(r.retryState, key)
}

func (r *SeiNodeReconciler) ensureRetryState() {
	if r.retryState == nil {
		r.retryState = make(map[types.NamespacedName]map[string]*retryInfo)
	}
}

func (r *SeiNodeReconciler) maxBootstrapRetries() int {
	if r.MaxBootstrapRetries > 0 {
		return r.MaxBootstrapRetries
	}
	return defaultMaxRetries
}

// issueFirstTask determines the first task from the bootstrap mode and submits it.
func (r *SeiNodeReconciler) issueFirstTask(ctx context.Context, node *seiv1alpha1.SeiNode, sc SidecarStatusClient) (ctrl.Result, error) {
	progression := taskProgressionForNode(node)
	if len(progression) == 0 {
		return ctrl.Result{}, fmt.Errorf("no task progression for mode %q", bootstrapMode(node))
	}

	first := progression[0]
	return r.issueTask(ctx, sc, taskBuilderForNode(node, first))
}

// issueTask submits a task to the sidecar and requeues for polling.
func (r *SeiNodeReconciler) issueTask(ctx context.Context, sc SidecarStatusClient, task sidecar.TaskBuilder) (ctrl.Result, error) {
	if err := sc.SubmitTask(ctx, task); err != nil {
		return ctrl.Result{RequeueAfter: bootstrapPollInterval}, nil
	}
	return ctrl.Result{RequeueAfter: bootstrapPollInterval}, nil
}

// issueNextTask walks the task progression to find the next task after the
// completed one. When the last task completes, bootstrap is done and the
// controller switches to steady-state polling.
func (r *SeiNodeReconciler) issueNextTask(
	ctx context.Context,
	node *seiv1alpha1.SeiNode,
	sc SidecarStatusClient,
	completedTask string,
) (ctrl.Result, error) {
	progression := taskProgressionForNode(node)

	for i, t := range progression {
		if t == completedTask && i+1 < len(progression) {
			next := progression[i+1]
			return r.issueTask(ctx, sc, taskBuilderForNode(node, next))
		}
	}

	// completedTask was the last in the progression — bootstrap is done.
	return ctrl.Result{RequeueAfter: statusPollInterval}, nil
}

// handleTaskFailure implements retry logic for failed bootstrap tasks and
// simple requeue for runtime failures.
//
// Bootstrap: exponential backoff (5s × 2^(retry-1)) up to configurable max.
// After max retries: sets Degraded condition and stops retrying.
// Runtime (post-Ready): logs and requeues at the normal 30s interval.
func (r *SeiNodeReconciler) handleTaskFailure(
	ctx context.Context,
	node *seiv1alpha1.SeiNode,
	status *sidecar.StatusResponse,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	taskType := status.LastTask.Type

	if isRuntimeTask(node, taskType) {
		logger.Info("runtime task failed, will re-evaluate on next reconcile",
			"task", taskType)
		return ctrl.Result{RequeueAfter: statusPollInterval}, nil
	}

	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}
	state := r.getRetryState(key, taskType)
	state.Count++

	if state.Count > r.maxBootstrapRetries() {
		patch := client.MergeFrom(node.DeepCopy())
		meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeDegraded,
			Status:             metav1.ConditionTrue,
			Reason:             reasonBootstrapTaskFailed,
			ObservedGeneration: node.Generation,
			Message:            fmt.Sprintf("task %s failed after %d retries", taskType, state.Count-1),
		})
		if err := r.Status().Patch(ctx, node, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("patching Degraded condition: %w", err)
		}
		logger.Error(fmt.Errorf("bootstrap task exhausted retries"), "manual intervention required",
			"task", taskType, "retries", state.Count-1)
		return ctrl.Result{}, nil
	}

	backoff := baseRetryBackoff * (1 << (state.Count - 1))
	logger.Info("bootstrap task failed, retrying with backoff",
		"task", taskType, "retry", state.Count, "backoff", backoff)
	return ctrl.Result{RequeueAfter: backoff}, nil
}

// isRuntimeTask returns true if the failed task is not part of the bootstrap
// progression for this node's mode, indicating a post-Ready runtime failure.
func isRuntimeTask(node *seiv1alpha1.SeiNode, task string) bool {
	return !slices.Contains(taskProgressionForNode(node), task)
}

// taskBuilderForNode maps a task type string to a typed TaskBuilder
// populated from the SeiNodeSpec.
func taskBuilderForNode(node *seiv1alpha1.SeiNode, taskType string) sidecar.TaskBuilder {
	switch taskType {
	case taskSnapshotRestore:
		return snapshotRestoreBuilder(node)
	case taskDiscoverPeers:
		return discoverPeersBuilder(node)
	case taskConfigureGenesis:
		return configureGenesisBuilder(node)
	case taskConfigureStateSync:
		return sidecar.ConfigureStateSyncTask{}
	case taskConfigPatch:
		return configPatchBuilder(node)
	case taskMarkReady:
		return sidecar.MarkReadyTask{}
	default:
		return sidecar.MarkReadyTask{}
	}
}

func snapshotRestoreBuilder(node *seiv1alpha1.SeiNode) sidecar.TaskBuilder {
	snap := node.Spec.Snapshot
	if snap == nil {
		return sidecar.SnapshotRestoreTask{}
	}
	bucket, prefix := parseS3URI(snap.Bucket.URI)
	return sidecar.SnapshotRestoreTask{
		Bucket:  bucket,
		Prefix:  prefix,
		Region:  snap.Region,
		ChainID: node.Spec.ChainID,
	}
}

func discoverPeersBuilder(node *seiv1alpha1.SeiNode) sidecar.TaskBuilder {
	if node.Spec.Peers == nil {
		return sidecar.DiscoverPeersTask{}
	}
	var sources []sidecar.PeerSource
	for _, s := range node.Spec.Peers.Sources {
		if s.EC2Tags != nil {
			sources = append(sources, sidecar.PeerSource{
				Type:   sidecar.PeerSourceEC2Tags,
				Region: s.EC2Tags.Region,
				Tags:   s.EC2Tags.Tags,
			})
		}
		if s.Static != nil {
			sources = append(sources, sidecar.PeerSource{
				Type:      sidecar.PeerSourceStatic,
				Addresses: s.Static.Addresses,
			})
		}
	}
	return sidecar.DiscoverPeersTask{Sources: sources}
}

func configureGenesisBuilder(node *seiv1alpha1.SeiNode) sidecar.TaskBuilder {
	if node.Spec.Genesis.S3 == nil {
		return sidecar.ConfigureGenesisTask{}
	}
	return sidecar.ConfigureGenesisTask{
		URI:    node.Spec.Genesis.S3.URI,
		Region: node.Spec.Genesis.S3.Region,
	}
}

func configPatchBuilder(node *seiv1alpha1.SeiNode) sidecar.TaskBuilder {
	if node.Spec.SnapshotGeneration == nil {
		return sidecar.ConfigPatchTask{}
	}
	sg := node.Spec.SnapshotGeneration
	keepRecent := sg.KeepRecent
	if keepRecent == 0 {
		keepRecent = 5
	}
	return sidecar.ConfigPatchTask{
		SnapshotGeneration: &sidecar.SnapshotGenerationPatch{
			Interval:   sg.Interval,
			KeepRecent: keepRecent,
		},
	}
}

// snapshotUploadTask builds a typed SnapshotUploadTask from the node spec.
// Returns nil when no snapshot destination is configured.
func snapshotUploadTask(node *seiv1alpha1.SeiNode) sidecar.TaskBuilder {
	sg := node.Spec.SnapshotGeneration
	if sg == nil || sg.Destination == nil || sg.Destination.S3 == nil {
		return nil
	}
	dest := sg.Destination.S3
	return sidecar.SnapshotUploadTask{
		Bucket: dest.Bucket,
		Prefix: dest.Prefix,
		Region: dest.Region,
	}
}
