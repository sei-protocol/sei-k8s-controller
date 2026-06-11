package planner

import (
	"fmt"

	seiconfig "github.com/sei-protocol/sei-config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

type validatorPlanner struct {
	platform platform.Config
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
	if err := validateOperatorKeyringDistinctness(node.Spec.Validator); err != nil {
		return err
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

// validateOperatorKeyringDistinctness mirrors the CRD XValidation rules
// so the planner rejects identical specs even when admission webhooks
// haven't run (in-memory specs in tests, or stale objects predating the
// CRD update). The CEL rules remain the canonical surface — these checks
// are defense in depth.
func validateOperatorKeyringDistinctness(v *seiv1alpha1.ValidatorSpec) error {
	if v.OperatorKeyring == nil || v.OperatorKeyring.Secret == nil {
		return nil
	}
	opk := v.OperatorKeyring.Secret
	pass := opk.PassphraseSecretRef.SecretName

	if opk.SecretName != "" && opk.SecretName == pass {
		return fmt.Errorf("validator: operatorKeyring data Secret %q must differ from its passphrase Secret", opk.SecretName)
	}
	if sk := v.SigningKey; sk != nil && sk.Secret != nil && sk.Secret.SecretName != "" {
		if opk.SecretName == sk.Secret.SecretName {
			return fmt.Errorf("validator: operatorKeyring Secret %q must differ from signingKey Secret", opk.SecretName)
		}
		if pass != "" && pass == sk.Secret.SecretName {
			return fmt.Errorf("validator: operatorKeyring passphrase Secret %q must differ from signingKey Secret", pass)
		}
	}
	if nk := v.NodeKey; nk != nil && nk.Secret != nil && nk.Secret.SecretName != "" {
		if opk.SecretName == nk.Secret.SecretName {
			return fmt.Errorf("validator: operatorKeyring Secret %q must differ from nodeKey Secret", opk.SecretName)
		}
		if pass != "" && pass == nk.Secret.SecretName {
			return fmt.Errorf("validator: operatorKeyring passphrase Secret %q must differ from nodeKey Secret", pass)
		}
	}
	return nil
}

func (p *validatorPlanner) BuildPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
	if node.Status.Phase == seiv1alpha1.PhaseRunning {
		return p.buildRunningPlan(node)
	}
	if isGenesisCeremonyNode(node) {
		return buildGenesisPlan(node)
	}
	v := node.Spec.Validator
	intent := &seiconfig.ConfigIntent{
		Mode:      seiconfig.ModeValidator,
		Overrides: mergeOverrides(commonOverrides(node), node.Spec.Overrides),
	}
	if NeedsBootstrap(node) {
		return buildBootstrapPlan(node, v.Snapshot, intent)
	}
	return buildBasePlan(node, v.Snapshot, intent)
}

// buildRunningPlan returns the update plan for a Running validator. The
// key-validation gates run before any STS mutation so a missing or
// malformed secret aborts with a clear controller-side error rather than
// a kubelet volume-mount failure on the recreated pod. Each gate is
// guarded by its needs* predicate so an unset field is skipped rather
// than validated as empty — operatorKeyring is independently optional, so
// a signingKey+nodeKey validator with no operatorKeyring must not gate on
// it. Mirrors buildBasePlan's guards.
func (p *validatorPlanner) buildRunningPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
	if imageDrifted(node) || sidecarImageDrifted(node, p.platform) {
		setNodeUpdateCondition(node, metav1.ConditionTrue, "UpdateStarted", imageDriftMessage(node, p.platform))
		prog := make([]string, 0, 10)
		if needsValidateSigningKey(node) {
			prog = append(prog, task.TaskTypeValidateSigningKey)
		}
		if needsValidateNodeKey(node) {
			prog = append(prog, task.TaskTypeValidateNodeKey)
		}
		if needsValidateOperatorKeyring(node) {
			prog = append(prog, task.TaskTypeValidateOperatorKeyring)
		}
		prog = append(prog,
			task.TaskTypeApplyStatefulSet,
			task.TaskTypeApplyService,
			TaskConfigPatch,
			TaskConfigValidate,
			task.TaskTypeReplacePod,
			task.TaskTypeObserveImage,
			TaskMarkReady,
		)
		return assembleUpdatePlan(node, prog, externalAddressPatch(node))
	}
	if sidecarNeedsReapproval(node) {
		return buildMarkReadyPlan(node)
	}
	return nil, nil
}
