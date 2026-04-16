package task

import (
	"context"
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func observeImageNode() *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1", Namespace: "default", UID: "uid-1"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  "atlantic-2",
			Image:    "sei:v2.0.0",
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
		Status: seiv1alpha1.SeiNodeStatus{
			Phase:        seiv1alpha1.PhaseRunning,
			CurrentImage: "sei:v1.0.0",
		},
	}
}

func observeImageCfg(t *testing.T, node *seiv1alpha1.SeiNode, sts *appsv1.StatefulSet) ExecutionConfig {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := seiv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	builder := fake.NewClientBuilder().WithScheme(s)
	if sts != nil {
		builder = builder.WithObjects(sts)
	}
	c := builder.Build()
	return ExecutionConfig{
		KubeClient: c,
		Scheme:     s,
		Resource:   node,
	}
}

func int32Ptr(v int32) *int32 { return &v }

func TestObserveImage_RolloutComplete_SetsCurrentImage(t *testing.T) {
	g := NewWithT(t)
	node := observeImageNode()

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       node.Name,
			Namespace:  node.Namespace,
			Generation: 2,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: int32Ptr(1),
		},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 2,
			UpdatedReplicas:    1,
			Replicas:           1,
		},
	}

	cfg := observeImageCfg(t, node, sts)
	params := ObserveImageParams{NodeName: node.Name, Namespace: node.Namespace}
	raw, _ := json.Marshal(params)

	exec, err := deserializeObserveImage("obs-1", raw, cfg)
	g.Expect(err).NotTo(HaveOccurred())

	// Execute is a no-op.
	g.Expect(exec.Execute(context.Background())).To(Succeed())

	// Status should detect the completed rollout.
	status := exec.Status(context.Background())
	g.Expect(status).To(Equal(ExecutionComplete))

	// The task should stamp currentImage on the node.
	g.Expect(node.Status.CurrentImage).To(Equal("sei:v2.0.0"),
		"currentImage should be updated to spec.image on rollout completion")
}

func TestObserveImage_RollingUpdate_ReturnsRunning(t *testing.T) {
	g := NewWithT(t)
	node := observeImageNode()

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       node.Name,
			Namespace:  node.Namespace,
			Generation: 2,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: int32Ptr(1),
		},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 2,
			UpdatedReplicas:    0, // not yet updated
			Replicas:           1,
		},
	}

	cfg := observeImageCfg(t, node, sts)
	params := ObserveImageParams{NodeName: node.Name, Namespace: node.Namespace}
	raw, _ := json.Marshal(params)

	exec, err := deserializeObserveImage("obs-2", raw, cfg)
	g.Expect(err).NotTo(HaveOccurred())

	status := exec.Status(context.Background())
	g.Expect(status).To(Equal(ExecutionRunning),
		"should return Running when UpdatedReplicas < Replicas")

	// CurrentImage should NOT be updated yet.
	g.Expect(node.Status.CurrentImage).To(Equal("sei:v1.0.0"))
}

func TestObserveImage_StaleGeneration_ReturnsRunning(t *testing.T) {
	g := NewWithT(t)
	node := observeImageNode()

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       node.Name,
			Namespace:  node.Namespace,
			Generation: 3,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: int32Ptr(1),
		},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 2, // stale — controller hasn't observed the latest spec
			UpdatedReplicas:    1,
			Replicas:           1,
		},
	}

	cfg := observeImageCfg(t, node, sts)
	params := ObserveImageParams{NodeName: node.Name, Namespace: node.Namespace}
	raw, _ := json.Marshal(params)

	exec, err := deserializeObserveImage("obs-3", raw, cfg)
	g.Expect(err).NotTo(HaveOccurred())

	status := exec.Status(context.Background())
	g.Expect(status).To(Equal(ExecutionRunning),
		"should return Running when ObservedGeneration < Generation")

	g.Expect(node.Status.CurrentImage).To(Equal("sei:v1.0.0"))
}

func TestObserveImage_StatefulSetNotFound_ReturnsRunning(t *testing.T) {
	g := NewWithT(t)
	node := observeImageNode()

	// No StatefulSet in the fake client.
	cfg := observeImageCfg(t, node, nil)
	params := ObserveImageParams{NodeName: node.Name, Namespace: node.Namespace}
	raw, _ := json.Marshal(params)

	exec, err := deserializeObserveImage("obs-4", raw, cfg)
	g.Expect(err).NotTo(HaveOccurred())

	status := exec.Status(context.Background())
	g.Expect(status).To(Equal(ExecutionRunning),
		"should return Running when StatefulSet does not exist yet")

	g.Expect(node.Status.CurrentImage).To(Equal("sei:v1.0.0"))
}

func TestObserveImage_Idempotent_AlreadyComplete(t *testing.T) {
	g := NewWithT(t)
	node := observeImageNode()

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       node.Name,
			Namespace:  node.Namespace,
			Generation: 2,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: int32Ptr(1),
		},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 2,
			UpdatedReplicas:    1,
			Replicas:           1,
		},
	}

	cfg := observeImageCfg(t, node, sts)
	params := ObserveImageParams{NodeName: node.Name, Namespace: node.Namespace}
	raw, _ := json.Marshal(params)

	exec, err := deserializeObserveImage("obs-5", raw, cfg)
	g.Expect(err).NotTo(HaveOccurred())

	// First call — completes.
	s1 := exec.Status(context.Background())
	g.Expect(s1).To(Equal(ExecutionComplete))

	// Second call — should remain Complete (terminal state cached).
	s2 := exec.Status(context.Background())
	g.Expect(s2).To(Equal(ExecutionComplete))
}

func TestObserveImage_NilReplicas_ReturnsRunning(t *testing.T) {
	g := NewWithT(t)
	node := observeImageNode()

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       node.Name,
			Namespace:  node.Namespace,
			Generation: 2,
		},
		Spec: appsv1.StatefulSetSpec{
			// Replicas is nil (defaults to 1 in k8s, but nil in the struct).
		},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 2,
			UpdatedReplicas:    1,
			Replicas:           1,
		},
	}

	cfg := observeImageCfg(t, node, sts)
	params := ObserveImageParams{NodeName: node.Name, Namespace: node.Namespace}
	raw, _ := json.Marshal(params)

	exec, err := deserializeObserveImage("obs-6", raw, cfg)
	g.Expect(err).NotTo(HaveOccurred())

	// nil Replicas means the check sts.Spec.Replicas == nil returns true,
	// so the task stays Running.
	status := exec.Status(context.Background())
	g.Expect(status).To(Equal(ExecutionRunning),
		"should return Running when Replicas is nil")
}

func TestObserveImage_DeserializeEmptyParams(t *testing.T) {
	g := NewWithT(t)
	node := observeImageNode()
	cfg := observeImageCfg(t, node, nil)

	exec, err := deserializeObserveImage("obs-7", nil, cfg)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(exec).NotTo(BeNil(), "deserialize with nil params should succeed")
}
