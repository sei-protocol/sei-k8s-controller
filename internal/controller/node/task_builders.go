package node

import (
	sidecar "github.com/sei-protocol/seictl/sidecar/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	defaultSnapshotUploadCron = "0 0 * * *"
	defaultSnapshotInterval   = int64(2000)

	resultExportBucket = "sei-node-mvp"
	resultExportRegion = "us-east-2"
	resultExportPrefix = "shadow-results/"

	valNothing = "nothing"
)

func configureGenesisBuilder(node *seiv1alpha1.SeiNode) sidecar.TaskBuilder {
	if node.Spec.Genesis.S3 == nil {
		return sidecar.ConfigureGenesisTask{}
	}
	return sidecar.ConfigureGenesisTask{
		URI:    node.Spec.Genesis.S3.URI,
		Region: node.Spec.Genesis.S3.Region,
	}
}

// resultExportTaskFromSpec returns a scheduled result-export task if the
// node is a replayer with result export enabled. Bucket, region, and prefix
// are platform defaults — the prefix is derived from the node's chain ID.
func resultExportTaskFromSpec(node *seiv1alpha1.SeiNode) sidecar.TaskBuilder {
	if node.Spec.Replayer == nil || node.Spec.Replayer.ResultExport == nil {
		return nil
	}
	return sidecar.ResultExportTask{
		Bucket: resultExportBucket,
		Prefix: resultExportPrefix + node.Spec.ChainID + "/",
		Region: resultExportRegion,
	}
}

// snapshotUploadTaskFromSpec returns a scheduled snapshot-upload task if the
// node's mode sub-spec configures snapshot generation with an S3 destination.
func snapshotUploadTaskFromSpec(node *seiv1alpha1.SeiNode) sidecar.TaskBuilder {
	sg := snapshotGeneration(node)
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
