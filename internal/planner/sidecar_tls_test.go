package planner

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const (
	testValidatorName = "validator-0"
	testSeidImage     = "seid:v6.4.1"
)

func TestValidatorPlanner_BuildPlan_NoSidecarTLSTasksByDefault(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testValidatorName, Namespace: sourceChainID},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:   sourceChainID,
			Image:     testSeidImage,
			Validator: &seiv1alpha1.ValidatorSpec{},
		},
	}
	plan, err := (&validatorPlanner{}).BuildPlan(node)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if idx := indexOfTaskType(plan, task.TaskTypeApplySidecarCert); idx >= 0 {
		t.Fatalf("plan must not contain %s without spec.sidecar.tls; got %v",
			task.TaskTypeApplySidecarCert, taskTypes(plan))
	}
	if idx := indexOfTaskType(plan, task.TaskTypeApplyRBACProxyConfig); idx >= 0 {
		t.Fatalf("plan must not contain %s without spec.sidecar.tls; got %v",
			task.TaskTypeApplyRBACProxyConfig, taskTypes(plan))
	}
}

func tlsSpec() *seiv1alpha1.SidecarTLSSpec {
	return &seiv1alpha1.SidecarTLSSpec{
		IssuerName: "validator-ca",
		IssuerKind: "ClusterIssuer",
	}
}

func TestValidatorPlanner_BuildPlan_SidecarTLSTasksSequencedBeforeStatefulSet(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testValidatorName, Namespace: sourceChainID},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:   sourceChainID,
			Image:     testSeidImage,
			Validator: &seiv1alpha1.ValidatorSpec{},
			Sidecar:   &seiv1alpha1.SidecarConfig{TLS: tlsSpec()},
		},
	}
	plan, err := (&validatorPlanner{}).BuildPlan(node)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	assertTLSTasksBeforeStatefulSet(t, plan)
}

// assertTLSTasksBeforeStatefulSet covers the invariant that the TLS
// side-resources (Certificate, ConfigMap) are emitted before the
// StatefulSet apply. The pod-spec includes the rbac-proxy ConfigMap
// + TLS Secret as required volumes; if those don't exist the pod
// stays Pending. Regression-locks the cursor-bot HIGH from PR #223.
func assertTLSTasksBeforeStatefulSet(t *testing.T, plan *seiv1alpha1.TaskPlan) {
	t.Helper()
	certIdx := indexOfTaskType(plan, task.TaskTypeApplySidecarCert)
	cfgIdx := indexOfTaskType(plan, task.TaskTypeApplyRBACProxyConfig)
	stsIdx := indexOfTaskType(plan, task.TaskTypeApplyStatefulSet)

	if certIdx < 0 || cfgIdx < 0 {
		t.Fatalf("plan must contain both %s and %s; got %v",
			task.TaskTypeApplySidecarCert, task.TaskTypeApplyRBACProxyConfig, taskTypes(plan))
	}
	if certIdx >= stsIdx || cfgIdx >= stsIdx {
		t.Fatalf("expected sidecar-cert(%d), rbac-proxy-config(%d) < apply-statefulset(%d); got %v",
			certIdx, cfgIdx, stsIdx, taskTypes(plan))
	}
}

func TestBuildGenesisPlan_SidecarTLSTasksSequencedBeforeStatefulSet(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testValidatorName, Namespace: sourceChainID},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:   sourceChainID,
			Image:     testSeidImage,
			Validator: &seiv1alpha1.ValidatorSpec{GenesisCeremony: &seiv1alpha1.GenesisCeremonyNodeConfig{}},
			Sidecar:   &seiv1alpha1.SidecarConfig{TLS: tlsSpec()},
		},
	}
	plan, err := buildGenesisPlan(node)
	if err != nil {
		t.Fatalf("buildGenesisPlan: %v", err)
	}
	assertTLSTasksBeforeStatefulSet(t, plan)
}
