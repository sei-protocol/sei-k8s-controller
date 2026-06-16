package task

import (
	"context"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest"
)

const (
	duTestImage        = "sei:v2.0.0"
	duTestChainID      = "pacific-1"
	duTestSidecarImage = "seictl:v2"
)

func testDeploymentGroup() *seiv1alpha1.SeiNetwork {
	return &seiv1alpha1.SeiNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "wave", Namespace: "sei", UID: "uid-wave"},
		Spec: seiv1alpha1.SeiNetworkSpec{
			Image:    duTestImage,
			Replicas: 2,
			Genesis:  seiv1alpha1.GenesisCeremonyConfig{ChainID: duTestChainID},
			Sidecar: &seiv1alpha1.SidecarConfig{
				Image: duTestSidecarImage,
				Port:  7777,
			},
		},
	}
}

func testDeploymentCfg(t *testing.T, group *seiv1alpha1.SeiNetwork, nodes ...*seiv1alpha1.SeiNode) ExecutionConfig {
	t.Helper()
	s := testScheme(t)
	builder := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(group).
		WithStatusSubresource(&seiv1alpha1.SeiNode{})
	for _, n := range nodes {
		builder = builder.WithObjects(n)
	}
	c := builder.Build()
	return ExecutionConfig{
		KubeClient: c,
		APIReader:  c,
		Scheme:     s,
		Resource:   group,
		Platform:   platformtest.Config(),
	}
}

// --- UpdateNodeSpecs ---

func TestUpdateNodeSpecs_PatchesImage(t *testing.T) {
	group := testDeploymentGroup()
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "wave-0", Namespace: "sei"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: duTestChainID,
			Image:   "sei:v1.0.0",
			FullNode: &seiv1alpha1.FullNodeSpec{
				Snapshot: &seiv1alpha1.SnapshotSource{
					S3: &seiv1alpha1.S3SnapshotSource{TargetHeight: 100},
				},
			},
			Sidecar: &seiv1alpha1.SidecarConfig{
				Image: "seictl:v1",
				Port:  7777,
			},
		},
	}
	cfg := testDeploymentCfg(t, group, node)

	params := UpdateNodeSpecsParams{
		GroupName: "wave",
		Namespace: "sei",
		NodeNames: []string{"wave-0"},
	}
	raw, _ := json.Marshal(params)
	exec, err := deserializeUpdateNodeSpecs("id-1", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	ctx := context.Background()
	if err := exec.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if exec.Status(ctx) != ExecutionComplete {
		t.Fatalf("expected Complete, got %s", exec.Status(ctx))
	}

	fetched := &seiv1alpha1.SeiNode{}
	if err := cfg.KubeClient.Get(ctx, types.NamespacedName{Name: "wave-0", Namespace: "sei"}, fetched); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if fetched.Spec.Image != duTestImage {
		t.Errorf("image = %q, want %q", fetched.Spec.Image, duTestImage)
	}
	if fetched.Spec.Sidecar.Image != duTestSidecarImage {
		t.Errorf("sidecar image = %q, want %q", fetched.Spec.Sidecar.Image, duTestSidecarImage)
	}
}

func TestUpdateNodeSpecs_SkipsCurrentImage(t *testing.T) {
	group := testDeploymentGroup()
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "wave-0", Namespace: "sei"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: duTestChainID,
			Image:   duTestImage,
			FullNode: &seiv1alpha1.FullNodeSpec{
				Snapshot: &seiv1alpha1.SnapshotSource{
					S3: &seiv1alpha1.S3SnapshotSource{TargetHeight: 100},
				},
			},
			Sidecar: &seiv1alpha1.SidecarConfig{
				Image: duTestSidecarImage,
				Port:  7777,
			},
		},
	}
	cfg := testDeploymentCfg(t, group, node)

	params := UpdateNodeSpecsParams{
		GroupName: "wave",
		Namespace: "sei",
		NodeNames: []string{"wave-0"},
	}
	raw, _ := json.Marshal(params)
	exec, err := deserializeUpdateNodeSpecs("id-1", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	ctx := context.Background()
	if err := exec.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if exec.Status(ctx) != ExecutionComplete {
		t.Fatalf("expected Complete, got %s", exec.Status(ctx))
	}

	fetched := &seiv1alpha1.SeiNode{}
	if err := cfg.KubeClient.Get(ctx, types.NamespacedName{Name: "wave-0", Namespace: "sei"}, fetched); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if fetched.Spec.Image != duTestImage {
		t.Errorf("image should remain %q, got %q", duTestImage, fetched.Spec.Image)
	}
}

// --- AwaitSpecUpdate ---

func TestAwaitSpecUpdate_CompletesWhenConverged(t *testing.T) {
	group := testDeploymentGroup()
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "wave-0", Namespace: "sei"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  duTestChainID,
			Image:    duTestImage,
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
		Status: seiv1alpha1.SeiNodeStatus{
			CurrentImage: duTestImage,
		},
	}
	cfg := testDeploymentCfg(t, group, node)

	params := AwaitSpecUpdateParams{
		Namespace: "sei",
		NodeNames: []string{"wave-0"},
	}
	raw, _ := json.Marshal(params)
	exec, err := deserializeAwaitSpecUpdate("id-2", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	if exec.Status(context.Background()) != ExecutionComplete {
		t.Fatal("expected Complete when currentImage == spec.image")
	}
}

func TestAwaitSpecUpdate_RunningWhenNotConverged(t *testing.T) {
	group := testDeploymentGroup()
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "wave-0", Namespace: "sei"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  duTestChainID,
			Image:    duTestImage,
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
		Status: seiv1alpha1.SeiNodeStatus{
			CurrentImage: "sei:v1.0.0",
		},
	}
	cfg := testDeploymentCfg(t, group, node)

	params := AwaitSpecUpdateParams{
		Namespace: "sei",
		NodeNames: []string{"wave-0"},
	}
	raw, _ := json.Marshal(params)
	exec, err := deserializeAwaitSpecUpdate("id-2", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	if exec.Status(context.Background()) != ExecutionRunning {
		t.Fatalf("expected Running when not converged, got %s", exec.Status(context.Background()))
	}
}

func TestAwaitSpecUpdate_RunningWhenNodeNotFound(t *testing.T) {
	group := testDeploymentGroup()
	cfg := testDeploymentCfg(t, group)

	params := AwaitSpecUpdateParams{
		Namespace: "sei",
		NodeNames: []string{"wave-nonexistent"},
	}
	raw, _ := json.Marshal(params)
	exec, err := deserializeAwaitSpecUpdate("id-2", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	if exec.Status(context.Background()) != ExecutionRunning {
		t.Fatalf("expected Running for missing node, got %s", exec.Status(context.Background()))
	}
}
