package planner

import (
	"fmt"

	seiconfig "github.com/sei-protocol/sei-config"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

type validatorPlanner struct {
}

func (p *validatorPlanner) Mode() string { return string(seiconfig.ModeValidator) }

func (p *validatorPlanner) Validate(node *seiv1alpha1.SeiNode) error {
	if node.Spec.Validator == nil {
		return fmt.Errorf("validator sub-spec is nil")
	}
	if sk := node.Spec.Validator.SigningKey; sk != nil {
		if node.Spec.Validator.GenesisCeremony != nil {
			return fmt.Errorf("validator: signingKey is mutually exclusive with genesisCeremony")
		}
		if sk.Secret == nil || sk.Secret.SecretName == "" {
			return fmt.Errorf("validator: signingKey.secret.secretName is required")
		}
		nk := node.Spec.Validator.NodeKey
		if nk == nil {
			return fmt.Errorf("validator: nodeKey is required when signingKey is set")
		}
		if nk.Secret == nil || nk.Secret.SecretName == "" {
			return fmt.Errorf("validator: nodeKey.secret.secretName is required")
		}
		if nk.Secret.SecretName == sk.Secret.SecretName {
			return fmt.Errorf("validator: signingKey and nodeKey must reference distinct Secrets")
		}
	}
	if node.Spec.Validator.NodeKey != nil && node.Spec.Validator.SigningKey == nil {
		return fmt.Errorf("validator: nodeKey requires signingKey to be set")
	}
	if gc := node.Spec.Validator.GenesisCeremony; gc != nil {
		if gc.ChainID == "" {
			return fmt.Errorf("validator: genesisCeremony.chainId is required")
		}
		if gc.StakingAmount == "" {
			return fmt.Errorf("validator: genesisCeremony.stakingAmount is required")
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

func (p *validatorPlanner) BuildPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
	if node.Status.Phase == seiv1alpha1.PhaseRunning {
		return buildRunningPlan(node)
	}
	if isGenesisCeremonyNode(node) {
		return buildGenesisPlan(node)
	}
	v := node.Spec.Validator
	params := &task.ConfigApplyParams{
		Mode:      string(seiconfig.ModeValidator),
		Overrides: mergeOverrides(commonOverrides(node), node.Spec.Overrides),
	}
	if NeedsBootstrap(node) {
		return buildBootstrapPlan(node, node.Spec.Peers, v.Snapshot, params)
	}
	return buildBasePlan(node, node.Spec.Peers, v.Snapshot, params)
}
