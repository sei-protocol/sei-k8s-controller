package node

import (
	"maps"

	sidecar "github.com/sei-protocol/seictl/sidecar/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	defaultSnapshotUploadCron = "0 0 * * *"
	defaultSnapshotInterval   = int64(2000)
	defaultResultExportCron   = "*/10 * * * *"

	resultExportBucket = "sei-node-mvp"
	resultExportRegion = "eu-central-1"
	resultExportPrefix = "shadow-results/"

	// sei-config unified schema keys used by planner controllerOverrides.
	keyConcurrencyWorkers = "chain.concurrency_workers"
	keyMinRetainBlocks    = "chain.min_retain_blocks"
	keyPruning            = "storage.pruning"
	keyPruningKeepRecent  = "storage.pruning_keep_recent"
	keyPruningKeepEvery   = "storage.pruning_keep_every"
	keyPruningInterval    = "storage.pruning_interval"
	keySnapshotInterval   = "storage.snapshot_interval"
	keySnapshotKeepRecent = "storage.snapshot_keep_recent"

	valCustom  = "custom"
	valNothing = "nothing"

	// Matches sei-infra production config across all node roles.
	defaultConcurrencyWorkers = "500"
)

// mergeOverrides combines controller-generated overrides with user-specified
// overrides from spec.overrides. User overrides take precedence.
func mergeOverrides(controllerOverrides map[string]string, userOverrides map[string]string) map[string]string {
	if len(controllerOverrides) == 0 && len(userOverrides) == 0 {
		return nil
	}
	merged := make(map[string]string, len(controllerOverrides)+len(userOverrides))
	maps.Copy(merged, controllerOverrides)
	maps.Copy(merged, userOverrides)
	return merged
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

// resultExportScheduledTask returns a result-export task builder with a
// cron schedule if the node is a replayer with result export enabled.
// Returns nil when not applicable.
func resultExportScheduledTask(node *seiv1alpha1.SeiNode) sidecar.TaskBuilder {
	if node.Spec.Replayer == nil || node.Spec.Replayer.ResultExport == nil {
		return nil
	}
	cron := defaultResultExportCron
	return sidecar.ResultExportTask{
		Bucket:   resultExportBucket,
		Prefix:   resultExportPrefix + node.Spec.ChainID + "/",
		Region:   resultExportRegion,
		Schedule: &sidecar.ScheduleConfig{Cron: &cron},
	}
}

// snapshotUploadScheduledTask returns a snapshot-upload task builder with a
// cron schedule if the node's mode sub-spec configures snapshot generation
// with an S3 destination. Returns nil when not applicable.
func snapshotUploadScheduledTask(node *seiv1alpha1.SeiNode) sidecar.TaskBuilder {
	sg := snapshotGeneration(node)
	if sg == nil || sg.Destination == nil || sg.Destination.S3 == nil {
		return nil
	}
	dest := sg.Destination.S3
	cron := defaultSnapshotUploadCron
	return sidecar.SnapshotUploadTask{
		Bucket:   dest.Bucket,
		Prefix:   dest.Prefix,
		Region:   dest.Region,
		Schedule: &sidecar.ScheduleConfig{Cron: &cron},
	}
}
