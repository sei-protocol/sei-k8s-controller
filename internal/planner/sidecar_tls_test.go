package planner

import (
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

func TestSidecarURLForNode_PlainHTTP(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testValidatorName, Namespace: "sei"},
	}
	got := SidecarURLForNode(node)
	want := fmt.Sprintf("http://%s-0.%s.sei.svc.cluster.local:7777", testValidatorName, testValidatorName)
	if got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
}

func TestSidecarURLForNode_TLSRoutesThroughProxyHTTPS(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testValidatorName, Namespace: "sei"},
		Spec: seiv1alpha1.SeiNodeSpec{Sidecar: &seiv1alpha1.SidecarConfig{
			TLS: &seiv1alpha1.SidecarTLSSpec{SecretName: testValidatorName + "-tls"},
		}},
	}
	got := SidecarURLForNode(node)
	want := fmt.Sprintf("https://%s-0.%s.sei.svc.cluster.local:8443", testValidatorName, testValidatorName)
	if got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
}

const (
	testValidatorName = "validator-0"
	testSeidImage     = "seid:v6.4.1"
)

func TestValidatorPlanner_BuildPlan_NoRBACProxyConfigTaskByDefault(t *testing.T) {
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
	if idx := indexOfTaskType(plan, task.TaskTypeApplyRBACProxyConfig); idx >= 0 {
		t.Fatalf("plan must not contain %s without spec.sidecar.tls; got %v",
			task.TaskTypeApplyRBACProxyConfig, taskTypes(plan))
	}
}

func tlsSpec() *seiv1alpha1.SidecarTLSSpec {
	return &seiv1alpha1.SidecarTLSSpec{SecretName: testValidatorName + "-tls"}
}

func TestValidatorPlanner_BuildPlan_RBACProxyConfigSequencedBeforeStatefulSet(t *testing.T) {
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
	assertRBACProxyConfigBeforeStatefulSet(t, plan)
}

// assertRBACProxyConfigBeforeStatefulSet locks the invariant that the
// kube-rbac-proxy authz ConfigMap is emitted before the StatefulSet
// apply. The pod-spec mounts the ConfigMap; if it doesn't exist the pod
// stays Pending. (Under the externalized-Secret design the TLS material
// itself is operator-provisioned and gated via the
// SidecarTLSSecretReady condition; only the authz ConfigMap is in-plan.)
func assertRBACProxyConfigBeforeStatefulSet(t *testing.T, plan *seiv1alpha1.TaskPlan) {
	t.Helper()
	cfgIdx := indexOfTaskType(plan, task.TaskTypeApplyRBACProxyConfig)
	stsIdx := indexOfTaskType(plan, task.TaskTypeApplyStatefulSet)

	if cfgIdx < 0 {
		t.Fatalf("plan must contain %s; got %v",
			task.TaskTypeApplyRBACProxyConfig, taskTypes(plan))
	}
	if cfgIdx >= stsIdx {
		t.Fatalf("expected rbac-proxy-config(%d) < apply-statefulset(%d); got %v",
			cfgIdx, stsIdx, taskTypes(plan))
	}
}

func TestBuildGenesisPlan_RBACProxyConfigSequencedBeforeStatefulSet(t *testing.T) {
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
	assertRBACProxyConfigBeforeStatefulSet(t, plan)
}
