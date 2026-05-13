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

func TestValidatorPlanner_BuildPlan_SidecarTLSTasksSequencedBeforeStatefulSet(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testValidatorName, Namespace: sourceChainID},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:   sourceChainID,
			Image:     testSeidImage,
			Validator: &seiv1alpha1.ValidatorSpec{},
			Sidecar: &seiv1alpha1.SidecarConfig{
				TLS: &seiv1alpha1.SidecarTLSSpec{
					IssuerRef: seiv1alpha1.CertManagerIssuerRef{
						Name:  "validator-ca",
						Kind:  "ClusterIssuer",
						Group: "cert-manager.io",
					},
				},
			},
		},
	}
	plan, err := (&validatorPlanner{}).BuildPlan(node)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	certIdx := indexOfTaskType(plan, task.TaskTypeApplySidecarCert)
	cfgIdx := indexOfTaskType(plan, task.TaskTypeApplyRBACProxyConfig)
	stsIdx := indexOfTaskType(plan, task.TaskTypeApplyStatefulSet)

	if certIdx < 0 || cfgIdx < 0 {
		t.Fatalf("plan must contain both %s and %s; got %v",
			task.TaskTypeApplySidecarCert, task.TaskTypeApplyRBACProxyConfig, taskTypes(plan))
	}
	// Both must precede the StatefulSet apply — the cert Secret + the
	// ConfigMap must exist by the time the pod schedules.
	if certIdx >= stsIdx || cfgIdx >= stsIdx {
		t.Fatalf("expected sidecar-cert(%d), rbac-proxy-config(%d) < apply-statefulset(%d); got %v",
			certIdx, cfgIdx, stsIdx, taskTypes(plan))
	}
}
