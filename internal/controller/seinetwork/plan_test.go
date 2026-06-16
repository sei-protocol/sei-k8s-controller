package seinetwork

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func TestCompletePlan_ClearsRolloutInProgress(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork(testNetworkName, testGroupNS)
	network.Generation = 3
	network.Status.Rollout = &seiv1alpha1.RolloutStatus{
		TargetHash: "newhash1234",
		StartedAt:  metav1.Now(),
	}
	network.Status.Plan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanComplete}
	setPlanInProgress(network, "Deployment", "deploying")
	setCondition(network, seiv1alpha1.ConditionRolloutInProgress, metav1.ConditionTrue,
		"TemplateChanged", "hash changed")

	childNode := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testNode0,
			Namespace: testGroupNS,
			Labels:    map[string]string{groupLabel: testNetworkName},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: testAPIVersion,
				Kind:       testKind,
				Name:       testNetworkName,
				UID:        network.UID,
				Controller: new(true),
			}},
		},
		Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseRunning},
	}

	r := newPlanTestReconciler(t, network, childNode)

	r.completePlan(ctx, network)

	g.Expect(network.Status.Rollout).To(BeNil())
	g.Expect(network.Status.Plan).To(BeNil())

	rolloutCond := apimeta.FindStatusCondition(network.Status.Conditions, seiv1alpha1.ConditionRolloutInProgress)
	g.Expect(rolloutCond).NotTo(BeNil())
	g.Expect(rolloutCond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(rolloutCond.Reason).To(Equal("RolloutComplete"))

	planCond := apimeta.FindStatusCondition(network.Status.Conditions, seiv1alpha1.ConditionPlanInProgress)
	g.Expect(planCond).NotTo(BeNil())
	g.Expect(planCond.Status).To(Equal(metav1.ConditionFalse))
}

// A non-deployment plan completing latches GenesisCeremonyComplete=True —
// every SeiNetwork's first plan is the ceremony.
func TestCompletePlan_GenesisCeremony_LatchesComplete(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork(testNetworkName, testGroupNS)
	network.Status.Plan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanComplete}
	setPlanInProgress(network, "Genesis", "assembling")

	r := newPlanTestReconciler(t, network)
	r.completePlan(ctx, network)

	cond := apimeta.FindStatusCondition(network.Status.Conditions, seiv1alpha1.ConditionGenesisCeremonyComplete)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("Complete"))
	g.Expect(network.Status.Plan).To(BeNil())
}

func TestFailPlan_ClearsRolloutInProgress(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork(testNetworkName, testGroupNS)
	network.Generation = 3
	network.Status.Rollout = &seiv1alpha1.RolloutStatus{
		TargetHash: "newhash1234",
		StartedAt:  metav1.Now(),
	}
	network.Status.Plan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanFailed}
	setPlanInProgress(network, "Deployment", "deploying")
	setCondition(network, seiv1alpha1.ConditionRolloutInProgress, metav1.ConditionTrue,
		"TemplateChanged", "hash changed")

	ownerRef := metav1.OwnerReference{
		APIVersion: testAPIVersion,
		Kind:       testKind,
		Name:       testNetworkName,
		UID:        network.UID,
		Controller: new(true),
	}
	childRunning := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNode0, Namespace: testGroupNS,
			Labels:          map[string]string{groupLabel: testNetworkName},
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseRunning},
	}
	childFailed := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "genesis-net-1", Namespace: testGroupNS,
			Labels:          map[string]string{groupLabel: testNetworkName},
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseFailed},
	}

	r := newPlanTestReconciler(t, network, childRunning, childFailed)

	r.failPlan(ctx, network)

	g.Expect(network.Status.Rollout).To(BeNil())
	g.Expect(network.Status.Plan).To(BeNil())
	g.Expect(network.Status.Phase).To(Equal(seiv1alpha1.GroupPhaseDegraded))

	rolloutCond := apimeta.FindStatusCondition(network.Status.Conditions, seiv1alpha1.ConditionRolloutInProgress)
	g.Expect(rolloutCond).NotTo(BeNil())
	g.Expect(rolloutCond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(rolloutCond.Reason).To(Equal("RolloutFailed"))

	planCond := apimeta.FindStatusCondition(network.Status.Conditions, seiv1alpha1.ConditionPlanInProgress)
	g.Expect(planCond).NotTo(BeNil())
	g.Expect(planCond.Status).To(Equal(metav1.ConditionFalse))
}
