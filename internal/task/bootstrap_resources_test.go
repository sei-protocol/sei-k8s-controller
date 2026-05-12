package task

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	bootstrapTestPassphraseSecret = "validator-0-passphrase"
	bootstrapTestNodeKeySecret    = "validator-0-nodekey"
	bootstrapTestSidecarContainer = "sei-sidecar"
	bootstrapTestDataVolumeName   = "data"
)

func validatorNodeWithSecrets(signingKeySecret, operatorKeyringSecret, passphraseSecret string) *seiv1alpha1.SeiNode {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: opkValidatorName, Namespace: opkNs},
		Spec: seiv1alpha1.SeiNodeSpec{
			Validator: &seiv1alpha1.ValidatorSpec{},
		},
	}
	if signingKeySecret != "" {
		node.Spec.Validator.SigningKey = &seiv1alpha1.SigningKeySource{
			Secret: &seiv1alpha1.SecretSigningKeySource{SecretName: signingKeySecret},
		}
	}
	if operatorKeyringSecret != "" {
		node.Spec.Validator.OperatorKeyring = &seiv1alpha1.OperatorKeyringSource{
			Secret: &seiv1alpha1.SecretOperatorKeyringSource{
				SecretName: operatorKeyringSecret,
				PassphraseSecretRef: seiv1alpha1.PassphraseSecretRef{
					SecretName: passphraseSecret,
					Key:        opkPassphraseKey,
				},
			},
		}
	}
	return node
}

func TestAssertNoValidatorSecretsOnBootstrapPod_NoValidator(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "fn-0", Namespace: opkNs},
	}
	if err := assertNoValidatorSecretsOnBootstrapPod(node, &corev1.PodSpec{}); err != nil {
		t.Fatalf("expected nil error for non-validator node, got %v", err)
	}
}

func TestAssertNoValidatorSecretsOnBootstrapPod_CleanPodSpec(t *testing.T) {
	node := validatorNodeWithSecrets("sk", "opk", "opk-pass")
	spec := &corev1.PodSpec{
		Volumes: []corev1.Volume{
			{Name: bootstrapTestDataVolumeName, VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-validator-0"},
			}},
		},
	}
	if err := assertNoValidatorSecretsOnBootstrapPod(node, spec); err != nil {
		t.Fatalf("expected nil error for clean bootstrap pod-spec, got %v", err)
	}
}

func TestAssertNoValidatorSecretsOnBootstrapPod_SigningKeyVolume_Rejects(t *testing.T) {
	node := validatorNodeWithSecrets("validator-0-signing", "opk", "opk-pass")
	spec := &corev1.PodSpec{
		Volumes: []corev1.Volume{
			{Name: "signing-key", VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: "validator-0-signing"},
			}},
		},
	}
	err := assertNoValidatorSecretsOnBootstrapPod(node, spec)
	if err == nil {
		t.Fatal("expected error for signing-key volume on bootstrap pod, got nil")
	}
	if !strings.Contains(err.Error(), "signing-key") {
		t.Fatalf("expected error to mention signing-key, got: %v", err)
	}
}

func TestAssertNoValidatorSecretsOnBootstrapPod_OperatorKeyringVolume_Rejects(t *testing.T) {
	node := validatorNodeWithSecrets("sk", "validator-0-opk", "opk-pass")
	spec := &corev1.PodSpec{
		Volumes: []corev1.Volume{
			{Name: "operator-keyring", VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: "validator-0-opk"},
			}},
		},
	}
	err := assertNoValidatorSecretsOnBootstrapPod(node, spec)
	if err == nil {
		t.Fatal("expected error for operator-keyring volume on bootstrap pod, got nil")
	}
	if !strings.Contains(err.Error(), "operator-keyring") {
		t.Fatalf("expected error to mention operator-keyring, got: %v", err)
	}
}

func TestAssertNoValidatorSecretsOnBootstrapPod_PassphraseVolume_Rejects(t *testing.T) {
	node := validatorNodeWithSecrets("sk", "opk", bootstrapTestPassphraseSecret)
	spec := &corev1.PodSpec{
		Volumes: []corev1.Volume{
			{Name: "passphrase-vol", VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: bootstrapTestPassphraseSecret},
			}},
		},
	}
	err := assertNoValidatorSecretsOnBootstrapPod(node, spec)
	if err == nil {
		t.Fatal("expected error for passphrase volume on bootstrap pod, got nil")
	}
	if !strings.Contains(err.Error(), "operator-keyring-passphrase") {
		t.Fatalf("expected error to mention operator-keyring-passphrase, got: %v", err)
	}
}

func TestAssertNoValidatorSecretsOnBootstrapPod_PassphraseEnv_Rejects(t *testing.T) {
	node := validatorNodeWithSecrets("sk", "opk", bootstrapTestPassphraseSecret)
	spec := &corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name: bootstrapTestSidecarContainer,
				Env: []corev1.EnvVar{
					{Name: "SEI_KEYRING_PASSPHRASE", ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: bootstrapTestPassphraseSecret},
							Key:                  opkPassphraseKey,
						},
					}},
				},
			},
		},
	}
	err := assertNoValidatorSecretsOnBootstrapPod(node, spec)
	if err == nil {
		t.Fatal("expected error for passphrase env on bootstrap pod, got nil")
	}
	if !strings.Contains(err.Error(), "operator-keyring-passphrase") || !strings.Contains(err.Error(), bootstrapTestSidecarContainer) {
		t.Fatalf("expected error to mention passphrase kind and container name, got: %v", err)
	}
}

func TestAssertNoValidatorSecretsOnBootstrapPod_PassphraseEnvInInitContainer_Rejects(t *testing.T) {
	node := validatorNodeWithSecrets("sk", "opk", bootstrapTestPassphraseSecret)
	spec := &corev1.PodSpec{
		InitContainers: []corev1.Container{
			{
				Name: bootstrapTestSidecarContainer,
				Env: []corev1.EnvVar{
					{Name: "SEI_KEYRING_PASSPHRASE", ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: bootstrapTestPassphraseSecret},
							Key:                  opkPassphraseKey,
						},
					}},
				},
			},
		},
	}
	if err := assertNoValidatorSecretsOnBootstrapPod(node, spec); err == nil {
		t.Fatal("expected error for passphrase env on init container, got nil")
	}
}

func TestAssertNoValidatorSecretsOnBootstrapPod_PassphraseEnvFrom_Rejects(t *testing.T) {
	node := validatorNodeWithSecrets("sk", "opk", bootstrapTestPassphraseSecret)
	spec := &corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name: bootstrapTestSidecarContainer,
				EnvFrom: []corev1.EnvFromSource{
					{SecretRef: &corev1.SecretEnvSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: bootstrapTestPassphraseSecret},
					}},
				},
			},
		},
	}
	err := assertNoValidatorSecretsOnBootstrapPod(node, spec)
	if err == nil {
		t.Fatal("expected error for passphrase envFrom on bootstrap pod, got nil")
	}
	if !strings.Contains(err.Error(), "operator-keyring-passphrase") || !strings.Contains(err.Error(), bootstrapTestSidecarContainer) {
		t.Fatalf("expected error to mention passphrase kind and container name, got: %v", err)
	}
	if !strings.Contains(err.Error(), "envFrom") {
		t.Fatalf("expected error to mention envFrom path, got: %v", err)
	}
}

func TestAssertNoValidatorSecretsOnBootstrapPod_NodeKeyExcluded(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: opkValidatorName, Namespace: opkNs},
		Spec: seiv1alpha1.SeiNodeSpec{
			Validator: &seiv1alpha1.ValidatorSpec{
				NodeKey: &seiv1alpha1.NodeKeySource{
					Secret: &seiv1alpha1.SecretNodeKeySource{SecretName: bootstrapTestNodeKeySecret},
				},
			},
		},
	}
	// node-key Secret on bootstrap is a design bug elsewhere but NOT a
	// slashing risk — this assertion intentionally does not catch it.
	spec := &corev1.PodSpec{
		Volumes: []corev1.Volume{
			{Name: "node-key", VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: bootstrapTestNodeKeySecret},
			}},
		},
	}
	if err := assertNoValidatorSecretsOnBootstrapPod(node, spec); err != nil {
		t.Fatalf("node-key is deliberately not part of the assertion, got: %v", err)
	}
}
