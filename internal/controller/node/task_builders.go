package node

import (
	sidecar "github.com/sei-protocol/seictl/sidecar/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
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
