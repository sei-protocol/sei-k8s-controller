//go:build envtest

package envtest_test

import (
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// makeNamespace creates a unique namespace per test for isolation.
func makeNamespace(t *testing.T) string {
	t.Helper()
	g := NewWithT(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "sn-" + rand.String(8)}}
	g.Expect(testCli.Create(testCtx, ns)).To(Succeed())
	return ns.Name
}

// stateSyncNode returns a state-sync full node with the given spec-declared
// rpc-server witnesses.
func stateSyncNode(ns, name string, rpcServers []string) *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "envtest-1",
			Image:   "sei:latest",
			FullNode: &seiv1alpha1.FullNodeSpec{
				Snapshot: &seiv1alpha1.SnapshotSource{
					StateSync:  &seiv1alpha1.StateSyncSource{},
					RpcServers: rpcServers,
				},
			},
		},
	}
}

// Two well-formed host:port witnesses satisfy the admission floor.
func TestRpcServers_TwoEndpoints_Accepted(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	node := stateSyncNode(ns, "rpcservers-ok", []string{"a.eng.svc:26657", "b.eng.svc:26657"})
	g.Expect(testCli.Create(testCtx, node)).To(Succeed())
}

// A single witness is below the CometBFT floor (>=2 rpc-servers) and is
// rejected at admission, not at reconcile.
func TestRpcServers_OneEndpoint_Rejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	node := stateSyncNode(ns, "rpcservers-one", []string{"a.eng.svc:26657"})
	err := testCli.Create(testCtx, node)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("should have at least 2 items"))
}

// listType=set enforces uniqueness at admission, so duplicates can't be used
// to sneak under the floor ([a,a] is neither 2 witnesses nor accepted).
func TestRpcServers_Duplicates_Rejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	node := stateSyncNode(ns, "rpcservers-dup", []string{"a.eng.svc:26657", "a.eng.svc:26657"})
	err := testCli.Create(testCtx, node)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("Duplicate value"))
}

// Endpoints are bare host:port — the port is required (the sidecar derives the
// scheme from it; a portless endpoint would default to an unreachable URL) and
// a scheme prefix is malformed.
func TestRpcServers_MalformedEndpoints_Rejected(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
	}{
		{"missing port", "a.eng.svc"},
		{"scheme prefix", "http://a.eng.svc:26657"},
		{"embedded space", "a.eng .svc:26657"},
		{"comma-joined pair", "a.eng.svc,b.eng.svc:26657"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			ns := makeNamespace(t)
			node := stateSyncNode(ns, "rpcservers-bad", []string{tc.endpoint, "b.eng.svc:26657"})
			err := testCli.Create(testCtx, node)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("should match"))
		})
	}
}

// rpcServers is valid on the s3 source variant too — an s3 restore applies via
// CometBFT state-sync and verifies against the same witnesses.
func TestRpcServers_S3Source_Accepted(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	node := stateSyncNode(ns, "rpcservers-s3", []string{"a.eng.svc:26657", "b.eng.svc:26657"})
	node.Spec.FullNode.Snapshot.StateSync = nil
	node.Spec.FullNode.Snapshot.S3 = &seiv1alpha1.S3SnapshotSource{TargetHeight: 100}
	g.Expect(testCli.Create(testCtx, node)).To(Succeed())
}
