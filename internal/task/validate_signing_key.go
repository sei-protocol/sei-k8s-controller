package task

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const TaskTypeValidateSigningKey = "validate-signing-key"

const privValidatorKeyDataKey = "priv_validator_key.json"

type ValidateSigningKeyParams struct {
	SecretName string `json:"secretName"`
	Namespace  string `json:"namespace"`
}

type validateSigningKeyExecution struct {
	taskBase
	params ValidateSigningKeyParams
	cfg    ExecutionConfig
}

func deserializeValidateSigningKey(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p ValidateSigningKeyParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing validate-signing-key params: %w", err)
		}
	}
	return &validateSigningKeyExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

func (e *validateSigningKeyExecution) Execute(ctx context.Context) error {
	node, err := ResourceAs[*seiv1alpha1.SeiNode](e.cfg)
	if err != nil {
		return Terminal(err)
	}

	err = e.validate(ctx, node)
	switch {
	case err == nil:
		setSigningKeyCondition(node, metav1.ConditionTrue, seiv1alpha1.ReasonSigningKeyValidated,
			fmt.Sprintf("Secret %q passes all signing-key validation rules", e.params.SecretName))
		e.complete()
		return nil
	case isTerminal(err):
		setSigningKeyCondition(node, metav1.ConditionFalse, seiv1alpha1.ReasonSigningKeyInvalid, err.Error())
		return err
	default:
		setSigningKeyCondition(node, metav1.ConditionFalse, seiv1alpha1.ReasonSigningKeyNotReady, err.Error())
		return nil
	}
}

// validate returns Terminal for operator-fixable defects (malformed key,
// missing data) and plain errors for transient conditions (Secret not yet
// applied). The executor retries plain errors and fails the plan on Terminal.
func (e *validateSigningKeyExecution) validate(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	name := e.params.SecretName
	if name == "" {
		return Terminal(fmt.Errorf("validate-signing-key: secretName is empty"))
	}

	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: name, Namespace: node.Namespace}
	if err := e.cfg.KubeClient.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("secret %q not found in namespace %q", name, node.Namespace)
		}
		return fmt.Errorf("getting Secret %q: %w", name, err)
	}

	if secret.DeletionTimestamp != nil {
		return fmt.Errorf("secret %q is being deleted (deletionTimestamp=%s)", name, secret.DeletionTimestamp)
	}

	raw, ok := secret.Data[privValidatorKeyDataKey]
	if !ok || len(raw) == 0 {
		return Terminal(fmt.Errorf("secret %q missing required data key %q", name, privValidatorKeyDataKey))
	}

	if err := validateTendermintValidatorKey(raw); err != nil {
		return Terminal(fmt.Errorf("secret %q data key %q is not a valid Tendermint validator key: %w",
			name, privValidatorKeyDataKey, err))
	}
	return nil
}

func (e *validateSigningKeyExecution) Status(_ context.Context) ExecutionStatus {
	return e.DefaultStatus()
}

// tendermintValidatorKey is the minimal shape required for the pre-flight
// check; algorithm-specific value blobs are opaque here and validated by seid.
type tendermintValidatorKey struct {
	Address string `json:"address"`
	PubKey  struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"pub_key"`
	PrivKey struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"priv_key"`
}

// validateTendermintValidatorKey checks shape only. Cryptographic validity
// is seid's responsibility at startup.
func validateTendermintValidatorKey(raw []byte) error {
	var k tendermintValidatorKey
	if err := json.Unmarshal(raw, &k); err != nil {
		return fmt.Errorf("not valid JSON: %w", err)
	}
	if k.Address == "" {
		return fmt.Errorf("missing address")
	}
	if k.PubKey.Type == "" || k.PubKey.Value == "" {
		return fmt.Errorf("missing pub_key.type or pub_key.value")
	}
	if k.PrivKey.Type == "" || k.PrivKey.Value == "" {
		return fmt.Errorf("missing priv_key.type or priv_key.value")
	}
	return nil
}

func setSigningKeyCondition(node *seiv1alpha1.SeiNode, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               seiv1alpha1.ConditionSigningKeyReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: node.Generation,
	})
}
