package task

import (
	"strings"
	"testing"

	seiconfig "github.com/sei-protocol/sei-config"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest"
)

const (
	testBootstrapImageV1   = "sei:bootstrap-v1"
	testValidatorSecret    = "validator-0-key"
	testValidatorNodeKey   = "validator-0-nodekey"
	testCustomSidecarImage = "ghcr.io/sei/sidecar:test"
	testCustomSidecarPort  = int32(9099)

	testNamespaceDefault = "default"
	testChainPacific     = "pacific-1"
	testChainAtlantic    = "atlantic-2"
	testImageLatest      = "sei:latest"
	testValidatorName    = "validator-0"
	testExporterName     = "exporter"
	testDataVolumeName   = "data"
)

func bootstrapTestReplayerNode() *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "test-replayer", Namespace: testNamespaceDefault, Generation: 1},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: testChainPacific,
			Image:   testImageLatest,
			Replayer: &seiv1alpha1.ReplayerSpec{
				Snapshot: seiv1alpha1.SnapshotSource{
					S3: &seiv1alpha1.S3SnapshotSource{TargetHeight: 100000000},
				},
			},
			Sidecar: &seiv1alpha1.SidecarConfig{Image: "sidecar:latest", Port: 7777},
		},
	}
}

// --- nodeToBootstrapInputs ---

func TestNodeToBootstrapInputs(t *testing.T) {
	withValidator := func(secretName, nodeKeyName string) *seiv1alpha1.SeiNode {
		return &seiv1alpha1.SeiNode{
			ObjectMeta: metav1.ObjectMeta{Name: "v0", Namespace: "consensus"},
			Spec: seiv1alpha1.SeiNodeSpec{
				ChainID: testChainAtlantic,
				Image:   "sei:v1",
				Validator: &seiv1alpha1.ValidatorSpec{
					Snapshot: &seiv1alpha1.SnapshotSource{
						BootstrapImage: testBootstrapImageV1,
						S3:             &seiv1alpha1.S3SnapshotSource{TargetHeight: 5000},
					},
					SigningKey: &seiv1alpha1.SigningKeySource{
						Secret: &seiv1alpha1.SecretSigningKeySource{SecretName: secretName},
					},
					NodeKey: &seiv1alpha1.NodeKeySource{
						Secret: &seiv1alpha1.SecretNodeKeySource{SecretName: nodeKeyName},
					},
				},
			},
		}
	}

	tests := []struct {
		name     string
		node     *seiv1alpha1.SeiNode
		snap     *seiv1alpha1.SnapshotSource
		validate func(t *testing.T, got BootstrapPodInputs)
	}{
		{
			name: "validator with bootstrap image and identity secrets",
			node: withValidator(testValidatorSecret, testValidatorNodeKey),
			snap: withValidator(testValidatorSecret, testValidatorNodeKey).Spec.SnapshotSource(),
			validate: func(t *testing.T, got BootstrapPodInputs) {
				if got.Image != testBootstrapImageV1 {
					t.Errorf("Image = %q, want snapshot BootstrapImage %q", got.Image, testBootstrapImageV1)
				}
				if got.Mode != string(seiconfig.ModeValidator) {
					t.Errorf("Mode = %q, want validator", got.Mode)
				}
				if got.HaltHeight != 5000 {
					t.Errorf("HaltHeight = %d, want 5000", got.HaltHeight)
				}
				wantForbidden := map[string]bool{testValidatorSecret: false, testValidatorNodeKey: false}
				for _, n := range got.ForbiddenSecretNames {
					wantForbidden[n] = true
				}
				if !wantForbidden[testValidatorSecret] || !wantForbidden[testValidatorNodeKey] {
					t.Errorf("ForbiddenSecretNames = %v, want both signing-key and node-key Secrets", got.ForbiddenSecretNames)
				}
			},
		},
		{
			name: "archive node maps to archive mode and has no forbidden secrets",
			node: &seiv1alpha1.SeiNode{
				ObjectMeta: metav1.ObjectMeta{Name: "archive-0", Namespace: testNamespaceDefault},
				Spec: seiv1alpha1.SeiNodeSpec{
					ChainID: testChainPacific,
					Image:   "sei:archive",
					Archive: &seiv1alpha1.ArchiveSpec{},
				},
			},
			snap: &seiv1alpha1.SnapshotSource{S3: &seiv1alpha1.S3SnapshotSource{TargetHeight: 42}},
			validate: func(t *testing.T, got BootstrapPodInputs) {
				if got.Mode != string(seiconfig.ModeArchive) {
					t.Errorf("Mode = %q, want archive", got.Mode)
				}
				if len(got.ForbiddenSecretNames) != 0 {
					t.Errorf("ForbiddenSecretNames = %v, want empty for non-validator", got.ForbiddenSecretNames)
				}
			},
		},
		{
			name: "Image falls back to Spec.Image when snap.BootstrapImage is empty",
			node: bootstrapTestReplayerNode(),
			snap: &seiv1alpha1.SnapshotSource{S3: &seiv1alpha1.S3SnapshotSource{TargetHeight: 42}},
			validate: func(t *testing.T, got BootstrapPodInputs) {
				if got.Image != testImageLatest {
					t.Errorf("Image = %q, want fallback to Spec.Image", got.Image)
				}
			},
		},
		{
			name: "full node with custom sidecar image and port",
			node: &seiv1alpha1.SeiNode{
				ObjectMeta: metav1.ObjectMeta{Name: "full-0", Namespace: testNamespaceDefault},
				Spec: seiv1alpha1.SeiNodeSpec{
					ChainID: testChainPacific,
					Image:   "sei:full",
					FullNode: &seiv1alpha1.FullNodeSpec{
						Snapshot: &seiv1alpha1.SnapshotSource{S3: &seiv1alpha1.S3SnapshotSource{TargetHeight: 100}},
					},
					Sidecar: &seiv1alpha1.SidecarConfig{Image: testCustomSidecarImage, Port: testCustomSidecarPort},
				},
			},
			snap: &seiv1alpha1.SnapshotSource{S3: &seiv1alpha1.S3SnapshotSource{TargetHeight: 100}},
			validate: func(t *testing.T, got BootstrapPodInputs) {
				if got.SidecarImage != testCustomSidecarImage {
					t.Errorf("SidecarImage = %q, want %q", got.SidecarImage, testCustomSidecarImage)
				}
				if got.SidecarPort != testCustomSidecarPort {
					t.Errorf("SidecarPort = %d, want %d", got.SidecarPort, testCustomSidecarPort)
				}
				if got.Mode != string(seiconfig.ModeFull) {
					t.Errorf("Mode = %q, want full", got.Mode)
				}
			},
		},
		{
			name: "replayer falls through to default mode (full)",
			node: bootstrapTestReplayerNode(),
			snap: bootstrapTestReplayerNode().Spec.SnapshotSource(),
			validate: func(t *testing.T, got BootstrapPodInputs) {
				if got.Mode != string(seiconfig.ModeFull) {
					t.Errorf("Mode = %q, want full (replayer falls through to default)", got.Mode)
				}
			},
		},
		{
			name: "default sidecar image when SeiNode.Sidecar is nil",
			node: &seiv1alpha1.SeiNode{
				ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: testNamespaceDefault},
				Spec: seiv1alpha1.SeiNodeSpec{
					ChainID: testChainPacific,
					Image:   "sei:full",
					FullNode: &seiv1alpha1.FullNodeSpec{
						Snapshot: &seiv1alpha1.SnapshotSource{S3: &seiv1alpha1.S3SnapshotSource{TargetHeight: 1}},
					},
				},
			},
			snap: &seiv1alpha1.SnapshotSource{S3: &seiv1alpha1.S3SnapshotSource{TargetHeight: 1}},
			validate: func(t *testing.T, got BootstrapPodInputs) {
				if got.SidecarImage != bootstrapDefaultSidecarImage {
					t.Errorf("SidecarImage = %q, want platform default %q", got.SidecarImage, bootstrapDefaultSidecarImage)
				}
				if got.SidecarPort != seiconfig.PortSidecar {
					t.Errorf("SidecarPort = %d, want sei-config default %d", got.SidecarPort, seiconfig.PortSidecar)
				}
			},
		},
		{
			name: "nil snapshot leaves HaltHeight zero and Image at Spec.Image",
			node: bootstrapTestReplayerNode(),
			snap: nil,
			validate: func(t *testing.T, got BootstrapPodInputs) {
				if got.HaltHeight != 0 {
					t.Errorf("HaltHeight = %d, want 0", got.HaltHeight)
				}
				if got.Image != testImageLatest {
					t.Errorf("Image = %q, want fallback %q", got.Image, testImageLatest)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nodeToBootstrapInputs(tt.node, tt.snap)
			if got.Name != tt.node.Name || got.Namespace != tt.node.Namespace {
				t.Errorf("Name/Namespace = %q/%q, want %q/%q", got.Namespace, got.Name, tt.node.Namespace, tt.node.Name)
			}
			if got.ChainID != tt.node.Spec.ChainID {
				t.Errorf("ChainID = %q, want %q", got.ChainID, tt.node.Spec.ChainID)
			}
			tt.validate(t, got)
		})
	}
}

// --- GenerateBootstrapJob ---

func TestGenerateBootstrapJob(t *testing.T) {
	node := bootstrapTestReplayerNode()
	node.Spec.Replayer.Snapshot.BootstrapImage = testBootstrapImageV1
	inputs := nodeToBootstrapInputs(node, node.Spec.SnapshotSource())
	job, err := GenerateBootstrapJob(inputs, platformtest.Config())
	if err != nil {
		t.Fatalf("GenerateBootstrapJob error: %v", err)
	}

	if job.Name != "test-replayer-bootstrap" {
		t.Errorf("Job name = %q, want test-replayer-bootstrap", job.Name)
	}
	spec := job.Spec.Template.Spec
	if spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q, want Never", spec.RestartPolicy)
	}
	if spec.Containers[0].Image != testBootstrapImageV1 {
		t.Errorf("main container image = %q, want %q", spec.Containers[0].Image, testBootstrapImageV1)
	}
	if spec.InitContainers[0].Image != testBootstrapImageV1 {
		t.Errorf("seid-init image = %q, want %q (same image as main)", spec.InitContainers[0].Image, testBootstrapImageV1)
	}
}

func TestGenerateBootstrapJob_Validation(t *testing.T) {
	base := func() BootstrapPodInputs {
		return BootstrapPodInputs{
			Name:        "n",
			Namespace:   testNamespaceDefault,
			ChainID:     testChainPacific,
			Image:       testImageLatest,
			SidecarPort: 7777,
			Mode:        string(seiconfig.ModeFull),
			HaltHeight:  100,
		}
	}
	tests := []struct {
		name      string
		mutate    func(*BootstrapPodInputs)
		errSubstr string
	}{
		{"empty Name", func(in *BootstrapPodInputs) { in.Name = "" }, "Name and Namespace"},
		{"empty Namespace", func(in *BootstrapPodInputs) { in.Namespace = "" }, "Name and Namespace"},
		{"empty ChainID", func(in *BootstrapPodInputs) { in.ChainID = "" }, "ChainID"},
		{"empty Image", func(in *BootstrapPodInputs) { in.Image = "" }, "Image"},
		{"zero HaltHeight", func(in *BootstrapPodInputs) { in.HaltHeight = 0 }, "HaltHeight"},
		{"negative HaltHeight", func(in *BootstrapPodInputs) { in.HaltHeight = -1 }, "HaltHeight"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := base()
			tt.mutate(&in)
			_, err := GenerateBootstrapJob(in, platformtest.Config())
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.errSubstr) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.errSubstr)
			}
		})
	}
}

func TestGenerateBootstrapJob_NilSnapshotProducesZeroHaltHeightError(t *testing.T) {
	node := bootstrapTestReplayerNode()
	inputs := nodeToBootstrapInputs(node, nil)
	if inputs.HaltHeight != 0 {
		t.Fatalf("expected HaltHeight=0 from nil snapshot, got %d", inputs.HaltHeight)
	}
	_, err := GenerateBootstrapJob(inputs, platformtest.Config())
	if err == nil || !strings.Contains(err.Error(), "HaltHeight") {
		t.Fatalf("expected HaltHeight error, got %v", err)
	}
}

func TestGenerateBootstrapJob_SidecarResources(t *testing.T) {
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
	inputs := nodeToBootstrapInputs(node, node.Spec.SnapshotSource())
	job, err := GenerateBootstrapJob(inputs, platformtest.Config())
	if err != nil {
		t.Fatalf("GenerateBootstrapJob error: %v", err)
	}

	sidecarContainer := job.Spec.Template.Spec.InitContainers[1]
	cpu := sidecarContainer.Resources.Requests[corev1.ResourceCPU]
	if cpu.String() != "500m" {
		t.Errorf("sidecar CPU request = %q, want 500m", cpu.String())
	}
}

// TestGenerateBootstrapJob_NeverHasIdentityVolumes is the safety regression
// guard for the load-bearing invariant on validator identity material. The
// bootstrap pod must never carry the consensus signing key (slashing risk)
// nor the validator's permanent node key (peer-reputation pollution risk).
func TestGenerateBootstrapJob_NeverHasIdentityVolumes(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testValidatorName, Namespace: testNamespaceDefault},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: testChainAtlantic,
			Image:   "ghcr.io/sei-protocol/seid:latest",
			Validator: &seiv1alpha1.ValidatorSpec{
				Snapshot: &seiv1alpha1.SnapshotSource{
					BootstrapImage: testBootstrapImageV1,
					S3:             &seiv1alpha1.S3SnapshotSource{TargetHeight: 100000000},
				},
				SigningKey: &seiv1alpha1.SigningKeySource{
					Secret: &seiv1alpha1.SecretSigningKeySource{SecretName: testValidatorSecret},
				},
				NodeKey: &seiv1alpha1.NodeKeySource{
					Secret: &seiv1alpha1.SecretNodeKeySource{SecretName: testValidatorNodeKey},
				},
			},
		},
	}
	inputs := nodeToBootstrapInputs(node, node.Spec.SnapshotSource())
	job, err := GenerateBootstrapJob(inputs, platformtest.Config())
	if err != nil {
		t.Fatalf("GenerateBootstrapJob error: %v", err)
	}

	forbiddenVolumeNames := map[string]string{
		"signing-key": "consensus signing material — slashing risk",
		"node-key":    "validator permanent node ID — peer-reputation pollution risk",
	}
	forbiddenSecretNames := map[string]string{
		testValidatorSecret:  "consensus signing Secret",
		testValidatorNodeKey: "validator node-key Secret",
	}
	for _, vol := range job.Spec.Template.Spec.Volumes {
		if reason, forbidden := forbiddenVolumeNames[vol.Name]; forbidden {
			t.Fatalf("bootstrap Job pod-spec must NEVER include volume %q (%s); found %+v", vol.Name, reason, vol)
		}
		if vol.Secret != nil {
			if reason, forbidden := forbiddenSecretNames[vol.Secret.SecretName]; forbidden {
				t.Fatalf("bootstrap Job pod-spec must NEVER mount Secret %q (%s); found volume %q",
					vol.Secret.SecretName, reason, vol.Name)
			}
		}
	}
	for _, container := range job.Spec.Template.Spec.Containers {
		for _, mount := range container.VolumeMounts {
			if reason, forbidden := forbiddenVolumeNames[mount.Name]; forbidden {
				t.Fatalf("bootstrap Job container %q must NEVER mount %q (%s)", container.Name, mount.Name, reason)
			}
		}
	}
}

// --- rejectForbiddenSecretMounts ---

// TestRejectForbiddenSecretMounts is the negative test that proves the
// forbidden-Secret guard fires. The natural-path test above only exercises
// the case where the helper does not produce the volume in the first place;
// this one hand-builds a leaking PodSpec to catch regressions in the guard.
func TestRejectForbiddenSecretMounts(t *testing.T) {
	tests := []struct {
		name      string
		volumes   []corev1.Volume
		forbidden []string
		wantErr   string
	}{
		{
			name:      "empty forbidden list passes",
			volumes:   []corev1.Volume{{Name: testDataVolumeName}},
			forbidden: nil,
			wantErr:   "",
		},
		{
			name: "secret not in list passes",
			volumes: []corev1.Volume{
				{Name: "config", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "ok"}}},
			},
			forbidden: []string{testValidatorSecret},
			wantErr:   "",
		},
		{
			name: "signing-key secret in list fails closed",
			volumes: []corev1.Volume{
				{Name: testDataVolumeName},
				{Name: "signing-key", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: testValidatorSecret}}},
			},
			forbidden: []string{testValidatorSecret, testValidatorNodeKey},
			wantErr:   testValidatorSecret,
		},
		{
			name: "node-key secret in list fails closed",
			volumes: []corev1.Volume{
				{Name: "node-key", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: testValidatorNodeKey}}},
			},
			forbidden: []string{testValidatorSecret, testValidatorNodeKey},
			wantErr:   testValidatorNodeKey,
		},
		{
			name: "empty-string entries in forbidden list are ignored",
			volumes: []corev1.Volume{
				{Name: "config", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: ""}}},
			},
			forbidden: []string{""},
			wantErr:   "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := rejectForbiddenSecretMounts(&corev1.PodSpec{Volumes: tt.volumes}, tt.forbidden)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected nil error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error mentioning %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// --- GenerateBootstrapService ---

func TestGenerateBootstrapService_TolerantOfZeroFields(t *testing.T) {
	svc := GenerateBootstrapService(BootstrapPodInputs{
		Name:        testExporterName,
		Namespace:   "fork",
		SidecarPort: 7777,
	})
	if svc.Spec.ClusterIP != corev1.ClusterIPNone {
		t.Errorf("ClusterIP = %q, want None", svc.Spec.ClusterIP)
	}
	if !svc.Spec.PublishNotReadyAddresses {
		t.Error("PublishNotReadyAddresses = false, want true")
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 7777 {
		t.Errorf("ports = %+v, want single sidecar port 7777", svc.Spec.Ports)
	}
	if svc.Spec.Selector[bootstrapNodeLabel] != testExporterName {
		t.Errorf("selector node label = %q, want %q", svc.Spec.Selector[bootstrapNodeLabel], testExporterName)
	}
}
