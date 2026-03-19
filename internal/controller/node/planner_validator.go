package node

import (
	"fmt"

	seiconfig "github.com/sei-protocol/sei-config"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

type validatorPlanner struct{}

func (p *validatorPlanner) Mode() string { return string(seiconfig.ModeValidator) }

func (p *validatorPlanner) Validate(node *seiv1alpha1.SeiNode) error {
	if node.Spec.Validator == nil {
		return fmt.Errorf("validator sub-spec is nil")
	}
	return nil
}

func (p *validatorPlanner) BuildPlan(node *seiv1alpha1.SeiNode) *seiv1alpha1.TaskPlan {
	v := node.Spec.Validator
	return buildPlan(node, v.Peers, v.Snapshot)
}

func (p *validatorPlanner) BuildTask(node *seiv1alpha1.SeiNode, taskType string) sidecar.TaskBuilder {
	if taskType == taskConfigApply {
		return sidecar.ConfigApplyTask{
			Intent: seiconfig.ConfigIntent{
				Mode:      seiconfig.ModeValidator,
				Overrides: mergeOverrides(nil, node.Spec.Overrides),
			},
		}
	}
	v := node.Spec.Validator
	return buildSharedTask(node, v.Peers, v.Snapshot, taskType)
}
