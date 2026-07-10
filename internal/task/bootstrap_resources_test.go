package task

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest"
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

// findBootstrapContainer returns the container/init-container with the given
// name from a bootstrap pod-spec, or nil.
func findBootstrapContainer(spec corev1.PodSpec, name string) *corev1.Container {
	all := append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...)
	for i := range all {
		if all[i].Name == name {
			return &all[i]
		}
	}
	return nil
}

func bootstrapEnv(c *corev1.Container, name string) string {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

// TestBootstrapJob_HomeAndDataDir asserts the #449 fix shape holds on the
// bootstrap pod: seid start and seid init target an explicit --home
// <bootstrapDataDir> (never $HOME), and HOME is set to the data dir's PARENT
// (platform.HomeDir), never the data dir itself. A regression here — HOME left
// equal to the data dir, or a lingering `--home "$HOME"` — reintroduces the
// nesting bug and, mid-migration, risks a validator-data wipe.
func TestBootstrapJob_HomeAndDataDir(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "v-0", Namespace: testReplaceNs},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:   "sei-test",
			Image:     "ghcr.io/sei-protocol/seid:latest",
			Validator: &seiv1alpha1.ValidatorSpec{},
		},
	}
	snap := &seiv1alpha1.SnapshotSource{S3: &seiv1alpha1.S3SnapshotSource{TargetHeight: 12345}}

	job, err := GenerateBootstrapJob(node, snap, platformtest.Config())
	if err != nil {
		t.Fatalf("GenerateBootstrapJob error: %v", err)
	}
	spec := job.Spec.Template.Spec

	seid := findBootstrapContainer(spec, "seid")
	if seid == nil {
		t.Fatal("seid container not found")
	}
	seidScript := strings.Join(seid.Args, " ")
	if !strings.Contains(seidScript, `seid start --home "`+bootstrapDataDir+`" --halt-height`) {
		t.Errorf("seid start must use explicit --home %q; got: %s", bootstrapDataDir, seidScript)
	}
	if strings.Contains(seidScript, "$HOME") {
		t.Errorf("seid start script must not reference $HOME; got: %s", seidScript)
	}
	if got := bootstrapEnv(seid, "HOME"); got != platform.HomeDir {
		t.Errorf("seid HOME = %q, want %q (parent of data dir)", got, platform.HomeDir)
	}
	if platform.HomeDir == bootstrapDataDir {
		t.Fatalf("HOME must never equal the data dir (#449)")
	}

	seidInit := findBootstrapContainer(spec, "seid-init")
	if seidInit == nil {
		t.Fatal("seid-init container not found")
	}
	initScript := seidInit.Command[2]
	if !strings.Contains(initScript, `--home "`+bootstrapDataDir+`"`) {
		t.Errorf("seid init must use explicit --home %q; got: %s", bootstrapDataDir, initScript)
	}
	if !strings.Contains(initScript, `[ -f "`+bootstrapDataDir+`/config/genesis.json" ]`) {
		t.Errorf("genesis guard must check the data dir, not $HOME; got: %s", initScript)
	}
	if strings.Contains(initScript, "$HOME") {
		t.Errorf("seid-init script must not reference $HOME; got: %s", initScript)
	}
	if got := bootstrapEnv(seidInit, "HOME"); got != platform.HomeDir {
		t.Errorf("seid-init HOME = %q, want %q", got, platform.HomeDir)
	}
}
