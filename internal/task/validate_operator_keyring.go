package task

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const TaskTypeValidateOperatorKeyring = "validate-operator-keyring"

// File-keyring layout (documented in the operator runbook): each keyring
// entry materializes as a "<name>.info" data key plus one or more
// "<hex>.address" name→address index keys.
const (
	keyringInfoSuffix    = ".info"
	keyringAddressSuffix = ".address"
)

type ValidateOperatorKeyringParams struct {
	SecretName           string `json:"secretName"`
	KeyName              string `json:"keyName"`
	PassphraseSecretName string `json:"passphraseSecretName"`
	PassphraseSecretKey  string `json:"passphraseSecretKey"`
	Namespace            string `json:"namespace"`
}

type validateOperatorKeyringExecution struct {
	taskBase
	params ValidateOperatorKeyringParams
	cfg    ExecutionConfig
}

func deserializeValidateOperatorKeyring(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p ValidateOperatorKeyringParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing validate-operator-keyring params: %w", err)
		}
	}
	return &validateOperatorKeyringExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

func (e *validateOperatorKeyringExecution) Execute(ctx context.Context) error {
	node, err := ResourceAs[*seiv1alpha1.SeiNode](e.cfg)
	if err != nil {
		return Terminal(err)
	}

	err = e.validate(ctx, node)
	switch {
	case err == nil:
		setOperatorKeyringCondition(node, metav1.ConditionTrue,
			seiv1alpha1.ReasonOperatorKeyringValidated,
			fmt.Sprintf("Secret pair (%q, %q) passes operator-keyring validation",
				e.params.SecretName, e.params.PassphraseSecretName))
		e.complete()
		return nil
	case isTerminal(err):
		setOperatorKeyringCondition(node, metav1.ConditionFalse, seiv1alpha1.ReasonOperatorKeyringInvalid, err.Error())
		return err
	default:
		setOperatorKeyringCondition(node, metav1.ConditionFalse, seiv1alpha1.ReasonOperatorKeyringNotReady, err.Error())
		return nil
	}
}

func (e *validateOperatorKeyringExecution) Status(_ context.Context) ExecutionStatus {
	return e.DefaultStatus()
}

// validate checks the shape of both Secrets. It deliberately does NOT
// attempt to decrypt the keyring with the passphrase — running the
// keyring backend inside the controller process would expand the
// controller's TCB. Decryption is the sidecar's startup smoke test.
func (e *validateOperatorKeyringExecution) validate(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	if e.params.SecretName == "" {
		return Terminal(fmt.Errorf("validate-operator-keyring: secretName is empty"))
	}
	if e.params.PassphraseSecretName == "" {
		return Terminal(fmt.Errorf("validate-operator-keyring: passphraseSecretName is empty"))
	}
	if e.params.PassphraseSecretKey == "" {
		return Terminal(fmt.Errorf("validate-operator-keyring: passphraseSecretKey is empty"))
	}

	keyring, err := e.getSecret(ctx, e.params.SecretName, node.Namespace)
	if err != nil {
		return err
	}
	if err := validateKeyringShape(keyring, e.params.KeyName); err != nil {
		return err
	}

	passphrase, err := e.getSecret(ctx, e.params.PassphraseSecretName, node.Namespace)
	if err != nil {
		return err
	}
	return validatePassphraseShape(passphrase, e.params.PassphraseSecretKey)
}

func (e *validateOperatorKeyringExecution) getSecret(ctx context.Context, name, namespace string) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: name, Namespace: namespace}
	if err := e.cfg.KubeClient.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("secret %q not found in namespace %q", name, namespace)
		}
		return nil, fmt.Errorf("getting Secret %q: %w", name, err)
	}
	if secret.DeletionTimestamp != nil {
		return nil, fmt.Errorf("secret %q is being deleted (deletionTimestamp=%s)", name, secret.DeletionTimestamp)
	}
	return secret, nil
}

// validateKeyringShape walks Secret data keys looking for the file-keyring
// layout. Empty Secrets and Secrets missing either suffix are
// operator-fixable defects (Terminal).
func validateKeyringShape(secret *corev1.Secret, keyName string) error {
	var infoKeys, addressKeys []string
	for k, v := range secret.Data {
		switch {
		case strings.HasSuffix(k, keyringInfoSuffix):
			if len(v) == 0 {
				return Terminal(fmt.Errorf("secret %q data key %q is empty (expected non-empty keyring entry payload)",
					secret.Name, k))
			}
			infoKeys = append(infoKeys, k)
		case strings.HasSuffix(k, keyringAddressSuffix):
			addressKeys = append(addressKeys, k)
		}
	}
	if len(infoKeys) == 0 {
		return Terminal(fmt.Errorf("secret %q has no %q data keys (expected at least one keyring entry)",
			secret.Name, "*"+keyringInfoSuffix))
	}
	if len(addressKeys) == 0 {
		return Terminal(fmt.Errorf("secret %q has no %q data keys (expected name→address index for at least one keyring entry)",
			secret.Name, "*"+keyringAddressSuffix))
	}
	if keyName != "" {
		want := keyName + keyringInfoSuffix
		if _, ok := secret.Data[want]; !ok {
			return Terminal(fmt.Errorf("secret %q is missing data key %q for keyName %q",
				secret.Name, want, keyName))
		}
	}
	return nil
}

func validatePassphraseShape(secret *corev1.Secret, dataKey string) error {
	v, ok := secret.Data[dataKey]
	if !ok {
		return Terminal(fmt.Errorf("passphrase Secret %q missing data key %q", secret.Name, dataKey))
	}
	if len(v) == 0 {
		return Terminal(fmt.Errorf("passphrase Secret %q data key %q is empty", secret.Name, dataKey))
	}
	return nil
}

func setOperatorKeyringCondition(node *seiv1alpha1.SeiNode, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               seiv1alpha1.ConditionOperatorKeyringReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: node.Generation,
	})
}
