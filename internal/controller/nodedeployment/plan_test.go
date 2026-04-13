package nodedeployment

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func newPlanTestScheme(t *testing.T) *k8sruntime.Scheme {
	t.Helper()
	s := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := seiv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newPlanTestReconciler(t *testing.T, objs ...client.Object) (*SeiNodeDeploymentReconciler, client.Client) {
	t.Helper()
	s := newPlanTestScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&seiv1alpha1.SeiNodeDeployment{}).
		Build()
	r := &SeiNodeDeploymentReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(100),
	}
	return r, c
}

func TestCompletePlan_ClearsRolloutInProgress(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	group := newTestGroup("archive-rpc", "sei")
	group.Generation = 3
	group.Status.Rollout = &seiv1alpha1.RolloutStatus{
		Strategy:   seiv1alpha1.UpdateStrategyInPlace,
		TargetHash: "newhash1234",
		StartedAt:  metav1.Now(),
	}
	group.Status.Plan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanComplete}
	setPlanInProgress(group, "Deployment", "deploying")
	setCondition(group, seiv1alpha1.ConditionRolloutInProgress, metav1.ConditionTrue,
		"TemplateChanged", "hash changed")

	childNode := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "archive-rpc-0",
			Namespace: "sei",
			Labels:    map[string]string{groupLabel: "archive-rpc"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "sei.io/v1alpha1",
				Kind:       "SeiNodeDeployment",
				Name:       "archive-rpc",
				UID:        group.UID,
				Controller: boolPtr(true),
			}},
		},
		Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseRunning},
	}

	r, c := newPlanTestReconciler(t, group, childNode)

	_, err := r.completePlan(ctx, group)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := &seiv1alpha1.SeiNodeDeployment{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(group), fetched)).To(Succeed())

	g.Expect(fetched.Status.Rollout).To(BeNil())
	g.Expect(fetched.Status.Plan).To(BeNil())

	rolloutCond := apimeta.FindStatusCondition(fetched.Status.Conditions, seiv1alpha1.ConditionRolloutInProgress)
	g.Expect(rolloutCond).NotTo(BeNil())
	g.Expect(rolloutCond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(rolloutCond.Reason).To(Equal("RolloutComplete"))

	planCond := apimeta.FindStatusCondition(fetched.Status.Conditions, seiv1alpha1.ConditionPlanInProgress)
	g.Expect(planCond).NotTo(BeNil())
	g.Expect(planCond.Status).To(Equal(metav1.ConditionFalse))
}

func TestFailPlan_ClearsRolloutInProgress(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	group := newTestGroup("archive-rpc", "sei")
	group.Generation = 3
	group.Status.Rollout = &seiv1alpha1.RolloutStatus{
		Strategy:   seiv1alpha1.UpdateStrategyInPlace,
		TargetHash: "newhash1234",
		StartedAt:  metav1.Now(),
	}
	group.Status.Plan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanFailed}
	setPlanInProgress(group, "Deployment", "deploying")
	setCondition(group, seiv1alpha1.ConditionRolloutInProgress, metav1.ConditionTrue,
		"TemplateChanged", "hash changed")

	ownerRef := metav1.OwnerReference{
		APIVersion: "sei.io/v1alpha1",
		Kind:       "SeiNodeDeployment",
		Name:       "archive-rpc",
		UID:        group.UID,
		Controller: boolPtr(true),
	}
	childRunning := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "archive-rpc-0", Namespace: "sei",
			Labels:          map[string]string{groupLabel: "archive-rpc"},
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseRunning},
	}
	childFailed := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "archive-rpc-1", Namespace: "sei",
			Labels:          map[string]string{groupLabel: "archive-rpc"},
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseFailed},
	}
	childFailed2 := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "archive-rpc-2", Namespace: "sei",
			Labels:          map[string]string{groupLabel: "archive-rpc"},
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseFailed},
	}

	r, c := newPlanTestReconciler(t, group, childRunning, childFailed, childFailed2)

	_, err := r.failPlan(ctx, group)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := &seiv1alpha1.SeiNodeDeployment{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(group), fetched)).To(Succeed())

	g.Expect(fetched.Status.Rollout).To(BeNil())
	g.Expect(fetched.Status.Plan).To(BeNil())
	g.Expect(fetched.Status.Phase).To(Equal(seiv1alpha1.GroupPhaseDegraded))

	rolloutCond := apimeta.FindStatusCondition(fetched.Status.Conditions, seiv1alpha1.ConditionRolloutInProgress)
	g.Expect(rolloutCond).NotTo(BeNil())
	g.Expect(rolloutCond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(rolloutCond.Reason).To(Equal("RolloutFailed"))

	planCond := apimeta.FindStatusCondition(fetched.Status.Conditions, seiv1alpha1.ConditionPlanInProgress)
	g.Expect(planCond).NotTo(BeNil())
	g.Expect(planCond.Status).To(Equal(metav1.ConditionFalse))
}

func boolPtr(b bool) *bool { return &b }
