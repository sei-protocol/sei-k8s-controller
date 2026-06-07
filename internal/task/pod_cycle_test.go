package task

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	restartNS  = "default"
	restartSTS = "node-1"
	restartRev = "rev-1"
	kindSTS    = "StatefulSet"

	restartedUID   = types.UID("pod-uid-original")
	replacementUID = types.UID("pod-uid-replacement")
)

var someTime = metav1.NewTime(time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC))

func restartPodNode() *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: restartSTS, Namespace: restartNS},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  "test-chain",
			Image:    "test:v1",
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
		Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseRunning},
	}
}

func stsForRestart() *appsv1.StatefulSet {
	one := int32(1)
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: restartSTS, Namespace: restartNS, UID: stsUID, Generation: 1,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
				"sei.io/node": restartSTS,
			}},
		},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 1,
			CurrentRevision:    restartRev,
			UpdateRevision:     restartRev,
		},
	}
}

// podForRestart builds a pod owned by the restart STS. uid identifies the pod;
// created sets the creationTimestamp; ready/terminating set the corresponding
// pod state.
func podForRestart(uid types.UID, created metav1.Time, ready, terminating bool) *corev1.Pod {
	controller := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              restartSTS + "-0",
			Namespace:         restartNS,
			UID:               uid,
			CreationTimestamp: created,
			Labels: map[string]string{
				"sei.io/node":                         restartSTS,
				appsv1.ControllerRevisionHashLabelKey: restartRev,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       kindSTS,
				Name:       restartSTS,
				UID:        stsUID,
				Controller: &controller,
			}},
		},
	}
	if ready {
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	if terminating {
		now := metav1.Now()
		pod.DeletionTimestamp = &now
		pod.Finalizers = []string{"test/keep-around"}
	}
	return pod
}

func restartPodCfg(t *testing.T, node *seiv1alpha1.SeiNode, objs ...client.Object) ExecutionConfig {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := seiv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return ExecutionConfig{KubeClient: c, APIReader: c, Scheme: s, Resource: node}
}

// fetchStatefulSet signals a missing StatefulSet as (node, nil, nil) so callers
// treat it as a transient wait rather than an error.
func TestPodCycle_FetchStatefulSet_MissingIsTransient(t *testing.T) {
	g := NewWithT(t)
	node := restartPodNode()
	cfg := restartPodCfg(t, node)

	pc := podCycle{cfg: cfg}
	gotNode, sts, err := pc.fetchStatefulSet(context.Background())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(sts).To(BeNil())
	g.Expect(gotNode.Name).To(Equal(node.Name))
}

func TestPodCycle_GuardSelectorAndReplicas(t *testing.T) {
	node := restartPodNode()

	t.Run("nil selector is terminal", func(t *testing.T) {
		g := NewWithT(t)
		sts := stsForRestart()
		sts.Spec.Selector = nil
		err := guardSelectorAndReplicas(node, sts, TaskTypeReplacePod)
		var termErr *TerminalError
		g.Expect(err).To(BeAssignableToTypeOf(termErr))
		g.Expect(err.Error()).To(ContainSubstring("no selector"))
	})

	t.Run("empty selector is terminal", func(t *testing.T) {
		g := NewWithT(t)
		sts := stsForRestart()
		sts.Spec.Selector = &metav1.LabelSelector{}
		err := guardSelectorAndReplicas(node, sts, TaskTypeReplacePod)
		var termErr *TerminalError
		g.Expect(err).To(BeAssignableToTypeOf(termErr))
	})

	t.Run("multi-replica is terminal and names the task", func(t *testing.T) {
		g := NewWithT(t)
		sts := stsForRestart()
		three := int32(3)
		sts.Spec.Replicas = &three
		err := guardSelectorAndReplicas(node, sts, TaskTypeReplacePod)
		var termErr *TerminalError
		g.Expect(err).To(BeAssignableToTypeOf(termErr))
		g.Expect(err.Error()).To(ContainSubstring("multi-replica"))
		g.Expect(err.Error()).To(ContainSubstring(TaskTypeReplacePod))
	})

	t.Run("single-replica with selector passes", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(guardSelectorAndReplicas(node, stsForRestart(), TaskTypeReplacePod)).To(Succeed())
	})
}

// ownedPods returns only pods that carry an ownerReference to the StatefulSet,
// even when a label-matching but unowned pod shares the selector.
func TestPodCycle_OwnedPods_FiltersByOwnership(t *testing.T) {
	g := NewWithT(t)
	node := restartPodNode()
	sts := stsForRestart()

	owned := podForRestart(restartedUID, someTime, false, false)
	unowned := podForRestart(replacementUID, someTime, false, false)
	unowned.Name = restartSTS + "-imposter"
	unowned.OwnerReferences = nil // matches selector labels but not owned

	cfg := restartPodCfg(t, node, sts, owned, unowned)
	pc := podCycle{cfg: cfg}

	pods, err := pc.ownedPods(context.Background(), node, sts)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pods).To(HaveLen(1))
	g.Expect(pods[0].UID).To(Equal(restartedUID))
}

// deletePod tolerates an already-absent pod (NotFound) so the task is
// idempotent across reconciles.
func TestPodCycle_DeletePod_NotFoundTolerant(t *testing.T) {
	g := NewWithT(t)
	node := restartPodNode()
	cfg := restartPodCfg(t, node)
	pc := podCycle{cfg: cfg}

	ghost := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "gone", Namespace: restartNS}}
	g.Expect(pc.deletePod(context.Background(), ghost)).To(Succeed())
}
