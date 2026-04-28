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

const TaskTypeValidateNodeKey = "validate-node-key"

const nodeKeyDataKey = "node_key.json"

// tendermintNodeKey is the minimal shape required for the pre-flight check.
// node_key.json carries only the priv_key (no address, no pub_key — node ID
// is derived from priv_key by libp2p at runtime).
type tendermintNodeKey struct {
	PrivKey struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"priv_key"`
}

type ValidateNodeKeyParams struct {
	SecretName string `json:"secretName"`
	Namespace  string `json:"namespace"`
}

type validateNodeKeyExecution struct {
	taskBase
	params ValidateNodeKeyParams
	cfg    ExecutionConfig
}

func deserializeValidateNodeKey(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p ValidateNodeKeyParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing validate-node-key params: %w", err)
		}
	}
	return &validateNodeKeyExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

func (e *validateNodeKeyExecution) Execute(ctx context.Context) error {
	node, err := ResourceAs[*seiv1alpha1.SeiNode](e.cfg)
	if err != nil {
		return Terminal(err)
	}

	err = e.validate(ctx, node)
	switch {
	case err == nil:
		setNodeKeyCondition(node, metav1.ConditionTrue, seiv1alpha1.ReasonNodeKeyValidated,
			fmt.Sprintf("Secret %q passes all node-key validation rules", e.params.SecretName))
		e.complete()
		return nil
	case isTerminal(err):
		setNodeKeyCondition(node, metav1.ConditionFalse, seiv1alpha1.ReasonNodeKeyInvalid, err.Error())
		return err
	default:
		setNodeKeyCondition(node, metav1.ConditionFalse, seiv1alpha1.ReasonNodeKeyNotReady, err.Error())
		return nil
	}
}

// validate returns Terminal for operator-fixable defects (malformed key,
// missing data) and plain errors for transient conditions (Secret not yet
// applied). The executor retries plain errors and fails the plan on Terminal.
func (e *validateNodeKeyExecution) validate(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	name := e.params.SecretName
	if name == "" {
		return Terminal(fmt.Errorf("validate-node-key: secretName is empty"))
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

	raw, ok := secret.Data[nodeKeyDataKey]
	if !ok || len(raw) == 0 {
		return Terminal(fmt.Errorf("secret %q missing required data key %q", name, nodeKeyDataKey))
	}

	if err := validateTendermintNodeKey(raw); err != nil {
		return Terminal(fmt.Errorf("secret %q data key %q is not a valid Tendermint node key: %w",
			name, nodeKeyDataKey, err))
	}
	return nil
}

func (e *validateNodeKeyExecution) Status(_ context.Context) ExecutionStatus {
	return e.DefaultStatus()
}

// validateTendermintNodeKey checks shape only. Cryptographic validity is
// libp2p's responsibility at peer-handshake time.
func validateTendermintNodeKey(raw []byte) error {
	var k tendermintNodeKey
	if err := json.Unmarshal(raw, &k); err != nil {
		return fmt.Errorf("not valid JSON: %w", err)
	}
	if k.PrivKey.Type == "" || k.PrivKey.Value == "" {
		return fmt.Errorf("missing priv_key.type or priv_key.value")
	}
	return nil
}

func setNodeKeyCondition(node *seiv1alpha1.SeiNode, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               seiv1alpha1.ConditionNodeKeyReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: node.Generation,
	})
}
