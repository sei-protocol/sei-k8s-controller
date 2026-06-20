package k8s

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

const (
	testNS    = "nightly"
	testNet   = "chaos-net"
	testImage = "img:1"

	rpc0Name  = "rpc-0"
	rpc1Name  = "rpc-1"
	resultKey = "result"
)

// TestMain shrinks the poll cadences so phase/probe tests resolve in
// milliseconds instead of seconds; the caller's ctx still owns the budget.
func TestMain(m *testing.M) {
	pollInterval = 10 * time.Millisecond
	probeInterval = 10 * time.Millisecond
	os.Exit(m.Run())
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := seiv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// healthyRPC answers TM /status with a height and EVM eth_blockNumber 200.
func healthyRPC(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodGet { // TM /status
			_, _ = w.Write([]byte(statusFixture("12", false)))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, resultKey: "0x10"})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// providerWith builds a Provider backed by a fake client pre-staged with objs.
func providerWith(t *testing.T, hc *http.Client, objs ...runtime.Object) *Provider {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&seiv1alpha1.SeiNode{}, &seiv1alpha1.SeiNetwork{}).
		Build()
	return &Provider{c: c, httpClient: hc, defaultNS: testNS}
}

func runningNode(tmRPC, evm string) *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: rpc0Name, Namespace: testNS},
		Spec:       seiv1alpha1.SeiNodeSpec{ChainID: testNet, Image: testImage, FullNode: &seiv1alpha1.FullNodeSpec{}},
		Status: seiv1alpha1.SeiNodeStatus{
			Phase:    seiv1alpha1.PhaseRunning,
			Endpoint: &seiv1alpha1.NodeEndpointStatus{EvmJsonRpc: evm, TendermintRpc: tmRPC},
		},
	}
}

func readyNetwork(tmRPC string) *seiv1alpha1.SeiNetwork {
	return &seiv1alpha1.SeiNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: testNet, Namespace: testNS},
		Spec: seiv1alpha1.SeiNetworkSpec{
			Image: testImage, Replicas: 1,
			Genesis: seiv1alpha1.GenesisCeremonyConfig{ChainID: testNet},
		},
		Status: seiv1alpha1.SeiNetworkStatus{
			Phase: seiv1alpha1.GroupPhaseReady,
			Endpoints: &seiv1alpha1.Endpoints{
				TendermintRpc:  tmRPC,
				TendermintRest: "http://chaos-net-internal.nightly.svc:1317",
			},
		},
	}
}

func TestCreateNetwork_AppliesAndReturnsHandle(t *testing.T) {
	p := providerWith(t, http.DefaultClient)
	h, err := p.CreateNetwork(context.Background(), sei.NetworkSpec{
		Name: testNet, Namespace: testNS, Image: testImage, Validators: 1,
	})
	if err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	if h.Name() != testNet || h.Namespace() != testNS {
		t.Fatalf("handle identity = %s/%s", h.Namespace(), h.Name())
	}
	// The object actually landed on the apiserver.
	got := &seiv1alpha1.SeiNetwork{}
	if err := p.c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: testNet}, got); err != nil {
		t.Fatalf("applied network not found: %v", err)
	}
	// Object() is the raw-CR escape hatch.
	if _, ok := h.Object().(*seiv1alpha1.SeiNetwork); !ok {
		t.Errorf("Object() did not return *SeiNetwork, got %T", h.Object())
	}
}

func TestNetworkWaitReady_PhaseAndProbe(t *testing.T) {
	srv := healthyRPC(t)
	p := providerWith(t, srv.Client(), readyNetwork(srv.URL))
	h := &networkHandle{p: p, namespace: testNS, name: testNet, net: &seiv1alpha1.SeiNetwork{}}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	// After WaitReady the cached status carries the endpoints.
	if h.TendermintRPC() != srv.URL || h.REST() == "" {
		t.Errorf("endpoints not projected after Ready: rpc=%q rest=%q", h.TendermintRPC(), h.REST())
	}
}

func TestNetworkWaitReady_FailFastOnFailedPhase(t *testing.T) {
	failed := readyNetwork("")
	failed.Status.Phase = seiv1alpha1.GroupPhaseFailed
	p := providerWith(t, http.DefaultClient, failed)
	h := &networkHandle{p: p, namespace: testNS, name: testNet, net: &seiv1alpha1.SeiNetwork{}}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := h.WaitReady(ctx)
	if err == nil {
		t.Fatal("Failed phase should error")
	}
	if sei.IsTimeout(err) {
		t.Fatalf("Failed phase must not be a timeout, got %v", err)
	}
}

// TestNetworkWaitReady_DeadlineIsTimeout pins the deadline->IsTimeout contract:
// a network that never reaches Ready under an elapsed ctx surfaces as
// sei.IsTimeout.
func TestNetworkWaitReady_DeadlineIsTimeout(t *testing.T) {
	pending := readyNetwork("")
	pending.Status.Phase = seiv1alpha1.GroupPhasePending
	p := providerWith(t, http.DefaultClient, pending)
	h := &networkHandle{p: p, namespace: testNS, name: testNet, net: &seiv1alpha1.SeiNetwork{}}

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	err := h.WaitReady(ctx)
	if err == nil {
		t.Fatal("never-Ready network should error on deadline")
	}
	if !sei.IsTimeout(err) {
		t.Fatalf("elapsed deadline should be sei.IsTimeout, got %v", err)
	}
}

func TestCreateNode_LabelsAndPeerWiring(t *testing.T) {
	p := providerWith(t, http.DefaultClient)
	h, err := p.CreateNode(context.Background(), sei.NodeSpec{
		Name: rpc0Name, Network: testNet, Namespace: testNS, Image: testImage,
	})
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	if h.Name() != rpc0Name {
		t.Fatalf("node name = %q", h.Name())
	}
	applied := &seiv1alpha1.SeiNode{}
	if err := p.c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: rpc0Name}, applied); err != nil {
		t.Fatalf("get applied node: %v", err)
	}
	if applied.Labels[sei.LabelRole] != sei.RoleNode || applied.Labels[sei.LabelSeiNetwork] != testNet {
		t.Errorf("applied node missing canonical labels: %v", applied.Labels)
	}
	if len(applied.Spec.Peers) != 1 || applied.Spec.Peers[0].Label == nil {
		t.Fatalf("synthesized label peer missing: %+v", applied.Spec.Peers)
	}
	if applied.Spec.Peers[0].Label.Selector[sei.LabelSeiNetwork] != testNet {
		t.Errorf("peer selector = %v", applied.Spec.Peers[0].Label.Selector)
	}
}

func TestCreateNode_PeerSelectorNamespace(t *testing.T) {
	// Co-located (NetworkNamespace==""): the peer selector searches the node's own
	// namespace. Cross-namespace: the selector searches the network's namespace,
	// where the genesis validators live, while the node object stays in its own.
	cases := []struct {
		name      string
		nodeNS    string
		networkNS string
		wantPeer  string
	}{
		{name: "co-located default", nodeNS: testNS, networkNS: "", wantPeer: testNS},
		{name: "cross-namespace", nodeNS: "rpc-ns", networkNS: "genesis-ns", wantPeer: "genesis-ns"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := providerWith(t, http.DefaultClient)
			if _, err := p.CreateNode(context.Background(), sei.NodeSpec{
				Name: rpc0Name, Network: testNet, Image: testImage,
				Namespace: tc.nodeNS, NetworkNamespace: tc.networkNS,
			}); err != nil {
				t.Fatalf("CreateNode: %v", err)
			}
			applied := &seiv1alpha1.SeiNode{}
			if err := p.c.Get(context.Background(), types.NamespacedName{Namespace: tc.nodeNS, Name: rpc0Name}, applied); err != nil {
				t.Fatalf("get applied node: %v", err)
			}
			// The node object lives in its own namespace, never the network's.
			if applied.Namespace != tc.nodeNS {
				t.Errorf("node namespace = %q, want %q", applied.Namespace, tc.nodeNS)
			}
			if len(applied.Spec.Peers) != 1 || applied.Spec.Peers[0].Label == nil {
				t.Fatalf("synthesized label peer missing: %+v", applied.Spec.Peers)
			}
			if got := applied.Spec.Peers[0].Label.Namespace; got != tc.wantPeer {
				t.Errorf("peer selector namespace = %q, want %q", got, tc.wantPeer)
			}
		})
	}
}

func TestNodeWaitReady_PhaseAndProbe(t *testing.T) {
	srv := healthyRPC(t)
	p := providerWith(t, srv.Client(), runningNode(srv.URL, srv.URL))
	h := &nodeHandle{p: p, namespace: testNS, name: rpc0Name, node: &seiv1alpha1.SeiNode{}}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if h.EVMRPC() != srv.URL || h.TendermintRPC() != srv.URL {
		t.Errorf("endpoints not projected: evm=%q tm=%q", h.EVMRPC(), h.TendermintRPC())
	}
}

func TestNodeWaitReady_FailFastOnFailedPhase(t *testing.T) {
	failed := runningNode("", "")
	failed.Status.Phase = seiv1alpha1.PhaseFailed
	p := providerWith(t, http.DefaultClient, failed)
	h := &nodeHandle{p: p, namespace: testNS, name: rpc0Name, node: &seiv1alpha1.SeiNode{}}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := h.WaitReady(ctx)
	if err == nil {
		t.Fatal("Failed phase should error")
	}
	if sei.IsTimeout(err) {
		t.Fatalf("Failed phase must not be a timeout, got %v", err)
	}
}

// TestNodeWaitReady_DeadlineIsTimeout: a node stuck pre-Running under an elapsed
// ctx surfaces as sei.IsTimeout.
func TestNodeWaitReady_DeadlineIsTimeout(t *testing.T) {
	pending := runningNode("", "")
	pending.Status.Phase = seiv1alpha1.SeiNodePhase("Pending")
	p := providerWith(t, http.DefaultClient, pending)
	h := &nodeHandle{p: p, namespace: testNS, name: rpc0Name, node: &seiv1alpha1.SeiNode{}}

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	err := h.WaitReady(ctx)
	if err == nil {
		t.Fatal("never-Running node should error on deadline")
	}
	if !sei.IsTimeout(err) {
		t.Fatalf("elapsed deadline should be sei.IsTimeout, got %v", err)
	}
}

func TestGetNetwork_ReadsBackStatus(t *testing.T) {
	srv := healthyRPC(t)
	p := providerWith(t, srv.Client(), readyNetwork(srv.URL))
	h, err := p.GetNetwork(context.Background(), testNet, testNS)
	if err != nil {
		t.Fatalf("GetNetwork: %v", err)
	}
	if h.TendermintRPC() != srv.URL {
		t.Errorf("GetNetwork did not project status endpoints: %q", h.TendermintRPC())
	}
}

func TestGetNode_NotFoundPassesThrough(t *testing.T) {
	p := providerWith(t, http.DefaultClient)
	_, err := p.GetNode(context.Background(), "absent", testNS)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("GetNode of a missing node should pass apierrors.IsNotFound, got %v", err)
	}
}

func TestDelete_Idempotent(t *testing.T) {
	p := providerWith(t, http.DefaultClient, runningNode("x", "z"))
	h := &nodeHandle{p: p, namespace: testNS, name: rpc0Name, node: &seiv1alpha1.SeiNode{}}

	if err := h.Delete(context.Background()); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	// Second delete after it is gone is also a no-op (NotFound is success).
	if err := h.Delete(context.Background()); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	got := &seiv1alpha1.SeiNode{}
	if err := p.c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: rpc0Name}, got); !apierrors.IsNotFound(err) {
		t.Fatalf("node should be deleted, got err=%v", err)
	}
}

func TestNetworkDelete_Idempotent(t *testing.T) {
	p := providerWith(t, http.DefaultClient)
	h := &networkHandle{p: p, namespace: testNS, name: testNet, net: &seiv1alpha1.SeiNetwork{}}
	// Network never existed; delete must still succeed.
	if err := h.Delete(context.Background()); err != nil {
		t.Fatalf("delete of absent network should be a no-op, got %v", err)
	}
}
