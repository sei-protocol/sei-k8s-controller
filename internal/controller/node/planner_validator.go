package node

import (
	"fmt"

	seiconfig "github.com/sei-protocol/sei-config"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

type validatorPlanner struct {
	snapshotRegion string
}

func (p *validatorPlanner) Mode() string { return string(seiconfig.ModeValidator) }

func (p *validatorPlanner) Validate(node *seiv1alpha1.SeiNode) error {
	if node.Spec.Validator == nil {
		return fmt.Errorf("validator sub-spec is nil")
	}
	if gc := node.Spec.Validator.GenesisCeremony; gc != nil {
		if gc.ChainID == "" {
			return fmt.Errorf("validator: genesisCeremony.chainId is required")
		}
		if gc.StakingAmount == "" {
			return fmt.Errorf("validator: genesisCeremony.stakingAmount is required")
		}
		if gc.ArtifactS3.Bucket == "" || gc.ArtifactS3.Region == "" {
			return fmt.Errorf("validator: genesisCeremony.artifactS3 bucket and region are required")
		}
		return nil
	}
	if snap := node.Spec.Validator.Snapshot; snap != nil && snap.BootstrapImage != "" {
		if snap.S3 == nil || snap.S3.TargetHeight <= 0 {
			return fmt.Errorf("validator: bootstrapImage requires s3 with targetHeight > 0")
		}
	}
	return nil
}

func (p *validatorPlanner) BuildPlan(node *seiv1alpha1.SeiNode) *seiv1alpha1.TaskPlan {
	v := node.Spec.Validator
	return buildPlan(v.Peers, v.Snapshot)
}

func (p *validatorPlanner) BuildTask(node *seiv1alpha1.SeiNode, taskType string) (sidecar.TaskBuilder, error) {
	if taskType == taskConfigApply {
		return sidecar.ConfigApplyTask{
			Intent: seiconfig.ConfigIntent{
				Mode:      seiconfig.ModeValidator,
				Overrides: mergeOverrides(nil, node.Spec.Overrides),
			},
		}, nil
	}
	v := node.Spec.Validator
	return buildSharedTask(node, v.Peers, v.Snapshot, taskType, p.snapshotRegion)
}
