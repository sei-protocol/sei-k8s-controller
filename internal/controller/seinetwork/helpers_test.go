package seinetwork

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	testSyncerOrd0 = "syncer-0"

	testNetworkName = "genesis-net"
	testNode0       = "genesis-net-0"
	testGroupNS     = "sei"
	testAPIVersion  = "sei.io/v1alpha1"
	testKind        = "SeiNetwork"

	testOverrideKey = "evm.http_port"
	testOverrideVal = "8545"
)

// newTestNetwork builds a SeiNetwork with a required genesis ceremony — the
// only shape the Kind admits. The controller synthesizes each child's
// validator role from these scalars.
func newTestNetwork(name, namespace string) *seiv1alpha1.SeiNetwork {
	return &seiv1alpha1.SeiNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: seiv1alpha1.SeiNetworkSpec{
			Image:    "ghcr.io/sei-protocol/seid:v1.0.0",
			Replicas: 3,
			Genesis:  seiv1alpha1.GenesisCeremonyConfig{ChainID: testNamespace},
			Sidecar:  &seiv1alpha1.SidecarConfig{Port: 7777},
		},
	}
}

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

func newPlanTestReconciler(t *testing.T, objs ...client.Object) *SeiNetworkReconciler {
	t.Helper()
	s := newPlanTestScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&seiv1alpha1.SeiNetwork{}).
		Build()
	return &SeiNetworkReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(100),
	}
}
