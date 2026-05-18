package planner

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const proxyTestValidator = "validator-0"

func TestValidatorPlanner_BuildPlan_ApplyRBACProxyConfigBeforeStatefulSet(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: proxyTestValidator, Namespace: sourceChainID},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:   sourceChainID,
			Image:     "seid:v1.0.0",
			Validator: &seiv1alpha1.ValidatorSpec{},
		},
	}
	plan, err := (&validatorPlanner{}).BuildPlan(node)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	assertRBACProxyConfigBeforeStatefulSet(t, plan)
}

func TestBuildGenesisPlan_ApplyRBACProxyConfigBeforeStatefulSet(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: proxyTestValidator, Namespace: sourceChainID},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:   sourceChainID,
			Image:     "seid:v1.0.0",
			Validator: &seiv1alpha1.ValidatorSpec{GenesisCeremony: &seiv1alpha1.GenesisCeremonyNodeConfig{}},
		},
	}
	plan, err := buildGenesisPlan(node)
	if err != nil {
		t.Fatalf("buildGenesisPlan: %v", err)
	}
	assertRBACProxyConfigBeforeStatefulSet(t, plan)
}

func assertRBACProxyConfigBeforeStatefulSet(t *testing.T, plan *seiv1alpha1.TaskPlan) {
	t.Helper()
	cfgIdx := indexOfTaskType(plan, task.TaskTypeApplyRBACProxyConfig)
	stsIdx := indexOfTaskType(plan, task.TaskTypeApplyStatefulSet)
	if cfgIdx < 0 {
		t.Fatalf("plan must contain %s; got %v", task.TaskTypeApplyRBACProxyConfig, taskTypes(plan))
	}
	if cfgIdx >= stsIdx {
		t.Fatalf("expected rbac-proxy-config(%d) < apply-statefulset(%d); got %v", cfgIdx, stsIdx, taskTypes(plan))
	}
}
