package node

import (
	"fmt"

	seiconfig "github.com/sei-protocol/sei-config"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

type replayerPlanner struct{}

func (p *replayerPlanner) Mode() string { return string(seiconfig.ModeArchive) }

func (p *replayerPlanner) Validate(node *seiv1alpha1.SeiNode) error {
	if node.Spec.Replayer == nil {
		return fmt.Errorf("replayer sub-spec is nil")
	}
	if node.Spec.Replayer.Snapshot.Bucket.URI == "" {
		return fmt.Errorf("replayer requires a snapshot bucket URI")
	}
	if len(node.Spec.Replayer.Peers.Sources) == 0 {
		return fmt.Errorf("replayer requires at least one peer source for block sync")
	}
	return nil
}

func (p *replayerPlanner) BuildPlan(node *seiv1alpha1.SeiNode) *seiv1alpha1.TaskPlan {
	prog := []string{taskSnapshotRestore, taskConfigApply, taskDiscoverPeers, taskConfigValidate, taskMarkReady}

	if node.Spec.Genesis.S3 != nil {
		prog = insertBefore(prog, taskConfigApply, taskConfigureGenesis)
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

func (p *replayerPlanner) BuildTask(node *seiv1alpha1.SeiNode, taskType string) sidecar.TaskBuilder {
	switch taskType {
	case taskSnapshotRestore:
		snap := node.Spec.Replayer.Snapshot
		bucket, prefix := parseS3URI(snap.Bucket.URI)
		return sidecar.SnapshotRestoreTask{
			Bucket:  bucket,
			Prefix:  prefix,
			Region:  snap.Region,
			ChainID: node.Spec.ChainID,
		}
	case taskDiscoverPeers:
		return discoverPeersFromConfig(&node.Spec.Replayer.Peers)
	case taskConfigApply:
		return sidecar.ConfigApplyTask{
			Intent: seiconfig.ConfigIntent{
				Mode: seiconfig.ModeArchive,
			},
		}
	case taskConfigureGenesis:
		return configureGenesisBuilder(node)
	case taskConfigValidate:
		return sidecar.ConfigValidateTask{}
	case taskMarkReady:
		return sidecar.MarkReadyTask{}
	default:
		return sidecar.MarkReadyTask{}
	}
}
