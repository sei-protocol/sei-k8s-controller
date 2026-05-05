package task

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest"
)

const testBootstrapImageV1 = "sei:bootstrap-v1"

func bootstrapTestReplayerNode() *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "test-replayer", Namespace: "default", Generation: 1},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "pacific-1",
			Image:   "sei:latest",
			Replayer: &seiv1alpha1.ReplayerSpec{
				Snapshot: seiv1alpha1.SnapshotSource{
					S3: &seiv1alpha1.S3SnapshotSource{TargetHeight: 100000000},
				},
			},
			Sidecar: &seiv1alpha1.SidecarConfig{Image: "sidecar:latest", Port: 7777},
		},
	}
}

func TestTaskGenerateBootstrapJob(t *testing.T) {
	node := bootstrapTestReplayerNode()
	node.Spec.Replayer.Snapshot.BootstrapImage = testBootstrapImageV1
	in := nodeToBootstrapInputs(node, node.Spec.SnapshotSource())
	job, err := GenerateBootstrapJob(in, platformtest.Config())
	if err != nil {
		t.Fatalf("GenerateBootstrapJob error: %v", err)
	}

	wantName := "test-replayer-bootstrap"
	if job.Name != wantName {
		t.Errorf("Job name = %q, want %q", job.Name, wantName)
	}

	spec := job.Spec.Template.Spec
	if spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q, want Never", spec.RestartPolicy)
	}
	if spec.Containers[0].Image != testBootstrapImageV1 {
		t.Errorf("main container image = %q, want %q", spec.Containers[0].Image, testBootstrapImageV1)
	}
}

func TestTaskGenerateBootstrapJob_NilSnapshot(t *testing.T) {
	node := bootstrapTestReplayerNode()
	in := nodeToBootstrapInputs(node, nil)
	if _, err := GenerateBootstrapJob(in, platformtest.Config()); err == nil {
		t.Fatal("expected error for nil snapshot (HaltHeight=0), got nil")
	}
}

func TestTaskGenerateBootstrapJob_SidecarResources(t *testing.T) {
	node := bootstrapTestReplayerNode()
	node.Spec.Replayer.Snapshot.BootstrapImage = testBootstrapImageV1
	node.Spec.Sidecar = &seiv1alpha1.SidecarConfig{
		Resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
		},
	}
	in := nodeToBootstrapInputs(node, node.Spec.SnapshotSource())
	job, err := GenerateBootstrapJob(in, platformtest.Config())
	if err != nil {
		t.Fatalf("GenerateBootstrapJob error: %v", err)
	}
	spec := job.Spec.Template.Spec

	sc := spec.InitContainers[1]
	cpuReq := sc.Resources.Requests[corev1.ResourceCPU]
	if cpuReq.String() != "500m" {
		t.Errorf("sidecar CPU request = %q, want %q", cpuReq.String(), "500m")
	}
}

// TestTaskGenerateBootstrapJob_NeverHasIdentityVolumes is the safety
// regression guard for the load-bearing invariants on validator identity
// material. The bootstrap pod must never carry the consensus signing key
// (slashing risk, LLD §3) AND must never carry the validator's permanent
// node key (peer-reputation pollution risk — bootstrap pods crash, halt,
// and restart, and that misbehavior must not attribute to the validator's
// permanent libp2p identity).
func TestTaskGenerateBootstrapJob_NeverHasIdentityVolumes(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-0", Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "atlantic-2",
			Image:   "ghcr.io/sei-protocol/seid:latest",
			Validator: &seiv1alpha1.ValidatorSpec{
				Snapshot: &seiv1alpha1.SnapshotSource{
					BootstrapImage: testBootstrapImageV1,
					S3:             &seiv1alpha1.S3SnapshotSource{TargetHeight: 100000000},
				},
				SigningKey: &seiv1alpha1.SigningKeySource{
					Secret: &seiv1alpha1.SecretSigningKeySource{SecretName: "validator-0-key"},
				},
				NodeKey: &seiv1alpha1.NodeKeySource{
					Secret: &seiv1alpha1.SecretNodeKeySource{SecretName: "validator-0-nodekey"},
				},
			},
		},
	}
	in := nodeToBootstrapInputs(node, node.Spec.SnapshotSource())
	job, err := GenerateBootstrapJob(in, platformtest.Config())
	if err != nil {
		t.Fatalf("GenerateBootstrapJob error: %v", err)
	}
	if err := assertNoSigningKeyOnBootstrapPod(node, &job.Spec.Template.Spec); err != nil {
		t.Fatalf("assertNoSigningKeyOnBootstrapPod: %v", err)
	}
	forbiddenVolumeNames := map[string]string{
		"signing-key": "consensus signing material — slashing risk (LLD §3)",
		"node-key":    "validator permanent node ID — peer-reputation pollution risk",
	}
	forbiddenSecretNames := map[string]string{
		"validator-0-key":     "consensus signing Secret",
		"validator-0-nodekey": "validator node-key Secret",
	}
	for _, v := range job.Spec.Template.Spec.Volumes {
		if reason, forbidden := forbiddenVolumeNames[v.Name]; forbidden {
			t.Fatalf("bootstrap Job pod-spec must NEVER include volume %q (%s); found %+v", v.Name, reason, v)
		}
		if v.Secret != nil {
			if reason, forbidden := forbiddenSecretNames[v.Secret.SecretName]; forbidden {
				t.Fatalf("bootstrap Job pod-spec must NEVER mount Secret %q (%s); found volume %q",
					v.Secret.SecretName, reason, v.Name)
			}
		}
	}
	for _, c := range job.Spec.Template.Spec.Containers {
		for _, m := range c.VolumeMounts {
			if reason, forbidden := forbiddenVolumeNames[m.Name]; forbidden {
				t.Fatalf("bootstrap Job container %q must NEVER mount %q (%s)", c.Name, m.Name, reason)
			}
		}
	}
}
