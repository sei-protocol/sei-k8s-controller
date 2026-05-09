package task

import (
	"context"
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	stsUID         = types.UID("sts-uid-1")
	testReplaceNs  = "default"
	testReplaceSTS = "node-1"
)

func replacePodNode() *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testReplaceSTS, Namespace: testReplaceNs},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  "test-chain",
			Image:    "test:v1",
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
		Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseRunning},
	}
}

func stsForReplace(currentRev, updateRev string) *appsv1.StatefulSet {
	one := int32(1)
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: testReplaceSTS, Namespace: testReplaceNs, UID: stsUID, Generation: 1,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
				"app":            "seinode",
				"sei.io/seinode": testReplaceSTS,
			}},
		},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 1,
			CurrentRevision:    currentRev,
			UpdateRevision:     updateRev,
		},
	}
}

func podForReplace(revisionHash string, terminating bool) *corev1.Pod {
	controller := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testReplaceSTS + "-0",
			Namespace: testReplaceNs,
			Labels: map[string]string{
				"app":                                 "seinode",
				"sei.io/seinode":                      testReplaceSTS,
				appsv1.ControllerRevisionHashLabelKey: revisionHash,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "StatefulSet",
				Name:       testReplaceSTS,
				UID:        stsUID,
				Controller: &controller,
			}},
		},
	}
	if terminating {
		now := metav1.Now()
		pod.DeletionTimestamp = &now
		pod.Finalizers = []string{"test/keep-around"}
	}
	return pod
}

func replacePodCfg(t *testing.T, node *seiv1alpha1.SeiNode, objs ...client.Object) ExecutionConfig {
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

func newReplacePodExec(t *testing.T, cfg ExecutionConfig) TaskExecution {
	t.Helper()
	raw, _ := json.Marshal(ReplacePodParams{NodeName: testReplaceSTS, Namespace: testReplaceNs})
	exec, err := deserializeReplacePod("rp-test", raw, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return exec
}

// Stale-revision pod present → task deletes it and completes.
func TestReplacePod_StalePod_DeletesAndCompletes(t *testing.T) {
	g := NewWithT(t)
	node := replacePodNode()
	sts := stsForReplace("old-rev", "new-rev")
	stalePod := podForReplace("old-rev", false)

	cfg := replacePodCfg(t, node, sts, stalePod)
	exec := newReplacePodExec(t, cfg)

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionComplete))

	// Stale pod should be gone (or marked for deletion if a finalizer existed,
	// but ours has none, so the fake client deletes it outright).
	got := &corev1.Pod{}
	err := cfg.KubeClient.Get(context.Background(),
		types.NamespacedName{Name: stalePod.Name, Namespace: stalePod.Namespace}, got)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"expected stale pod to be deleted, got err=%v", err)
}

// Already at update revision → task is no-op (pod preserved, status complete).
func TestReplacePod_AlreadyAtUpdateRevision_NoOp(t *testing.T) {
	g := NewWithT(t)
	node := replacePodNode()
	sts := stsForReplace("new-rev", "new-rev")
	currentPod := podForReplace("new-rev", false)

	cfg := replacePodCfg(t, node, sts, currentPod)
	exec := newReplacePodExec(t, cfg)

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionComplete))

	got := &corev1.Pod{}
	g.Expect(cfg.KubeClient.Get(context.Background(),
		types.NamespacedName{Name: currentPod.Name, Namespace: currentPod.Namespace}, got)).To(Succeed())
}

// Pod already terminating (deletionTimestamp present) → task skips it.
// We assert the pod's finalizer wasn't stripped (i.e. task didn't double-delete).
func TestReplacePod_TerminatingPod_Skipped(t *testing.T) {
	g := NewWithT(t)
	node := replacePodNode()
	sts := stsForReplace("old-rev", "new-rev")
	terminatingPod := podForReplace("old-rev", true)

	cfg := replacePodCfg(t, node, sts, terminatingPod)
	exec := newReplacePodExec(t, cfg)

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionComplete))

	got := &corev1.Pod{}
	g.Expect(cfg.KubeClient.Get(context.Background(),
		types.NamespacedName{Name: terminatingPod.Name, Namespace: terminatingPod.Namespace}, got)).To(Succeed())
	g.Expect(got.DeletionTimestamp).NotTo(BeNil(), "terminating pod should still exist (finalizer holds it)")
}

// Empty UpdateRevision (StatefulSet status not yet populated) → task waits.
func TestReplacePod_NoUpdateRevisionYet_TransientWait(t *testing.T) {
	g := NewWithT(t)
	node := replacePodNode()
	sts := stsForReplace("rev-1", "") // UpdateRevision not yet set

	cfg := replacePodCfg(t, node, sts)
	exec := newReplacePodExec(t, cfg)

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
}

// StatefulSet missing entirely → task is a transient wait (apply-statefulset
// preceded us; the controller will retry on the next reconcile).
func TestReplacePod_StatefulSetMissing_TransientWait(t *testing.T) {
	g := NewWithT(t)
	node := replacePodNode()

	cfg := replacePodCfg(t, node) // no STS, no pods
	exec := newReplacePodExec(t, cfg)

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
}

// STS controller hasn't observed the latest spec yet — its reported revisions
// are stale. Task must wait, not delete.
func TestReplacePod_StatefulSetGenerationStale_TransientWait(t *testing.T) {
	g := NewWithT(t)
	node := replacePodNode()
	sts := stsForReplace("old-rev", "new-rev")
	sts.Generation = 2
	sts.Status.ObservedGeneration = 1 // stale
	stalePod := podForReplace("old-rev", false)

	cfg := replacePodCfg(t, node, sts, stalePod)
	exec := newReplacePodExec(t, cfg)

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))

	// Pod must NOT have been deleted while STS revisions are stale.
	got := &corev1.Pod{}
	g.Expect(cfg.KubeClient.Get(context.Background(),
		types.NamespacedName{Name: stalePod.Name, Namespace: stalePod.Namespace}, got)).To(Succeed())
}

// Pod has no controller-revision-hash label — task skips it (don't delete
// pods we don't recognize).
func TestReplacePod_PodMissingRevisionHashLabel_Skipped(t *testing.T) {
	g := NewWithT(t)
	node := replacePodNode()
	sts := stsForReplace("old-rev", "new-rev")
	pod := podForReplace("old-rev", false)
	delete(pod.Labels, appsv1.ControllerRevisionHashLabelKey)

	cfg := replacePodCfg(t, node, sts, pod)
	exec := newReplacePodExec(t, cfg)

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionComplete))

	got := &corev1.Pod{}
	g.Expect(cfg.KubeClient.Get(context.Background(),
		types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, got)).To(Succeed())
}

// Pod matches the STS selector by labels but isn't actually owned by the STS
// (e.g. a manually-applied pod with the same labels). Task skips it.
func TestReplacePod_PodNotOwnedByStatefulSet_Skipped(t *testing.T) {
	g := NewWithT(t)
	node := replacePodNode()
	sts := stsForReplace("old-rev", "new-rev")
	pod := podForReplace("old-rev", false)
	pod.OwnerReferences = nil // not owned by STS

	cfg := replacePodCfg(t, node, sts, pod)
	exec := newReplacePodExec(t, cfg)

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionComplete))

	got := &corev1.Pod{}
	g.Expect(cfg.KubeClient.Get(context.Background(),
		types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, got)).To(Succeed())
}

// Multi-replica StatefulSets need ordinal-aware deletion; the task fails
// loud rather than silently violating reverse-ordinal rolling-update semantics.
func TestReplacePod_MultiReplica_TerminalError(t *testing.T) {
	g := NewWithT(t)
	node := replacePodNode()
	sts := stsForReplace("old-rev", "new-rev")
	three := int32(3)
	sts.Spec.Replicas = &three

	cfg := replacePodCfg(t, node, sts)
	exec := newReplacePodExec(t, cfg)

	err := exec.Execute(context.Background())
	g.Expect(err).To(HaveOccurred())
	var termErr *TerminalError
	g.Expect(err).To(BeAssignableToTypeOf(termErr))
	g.Expect(err.Error()).To(ContainSubstring("multi-replica"))
}
