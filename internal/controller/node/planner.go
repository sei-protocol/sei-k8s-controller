package node

import (
	"fmt"
	"slices"

	sidecar "github.com/sei-protocol/seictl/sidecar/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// NodePlanner encapsulates mode-specific logic for validating a SeiNode,
// building its initialization task plan, and constructing individual sidecar
// task builders.
type NodePlanner interface {
	// Validate checks that the sub-spec fields are sufficient for this mode.
	Validate(node *seiv1alpha1.SeiNode) error

	// BuildPlan returns the ordered initialization task plan.
	BuildPlan(node *seiv1alpha1.SeiNode) *seiv1alpha1.TaskPlan

	// BuildTask constructs the sidecar TaskBuilder for a given task type.
	BuildTask(node *seiv1alpha1.SeiNode, taskType string) sidecar.TaskBuilder

	// Mode returns the sei-config mode string for config-apply.
	Mode() string
}

// PlannerForNode returns the appropriate NodePlanner based on which mode
// sub-spec is populated on the SeiNode.
func PlannerForNode(node *seiv1alpha1.SeiNode) (NodePlanner, error) {
	switch {
	case node.Spec.FullNode != nil:
		return &fullNodePlanner{}, nil
	case node.Spec.Archive != nil:
		return &archiveNodePlanner{}, nil
	case node.Spec.Replayer != nil:
		return &replayerPlanner{}, nil
	case node.Spec.Validator != nil:
		return &validatorPlanner{}, nil
	default:
		return nil, fmt.Errorf("no mode sub-spec set on SeiNode %s/%s", node.Namespace, node.Name)
	}
}

// syncConfig extracts the SyncConfig from whichever mode sub-spec is populated.
// Returns nil when the mode has no sync config (e.g. replayer).
func syncConfig(node *seiv1alpha1.SeiNode) *seiv1alpha1.SyncConfig {
	switch {
	case node.Spec.FullNode != nil:
		return node.Spec.FullNode.Sync
	case node.Spec.Archive != nil:
		return nil
	case node.Spec.Validator != nil:
		return node.Spec.Validator.Sync
	default:
		return nil
	}
}

// snapshotGeneration extracts the SnapshotGenerationConfig from the populated
// mode sub-spec. Returns nil when the mode doesn't support it.
func snapshotGeneration(node *seiv1alpha1.SeiNode) *seiv1alpha1.SnapshotGenerationConfig {
	switch {
	case node.Spec.FullNode != nil:
		return node.Spec.FullNode.SnapshotGeneration
	case node.Spec.Archive != nil:
		return node.Spec.Archive.SnapshotGeneration
	default:
		return nil
	}
}

// syncBootstrapMode determines the bootstrap strategy from a SyncConfig.
func syncBootstrapMode(sync *seiv1alpha1.SyncConfig) string {
	if sync == nil {
		return "genesis"
	}
	if sync.BlockSync != nil && sync.BlockSync.Snapshot != nil {
		return "snapshot"
	}
	if sync.StateSync != nil {
		return "state-sync"
	}
	if sync.Peers != nil {
		return "peer-sync"
	}
	return "genesis"
}

// hasPeers returns true when the sync config provides peer sources.
func hasPeers(sync *seiv1alpha1.SyncConfig) bool {
	return sync != nil && sync.Peers != nil
}

// hasSnapshotRestore returns true when block sync with an S3 snapshot is configured.
func hasSnapshotRestore(sync *seiv1alpha1.SyncConfig) bool {
	return sync != nil && sync.BlockSync != nil && sync.BlockSync.Snapshot != nil
}

// baseProgression defines the ordered task sequence for each bootstrap mode.
var baseProgression = map[string][]string{
	"snapshot":   {taskSnapshotRestore, taskConfigApply, taskConfigValidate, taskMarkReady},
	"state-sync": {taskConfigApply, taskConfigValidate, taskMarkReady},
	"peer-sync":  {taskConfigApply, taskConfigValidate, taskMarkReady},
	"genesis":    {taskConfigApply, taskConfigValidate, taskMarkReady},
}

// buildPlanFromSync builds a TaskPlan by starting with the base progression
// for the node's bootstrap mode and inserting optional tasks.
func buildPlanFromSync(node *seiv1alpha1.SeiNode, sync *seiv1alpha1.SyncConfig) *seiv1alpha1.TaskPlan {
	mode := syncBootstrapMode(sync)
	prog := slices.Clone(baseProgression[mode])

	if node.Spec.Genesis.S3 != nil {
		prog = insertBefore(prog, taskConfigApply, taskConfigureGenesis)
	}
	if hasPeers(sync) {
		prog = insertBefore(prog, taskConfigValidate, taskDiscoverPeers)
	}
	if sync != nil && (sync.StateSync != nil || hasSnapshotRestore(sync)) {
		prog = insertBefore(prog, taskConfigValidate, taskConfigureStateSync)
	}

	tasks := make([]seiv1alpha1.PlannedTask, len(prog))
	for i, taskType := range prog {
		tasks[i] = seiv1alpha1.PlannedTask{
			Type:   taskType,
			Status: seiv1alpha1.PlannedTaskPending,
		}
	}
	return &seiv1alpha1.TaskPlan{
		Phase: seiv1alpha1.TaskPlanActive,
		Tasks: tasks,
	}
}

// sharedTaskBuilder handles task types common across all modes.
// Returns nil when the task type is mode-specific and must be handled by the planner.
func sharedTaskBuilder(node *seiv1alpha1.SeiNode, sync *seiv1alpha1.SyncConfig, taskType string) sidecar.TaskBuilder {
	switch taskType {
	case taskSnapshotRestore:
		return snapshotRestoreFromSync(sync, node.Spec.ChainID)
	case taskDiscoverPeers:
		return discoverPeersFromSync(sync)
	case taskConfigureGenesis:
		return configureGenesisBuilder(node)
	case taskConfigureStateSync:
		return configureStateSyncFromSync(sync)
	case taskConfigValidate:
		return sidecar.ConfigValidateTask{}
	case taskMarkReady:
		return sidecar.MarkReadyTask{}
	default:
		return sidecar.MarkReadyTask{}
	}
}

func snapshotRestoreFromSync(sync *seiv1alpha1.SyncConfig, chainID string) sidecar.TaskBuilder {
	if !hasSnapshotRestore(sync) {
		return sidecar.SnapshotRestoreTask{}
	}
	snap := sync.BlockSync.Snapshot
	bucket, prefix := parseS3URI(snap.Bucket.URI)
	return sidecar.SnapshotRestoreTask{
		Bucket:  bucket,
		Prefix:  prefix,
		Region:  snap.Region,
		ChainID: chainID,
	}
}

func discoverPeersFromSync(sync *seiv1alpha1.SyncConfig) sidecar.TaskBuilder {
	if !hasPeers(sync) {
		return sidecar.DiscoverPeersTask{}
	}
	return discoverPeersFromConfig(sync.Peers)
}

func discoverPeersFromConfig(peers *seiv1alpha1.PeerConfig) sidecar.TaskBuilder {
	if peers == nil {
		return sidecar.DiscoverPeersTask{}
	}
	var sources []sidecar.PeerSource
	for _, s := range peers.Sources {
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

func configureStateSyncFromSync(sync *seiv1alpha1.SyncConfig) sidecar.TaskBuilder {
	task := sidecar.ConfigureStateSyncTask{
		UseLocalSnapshot: hasSnapshotRestore(sync),
	}
	if hasSnapshotRestore(sync) && sync.BlockSync.Snapshot.TrustPeriod != "" {
		task.TrustPeriod = sync.BlockSync.Snapshot.TrustPeriod
	}
	if hasSnapshotRestore(sync) {
		task.BackfillBlocks = sync.BlockSync.Snapshot.BackfillBlocks
	}
	return task
}
