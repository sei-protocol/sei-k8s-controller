package task

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
)

const TaskTypeWaitForSidecarTLSSecret = "wait-for-sidecar-tls-secret"

// certManagerFailedReason is the terminal failure reason on a
// Certificate's Ready condition. Other False reasons (Issuing, etc.)
// are transient and the task continues to poll.
const certManagerFailedReason = "Failed"

type WaitForSidecarTLSSecretParams struct {
	NodeName  string `json:"nodeName"`
	Namespace string `json:"namespace"`
}

type waitForSidecarTLSSecretExecution struct {
	taskBase
	params WaitForSidecarTLSSecretParams
	cfg    ExecutionConfig
}

func deserializeWaitForSidecarTLSSecret(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p WaitForSidecarTLSSecretParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing wait-for-sidecar-tls-secret params: %w", err)
		}
	}
	return &waitForSidecarTLSSecretExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

// Execute polls the cert-manager-managed Secret for populated tls.crt
// AND tls.key. Returns nil while either is absent so the task stays
// Running and the executor re-invokes on the next reconcile. Bypasses
// the cache because the Secret was just requested by the preceding
// apply-sidecar-cert task.
//
// Operator-visible failure surface: if the underlying Certificate's
// Ready condition reports terminal failure (e.g., the referenced
// Issuer/ClusterIssuer doesn't exist), the task returns Terminal so
// the plan fails fast with UpdateFailed rather than polling forever.
func (e *waitForSidecarTLSSecretExecution) Execute(ctx context.Context) error {
	node, err := ResourceAs[*seiv1alpha1.SeiNode](e.cfg)
	if err != nil {
		return Terminal(err)
	}

	sec := &corev1.Secret{}
	key := types.NamespacedName{Name: noderesource.SidecarTLSSecretName(node), Namespace: node.Namespace}
	if err := e.cfg.APIReader.Get(ctx, key, sec); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("getting sidecar TLS Secret: %w", err)
		}
		// Secret absent — check Certificate for terminal failure.
		return e.checkCertificateFailure(ctx, node)
	}
	if len(sec.Data[corev1.TLSCertKey]) == 0 || len(sec.Data[corev1.TLSPrivateKeyKey]) == 0 {
		return e.checkCertificateFailure(ctx, node)
	}

	e.complete()
	return nil
}

// checkCertificateFailure reads the cert-manager Certificate and
// returns Terminal if its Ready condition is False with Reason=Failed.
// All other states return nil — the task continues polling.
func (e *waitForSidecarTLSSecretExecution) checkCertificateFailure(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	cert := &unstructured.Unstructured{}
	cert.SetAPIVersion("cert-manager.io/v1")
	cert.SetKind("Certificate")
	key := types.NamespacedName{Name: noderesource.SidecarTLSSecretName(node), Namespace: node.Namespace}
	if err := e.cfg.APIReader.Get(ctx, key, cert); err != nil {
		// Certificate missing (transient: not yet applied or being
		// deleted) or any other read error: do not fail the plan.
		return nil
	}
	conditions, found, err := unstructured.NestedSlice(cert.Object, "status", "conditions")
	if err != nil || !found {
		return nil
	}
	for _, raw := range conditions {
		cond, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := cond["type"].(string); t != "Ready" {
			continue
		}
		status, _ := cond["status"].(string)
		reason, _ := cond["reason"].(string)
		message, _ := cond["message"].(string)
		if status == "False" && reason == certManagerFailedReason {
			return Terminal(fmt.Errorf("cert-manager Certificate %s/%s Failed: %s",
				key.Namespace, key.Name, message))
		}
		return nil
	}
	return nil
}

func (e *waitForSidecarTLSSecretExecution) Status(_ context.Context) ExecutionStatus {
	return e.DefaultStatus()
}
