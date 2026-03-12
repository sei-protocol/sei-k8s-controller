package node

import (
	"maps"
	"strconv"

	seiconfig "github.com/sei-protocol/sei-config"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	defaultSnapshotUploadCron = "0 0 * * *"
	defaultSnapshotInterval   = int64(2000)
	defaultMode               = "full"

	valTrue    = "true"
	valNothing = "nothing"
)

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
	case taskConfigApply:
		return configApplyBuilder(node)
	case taskConfigValidate:
		return sidecar.ConfigValidateTask{}
	case taskMarkReady:
		return sidecar.MarkReadyTask{}
	default:
		return sidecar.MarkReadyTask{}
	}
}

// resolveMode returns the node's operating mode, defaulting to "full".
func resolveMode(node *seiv1alpha1.SeiNode) string {
	if node.Spec.Mode != "" {
		return node.Spec.Mode
	}
	return defaultMode
}

func snapshotRestoreBuilder(node *seiv1alpha1.SeiNode) sidecar.TaskBuilder {
	if !hasLocalSnapshot(node) {
		return sidecar.SnapshotRestoreTask{}
	}
	snap := node.Spec.StateSync.Snapshot
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

// configApplyBuilder constructs a ConfigApplyTask with a ConfigIntent that
// captures the full desired state. The controller's only job is to build the
// intent; sei-config owns the resolution pipeline.
func configApplyBuilder(node *seiv1alpha1.SeiNode) sidecar.TaskBuilder {
	intent := seiconfig.ConfigIntent{
		Mode:      seiconfig.NodeMode(resolveMode(node)),
		Overrides: collectOverrides(node),
	}
	if node.Spec.Config != nil && node.Spec.Config.Version > 0 {
		intent.TargetVersion = node.Spec.Config.Version
	}
	return sidecar.ConfigApplyTask{Intent: intent}
}

// collectOverrides merges user-specified CRD overrides with controller-managed
// parameters (snapshot generation, state sync). Controller-managed keys take
// precedence over user overrides for the same key.
func collectOverrides(node *seiv1alpha1.SeiNode) map[string]string {
	overrides := make(map[string]string)

	if node.Spec.Config != nil {
		maps.Copy(overrides, node.Spec.Config.Overrides)
	}

	if node.Spec.SnapshotGeneration != nil {
		overrides["storage.pruning"] = valNothing
		overrides["storage.snapshot_interval"] = strconv.FormatInt(defaultSnapshotInterval, 10)
		overrides["storage.snapshot_keep_recent"] = strconv.FormatInt(
			int64(node.Spec.SnapshotGeneration.KeepRecent), 10)
	}

	if hasLocalSnapshot(node) {
		overrides["state_sync.enable"] = valTrue
		overrides["state_sync.use_local_snapshot"] = valTrue
		overrides["state_sync.trust_period"] = node.Spec.StateSync.TrustPeriod
		overrides["state_sync.backfill_blocks"] = strconv.FormatInt(
			node.Spec.StateSync.BackfillBlocks, 10)
	}

	return overrides
}

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
		Cron:   defaultSnapshotUploadCron,
	}
}
