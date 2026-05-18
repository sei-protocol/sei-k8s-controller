package planner

import (
	"fmt"
	"testing"

	. "github.com/onsi/gomega"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
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
		Status: seiv1alpha1.SeiNodeStatus{CurrentSidecarTLSSecretName: testValidatorName + "-tls"},
	}
	got := SidecarURLForNode(node)
	want := fmt.Sprintf("https://%s-0.%s.sei.svc.cluster.local:8443", testValidatorName, testValidatorName)
	if got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
}

// TestSidecarURLForNode_TLSSpecButNotYetObserved: spec set, status empty
// → HTTP until ObserveSidecarTLS stamps the mirror.
func TestSidecarURLForNode_TLSSpecButNotYetObserved(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testValidatorName, Namespace: "sei"},
		Spec: seiv1alpha1.SeiNodeSpec{Sidecar: &seiv1alpha1.SidecarConfig{
			TLS: &seiv1alpha1.SidecarTLSSpec{SecretName: testValidatorName + "-tls"},
		}},
		// Status.CurrentSidecarTLSSecretName intentionally empty.
	}
	got := SidecarURLForNode(node)
	want := fmt.Sprintf("http://%s-0.%s.sei.svc.cluster.local:7777", testValidatorName, testValidatorName)
	if got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
}

// TestSidecarURLForNode_StatusSetWithoutSpec: status drives transport,
// not spec.
func TestSidecarURLForNode_StatusSetWithoutSpec(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testValidatorName, Namespace: "sei"},
		Status:     seiv1alpha1.SeiNodeStatus{CurrentSidecarTLSSecretName: testValidatorName + "-tls"},
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

// --- ResolvePlan TLS gating tests (locks e3fd5fc behavior) ---

func tlsGatedNode() *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testValidatorName, Namespace: sourceChainID},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:   sourceChainID,
			Image:     testSeidImage,
			Validator: &seiv1alpha1.ValidatorSpec{},
			Sidecar:   &seiv1alpha1.SidecarConfig{TLS: tlsSpec()},
		},
	}
}

func setTLSSecretReady(node *seiv1alpha1.SeiNode, status metav1.ConditionStatus, reason string) {
	apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type: seiv1alpha1.ConditionSidecarTLSSecretReady, Status: status, Reason: reason,
	})
}

func TestResolvePlan_TLSGate_BlocksWhenConditionFalse(t *testing.T) {
	g := NewWithT(t)
	node := tlsGatedNode()
	setTLSSecretReady(node, metav1.ConditionFalse, seiv1alpha1.ReasonTLSSecretNotFound)

	err := (&NodeResolver{}).ResolvePlan(t.Context(), node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(node.Status.Plan).To(BeNil(), "plan must not be built when TLS Secret is not ready")
}

func TestResolvePlan_TLSGate_BlocksWhenConditionAbsent(t *testing.T) {
	g := NewWithT(t)
	node := tlsGatedNode()
	// Condition never set — gate treats missing == false.

	err := (&NodeResolver{}).ResolvePlan(t.Context(), node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(node.Status.Plan).To(BeNil(), "plan must not be built when TLS condition is absent")
}

func TestResolvePlan_TLSGate_AllowsWhenConditionTrue(t *testing.T) {
	g := NewWithT(t)
	node := tlsGatedNode()
	setTLSSecretReady(node, metav1.ConditionTrue, seiv1alpha1.ReasonTLSSecretReady)

	err := (&NodeResolver{}).ResolvePlan(t.Context(), node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(node.Status.Plan).NotTo(BeNil(), "plan should build when TLS Secret is Ready")
}

func TestResolvePlan_TLSGate_NotEvaluatedWhenTLSDisabled(t *testing.T) {
	g := NewWithT(t)
	node := tlsGatedNode()
	node.Spec.Sidecar = nil // TLS not enabled — gate is bypassed regardless of condition state

	err := (&NodeResolver{}).ResolvePlan(t.Context(), node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(node.Status.Plan).NotTo(BeNil(), "plan should build for TLS-disabled SeiNode")
}

func TestResolvePlan_TLSGate_BlocksImageDrift(t *testing.T) {
	// Regression for the pre-e3fd5fc carve-out: image drift used to bypass
	// the gate. With the carve-out dropped, image drift on a Running node
	// with broken TLS must also be blocked.
	g := NewWithT(t)
	node := tlsGatedNode()
	node.Status.Phase = seiv1alpha1.PhaseRunning
	node.Status.CurrentImage = "old"
	node.Spec.Image = "new" // image drift
	setTLSSecretReady(node, metav1.ConditionFalse, seiv1alpha1.ReasonTLSSecretSANsMismatch)

	err := (&NodeResolver{}).ResolvePlan(t.Context(), node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(node.Status.Plan).To(BeNil(),
		"image-drift NodeUpdate plan must not build while TLS Secret is broken — a cycled pod would mount the broken cert")
}
