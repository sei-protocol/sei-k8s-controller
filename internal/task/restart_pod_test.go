package task

import (
	"context"
	"encoding/json"
	"testing"
	"time"

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
	restartNS  = "default"
	restartSTS = "node-1"
	restartRev = "rev-1"
	kindSTS    = "StatefulSet"

	// restartedUID is the UID of the pod the restart targets (captured at
	// synthesis). The replacement pod gets replacementUID.
	restartedUID   = types.UID("pod-uid-original")
	replacementUID = types.UID("pod-uid-replacement")
)

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

// podForRestart builds a pod owned by the restart STS. uid identifies the pod
// (the task keys on UID, not creationTimestamp); created sets the
// creationTimestamp; ready/terminating set the corresponding pod state.
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

// newRestartPodExec builds an execution that targets restartedUID (the pod
// captured at synthesis).
func newRestartPodExec(t *testing.T, cfg ExecutionConfig) TaskExecution {
	t.Helper()
	return newRestartPodExecWithUID(t, cfg, restartedUID)
}

func newRestartPodExecWithUID(t *testing.T, cfg ExecutionConfig, uid types.UID) TaskExecution {
	t.Helper()
	raw, _ := json.Marshal(RestartPodParams{NodeName: restartSTS, Namespace: restartNS, RestartedPodUID: uid})
	exec, err := deserializeRestartPod("rp-test", raw, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return exec
}

var (
	someTime  = metav1.NewTime(time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC))
	laterTime = metav1.NewTime(someTime.Add(time.Minute))
)

// Happy path: the captured pod (restartedUID) is deleted; the task completes
// once the replacement (different UID) is Ready.
func TestRestartPod_DeletesTargetAndCompletesWhenReplacementReady(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	node := restartPodNode()
	sts := stsForRestart()
	original := podForRestart(restartedUID, someTime, true, false)

	cfg := restartPodCfg(t, node, sts, original)
	exec := newRestartPodExec(t, cfg)

	g.Expect(exec.Execute(ctx)).To(Succeed())
	got := &corev1.Pod{}
	err := cfg.KubeClient.Get(ctx, types.NamespacedName{Name: original.Name, Namespace: restartNS}, got)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "expected target pod deleted, got err=%v", err)

	g.Expect(exec.Status(ctx)).To(Equal(ExecutionRunning))

	// Replacement (different UID) appears but not Ready → still running.
	fresh := podForRestart(replacementUID, laterTime, false, false)
	g.Expect(cfg.KubeClient.Create(ctx, fresh)).To(Succeed())
	g.Expect(exec.Status(ctx)).To(Equal(ExecutionRunning))

	// Becomes Ready → complete.
	fresh.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	g.Expect(cfg.KubeClient.Status().Update(ctx, fresh)).To(Succeed())
	g.Expect(exec.Status(ctx)).To(Equal(ExecutionComplete))
}

// Same-second race guard: the OnDelete replacement's creationTimestamp truncates
// to the EXACT instant the original was created. A clock-based epoch would
// re-delete it (created <= epoch) and loop forever; the UID-keyed task must NOT
// delete it and MUST complete because its UID differs.
func TestRestartPod_ReplacementSameSecond_NotReDeletedAndCompletes(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	node := restartPodNode()
	sts := stsForRestart()
	// Replacement created at the identical timestamp the original had, but a
	// fresh UID — exactly the case the OnDelete recreation produces within 1s.
	replacement := podForRestart(replacementUID, someTime, true, false)

	cfg := restartPodCfg(t, node, sts, replacement)
	exec := newRestartPodExec(t, cfg)

	g.Expect(exec.Execute(ctx)).To(Succeed())
	got := &corev1.Pod{}
	g.Expect(cfg.KubeClient.Get(ctx, types.NamespacedName{Name: replacement.Name, Namespace: restartNS}, got)).To(Succeed())
	g.Expect(got.DeletionTimestamp).To(BeNil(), "same-second replacement (different UID) must not be re-deleted")
	g.Expect(exec.Status(ctx)).To(Equal(ExecutionComplete))
}

// Stateless across reconciles: a replacement pod (different UID) must NOT be
// deleted by a freshly reconstructed execution. Execute is a no-op and a Ready
// replacement completes immediately.
func TestRestartPod_Replacement_NotReDeleted(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	node := restartPodNode()
	sts := stsForRestart()
	fresh := podForRestart(replacementUID, laterTime, true, false)

	cfg := restartPodCfg(t, node, sts, fresh)
	exec := newRestartPodExec(t, cfg)

	g.Expect(exec.Execute(ctx)).To(Succeed())
	got := &corev1.Pod{}
	g.Expect(cfg.KubeClient.Get(ctx, types.NamespacedName{Name: fresh.Name, Namespace: restartNS}, got)).To(Succeed())
	g.Expect(got.DeletionTimestamp).To(BeNil(), "replacement pod must not be re-deleted")
	g.Expect(exec.Status(ctx)).To(Equal(ExecutionComplete))
}

// No pod existed at synthesis (empty RestartedPodUID, e.g. STS still
// scheduling): Execute is a no-op and never deletes; the task completes on the
// first owned Ready pod.
func TestRestartPod_EmptyUID_NoDeleteWaitsForReady(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	node := restartPodNode()
	sts := stsForRestart()
	// A pod already exists; with empty captured UID the task must not delete it.
	existing := podForRestart(restartedUID, someTime, false, false)

	cfg := restartPodCfg(t, node, sts, existing)
	exec := newRestartPodExecWithUID(t, cfg, "")

	g.Expect(exec.Execute(ctx)).To(Succeed())
	got := &corev1.Pod{}
	g.Expect(cfg.KubeClient.Get(ctx, types.NamespacedName{Name: existing.Name, Namespace: restartNS}, got)).To(Succeed())
	g.Expect(got.DeletionTimestamp).To(BeNil(), "empty UID must not delete any pod")
	g.Expect(exec.Status(ctx)).To(Equal(ExecutionRunning))

	existing.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	g.Expect(cfg.KubeClient.Status().Update(ctx, existing)).To(Succeed())
	g.Expect(exec.Status(ctx)).To(Equal(ExecutionComplete))
}

// No pod exists at Execute time: Execute is a no-op; the task waits for a Ready
// replacement.
func TestRestartPod_NoPodYet_WaitsForReady(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	node := restartPodNode()
	sts := stsForRestart()

	cfg := restartPodCfg(t, node, sts)
	exec := newRestartPodExec(t, cfg)

	g.Expect(exec.Execute(ctx)).To(Succeed())
	g.Expect(exec.Status(ctx)).To(Equal(ExecutionRunning))

	pod := podForRestart(replacementUID, laterTime, true, false)
	g.Expect(cfg.KubeClient.Create(ctx, pod)).To(Succeed())
	g.Expect(exec.Status(ctx)).To(Equal(ExecutionComplete))
}

// StatefulSet missing entirely → Execute is a transient no-op, Status waits.
func TestRestartPod_StatefulSetMissing_TransientWait(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	node := restartPodNode()

	cfg := restartPodCfg(t, node)
	exec := newRestartPodExec(t, cfg)

	g.Expect(exec.Execute(ctx)).To(Succeed())
	g.Expect(exec.Status(ctx)).To(Equal(ExecutionRunning))
}

// Multi-replica StatefulSets are rejected: restart-pod (like replace-pod) only
// supports single-replica nodes.
func TestRestartPod_MultiReplica_TerminalError(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	node := restartPodNode()
	sts := stsForRestart()
	three := int32(3)
	sts.Spec.Replicas = &three

	cfg := restartPodCfg(t, node, sts)
	exec := newRestartPodExec(t, cfg)

	err := exec.Execute(ctx)
	g.Expect(err).To(HaveOccurred())
	var termErr *TerminalError
	g.Expect(err).To(BeAssignableToTypeOf(termErr))
	g.Expect(err.Error()).To(ContainSubstring("multi-replica"))
}

// The captured pod already terminating is not re-deleted, and the task waits
// for the replacement.
func TestRestartPod_TargetTerminating_Skipped(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	node := restartPodNode()
	sts := stsForRestart()
	termPod := podForRestart(restartedUID, someTime, true, true)

	cfg := restartPodCfg(t, node, sts, termPod)
	exec := newRestartPodExec(t, cfg)

	g.Expect(exec.Execute(ctx)).To(Succeed())
	g.Expect(exec.Status(ctx)).To(Equal(ExecutionRunning))

	got := &corev1.Pod{}
	g.Expect(cfg.KubeClient.Get(ctx, types.NamespacedName{Name: termPod.Name, Namespace: restartNS}, got)).To(Succeed())
	g.Expect(got.DeletionTimestamp).NotTo(BeNil(), "terminating pod should still exist (finalizer holds it)")
}
