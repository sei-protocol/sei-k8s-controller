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

// needsLongStartup returns true when the node's bootstrap strategy involves
// replaying blocks from a snapshot or state sync, which can take hours.
func needsLongStartup(node *seiv1alpha1.SeiNode) bool {
	switch {
	case node.Spec.FullNode != nil:
		return node.Spec.FullNode.Snapshot != nil
	case node.Spec.Validator != nil:
		return node.Spec.Validator.Snapshot != nil
	case node.Spec.Replayer != nil:
		return true
	default:
		return false
	}
}

// hasS3Snapshot returns true when the snapshot source is an S3 download.
func hasS3Snapshot(snap *seiv1alpha1.SnapshotSource) bool {
	return snap != nil && snap.S3 != nil
}

// hasStateSync returns true when the snapshot source is Tendermint state sync.
func hasStateSync(snap *seiv1alpha1.SnapshotSource) bool {
	return snap != nil && snap.StateSync != nil
}

// bootstrapMode determines the bootstrap strategy from a snapshot source.
func bootstrapMode(snap *seiv1alpha1.SnapshotSource) string {
	if hasS3Snapshot(snap) {
		return "snapshot"
	}
	if hasStateSync(snap) {
		return "state-sync"
	}
	return "genesis"
}

// baseProgression defines the ordered task sequence for each bootstrap mode.
var baseProgression = map[string][]string{
	"snapshot":   {taskSnapshotRestore, taskConfigApply, taskConfigValidate, taskMarkReady},
	"state-sync": {taskConfigApply, taskConfigValidate, taskMarkReady},
	"genesis":    {taskConfigApply, taskConfigValidate, taskMarkReady},
}

// buildPlan builds a TaskPlan by starting with the base progression
// for the node's bootstrap mode and inserting optional tasks.
func buildPlan(
	node *seiv1alpha1.SeiNode,
	peers []seiv1alpha1.PeerSource,
	snap *seiv1alpha1.SnapshotSource,
) *seiv1alpha1.TaskPlan {
	mode := bootstrapMode(snap)
	prog := slices.Clone(baseProgression[mode])

	if node.Spec.Genesis.S3 != nil {
		prog = insertBefore(prog, taskConfigApply, taskConfigureGenesis)
	}
	if len(peers) > 0 {
		prog = insertBefore(prog, taskConfigValidate, taskDiscoverPeers)
	}
	if snap != nil {
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

// buildSharedTask handles task types common across modes that use the
// standard bootstrap fields (peers, snapshot).
func buildSharedTask(
	node *seiv1alpha1.SeiNode,
	peers []seiv1alpha1.PeerSource,
	snap *seiv1alpha1.SnapshotSource,
	taskType string,
) sidecar.TaskBuilder {
	switch taskType {
	case taskSnapshotRestore:
		return snapshotRestoreTask(snap, node.Spec.ChainID)
	case taskDiscoverPeers:
		return discoverPeersTask(peers)
	case taskConfigureGenesis:
		return configureGenesisBuilder(node)
	case taskConfigureStateSync:
		return configureStateSyncTask(snap)
	case taskConfigValidate:
		return sidecar.ConfigValidateTask{}
	case taskMarkReady:
		return sidecar.MarkReadyTask{}
	default:
		return sidecar.MarkReadyTask{}
	}
}

func snapshotRestoreTask(snap *seiv1alpha1.SnapshotSource, chainID string) sidecar.TaskBuilder {
	if snap == nil || snap.S3 == nil {
		return sidecar.SnapshotRestoreTask{}
	}
	s3 := snap.S3
	var bucket, prefix string
	if s3.URI != "" {
		bucket, prefix = parseS3URI(s3.URI)
	} else {
		bucket = chainID + "-snapshots"
		prefix = "state-sync/"
	}
	return sidecar.SnapshotRestoreTask{
		Bucket:  bucket,
		Prefix:  prefix,
		Region:  s3.Region,
		ChainID: chainID,
	}
}

func discoverPeersTask(peers []seiv1alpha1.PeerSource) sidecar.TaskBuilder {
	if len(peers) == 0 {
		return sidecar.DiscoverPeersTask{}
	}
	var sources []sidecar.PeerSource
	for _, s := range peers {
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

func configureStateSyncTask(snap *seiv1alpha1.SnapshotSource) sidecar.TaskBuilder {
	task := sidecar.ConfigureStateSyncTask{
		UseLocalSnapshot: hasS3Snapshot(snap),
	}
	if snap != nil {
		if snap.TrustPeriod != "" {
			task.TrustPeriod = snap.TrustPeriod
		}
		task.BackfillBlocks = snap.BackfillBlocks
	}
	return task
}
