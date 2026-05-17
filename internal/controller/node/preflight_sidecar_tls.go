package node

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
)

// reconcileSidecarTLSReady publishes status.sidecarTLS and sets the
// SidecarTLSSecretReady condition based on the referenced Secret's
// presence + cert validity. Clears both when TLS is disabled.
// Mutations are in-memory; caller's Status().Patch flushes.
func (r *SeiNodeReconciler) reconcileSidecarTLSReady(ctx context.Context, node *seiv1alpha1.SeiNode) {
	if !noderesource.SidecarTLSEnabled(node) {
		apimeta.RemoveStatusCondition(&node.Status.Conditions, seiv1alpha1.ConditionSidecarTLSSecretReady)
		node.Status.SidecarTLS = nil
		return
	}

	required := requiredDNSNames(node)
	node.Status.SidecarTLS = &seiv1alpha1.SidecarTLSStatus{
		SecretName:       node.Spec.Sidecar.TLS.SecretName,
		RequiredDNSNames: required,
	}

	reason, msg := validateTLSSecret(ctx, r.APIReader, node, required)
	status := metav1.ConditionFalse
	if reason == seiv1alpha1.ReasonTLSSecretReady {
		status = metav1.ConditionTrue
	}
	apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               seiv1alpha1.ConditionSidecarTLSSecretReady,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: node.Generation,
	})
}

// requiredDNSNames returns the headless service + pod-0 DNS names the
// proxy is reached on from inside the cluster.
func requiredDNSNames(node *seiv1alpha1.SeiNode) []string {
	return []string{
		fmt.Sprintf("%s.%s.svc.cluster.local", node.Name, node.Namespace),
		fmt.Sprintf("%s-0.%s.%s.svc.cluster.local", node.Name, node.Name, node.Namespace),
	}
}

// validateTLSSecret returns the SidecarTLSSecretReady reason + message
// for the Secret named in spec.sidecar.tls.secretName.
func validateTLSSecret(
	ctx context.Context,
	reader client.Reader,
	node *seiv1alpha1.SeiNode,
	required []string,
) (reason, msg string) {
	name := node.Spec.Sidecar.TLS.SecretName

	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: name, Namespace: node.Namespace}
	if err := reader.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return seiv1alpha1.ReasonTLSSecretNotFound,
				fmt.Sprintf("secret %q not found in namespace %q", name, node.Namespace)
		}
		return seiv1alpha1.ReasonTLSSecretNotFound,
			fmt.Sprintf("transient error getting Secret %q: %v", name, err)
	}

	if secret.Type != corev1.SecretTypeTLS {
		return seiv1alpha1.ReasonTLSSecretMalformed,
			fmt.Sprintf("secret %q has type %q; want %q", name, secret.Type, corev1.SecretTypeTLS)
	}

	crtPEM := secret.Data[corev1.TLSCertKey]
	keyPEM := secret.Data[corev1.TLSPrivateKeyKey]
	if len(crtPEM) == 0 {
		return seiv1alpha1.ReasonTLSSecretMalformed,
			fmt.Sprintf("secret %q has empty %s", name, corev1.TLSCertKey)
	}
	if len(keyPEM) == 0 {
		return seiv1alpha1.ReasonTLSSecretMalformed,
			fmt.Sprintf("secret %q has empty %s", name, corev1.TLSPrivateKeyKey)
	}

	block, _ := pem.Decode(crtPEM)
	if block == nil {
		return seiv1alpha1.ReasonTLSSecretMalformed,
			fmt.Sprintf("secret %q %s is not valid PEM", name, corev1.TLSCertKey)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return seiv1alpha1.ReasonTLSSecretMalformed,
			fmt.Sprintf("secret %q %s does not parse: %v", name, corev1.TLSCertKey, err)
	}

	if missing := missingSANs(cert.DNSNames, required); len(missing) > 0 {
		return seiv1alpha1.ReasonTLSSecretSANsMismatch,
			fmt.Sprintf("secret %q cert SANs missing required DNS names: %v (cert has: %v)",
				name, missing, cert.DNSNames)
	}

	return seiv1alpha1.ReasonTLSSecretReady,
		fmt.Sprintf("secret %q is well-formed and SANs cover required DNS names", name)
}

// missingSANs returns the elements of required that are absent from have.
func missingSANs(have, required []string) []string {
	var missing []string
	for _, r := range required {
		if !slices.Contains(have, r) {
			missing = append(missing, r)
		}
	}
	return missing
}
