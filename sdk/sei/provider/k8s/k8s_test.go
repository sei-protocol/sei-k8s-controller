package k8s

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	testNS      = "nightly"
	testNet     = "chaos-net"
	testChainID = "sei-chaos-1"
	testImage   = "img:1"

	rpcRole   = "rpc"
	rpc0Name  = "rpc-0"
	rpc1Name  = "rpc-1"
	chaosNet0 = "chaos-net-0"
	resultKey = "result"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := seiv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// healthyRPC returns a server that answers TM /status caught-up at height>1 and
// EVM eth_blockNumber 200 (mirrors provision_test.go's healthyRPCServer).
func healthyRPC(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodGet { // TM /status
			_, _ = w.Write([]byte(statusFixture("12", false, false)))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, resultKey: "0x10"})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// providerWith builds a Provider backed by a fake client pre-staged with objs
// and the test HTTP client.
func providerWith(t *testing.T, hc *http.Client, objs ...runtime.Object) *Provider {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&seiv1alpha1.SeiNode{}, &seiv1alpha1.SeiNetwork{}).
		Build()
	return &Provider{c: c, httpClient: hc, defaultNS: testNS}
}

func runningNode(name, tmRPC, tmREST, evm string) *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec:       seiv1alpha1.SeiNodeSpec{ChainID: testChainID, Image: testImage, FullNode: &seiv1alpha1.FullNodeSpec{}},
		Status: seiv1alpha1.SeiNodeStatus{
			Phase: seiv1alpha1.PhaseRunning,
			Endpoint: &seiv1alpha1.NodeEndpointStatus{
				EvmJsonRpc: evm, TendermintRpc: tmRPC, TendermintRest: tmREST,
			},
		},
	}
}

func readyNetwork() *seiv1alpha1.SeiNetwork {
	return &seiv1alpha1.SeiNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: testNet, Namespace: testNS},
		Spec: seiv1alpha1.SeiNetworkSpec{
			Image: testImage, Replicas: 1,
			Genesis: seiv1alpha1.GenesisCeremonyConfig{ChainID: testChainID},
		},
		Status: seiv1alpha1.SeiNetworkStatus{
			Phase: seiv1alpha1.GroupPhaseReady,
			Endpoints: &seiv1alpha1.Endpoints{
				TendermintRpc:  "http://chaos-net-internal.nightly.svc:26657",
				TendermintRest: "http://chaos-net-internal.nightly.svc:1317",
				Nodes: []seiv1alpha1.NodeEndpoint{
					{Name: chaosNet0, EvmJsonRpc: "http://chaos-net-0.nightly.svc:8545", EvmWs: "ws://chaos-net-0.nightly.svc:8546"},
				},
			},
		},
	}
}

func TestProvisionNetwork_AppliesAndWaitsReady(t *testing.T) {
	p := providerWith(t, http.DefaultClient, readyNetwork())
	net, err := p.ProvisionNetwork(context.Background(), sei.NetworkSpec{
		Name: testNet, Namespace: testNS, ChainID: testChainID, Image: testImage, Replicas: 1,
		ReadyTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("ProvisionNetwork: %v", err)
	}
	ep := net.Endpoints()
	if ep.TendermintRPC == "" || ep.TendermintREST == "" {
		t.Errorf("aggregate TM endpoints missing: %+v", ep)
	}
	if len(ep.Nodes) != 1 || ep.Nodes[0].EvmJsonRPC == "" {
		t.Errorf("per-pod EVM endpoint missing: %+v", ep.Nodes)
	}
}

func TestProvisionNetwork_FailFastOnFailedPhase(t *testing.T) {
	failed := readyNetwork()
	failed.Status.Phase = seiv1alpha1.GroupPhaseFailed
	p := providerWith(t, http.DefaultClient, failed)
	_, err := p.ProvisionNetwork(context.Background(), sei.NetworkSpec{
		Name: testNet, Namespace: testNS, ChainID: testChainID, Image: testImage, Replicas: 1,
		ReadyTimeout: time.Second,
	})
	if err == nil || !sei.IsFailed(err) {
		t.Fatalf("Failed phase should yield ClassFailed, got %v", err)
	}
}

func TestProvisionFleet_FanOutProbeAndProject(t *testing.T) {
	srv := healthyRPC(t)
	// Pre-stage N=2 Running followers with endpoints pointing at the test server
	// (the controller's job; the SDK tests wait+probe+project, not reconcile).
	n0 := runningNode(rpc0Name, srv.URL, "http://rpc-0.nightly.svc:1317", srv.URL)
	n1 := runningNode(rpc1Name, srv.URL, "http://rpc-1.nightly.svc:1317", srv.URL)
	p := providerWith(t, srv.Client(), readyNetwork(), n0, n1)

	net := &networkHandle{p: p, namespace: testNS, name: testNet, net: readyNetwork()}
	fleet, err := p.ProvisionFleet(context.Background(), net, sei.FleetSpec{
		NamePrefix: rpcRole, Namespace: testNS, Image: testImage, Replicas: 2,
		RunningTimeout: time.Second, FirstBlockTimeout: time.Second, PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("ProvisionFleet: %v", err)
	}

	fe := fleet.Endpoints()
	if len(fe.Nodes) != 2 {
		t.Fatalf("fleet endpoints = %d nodes, want 2", len(fe.Nodes))
	}
	// The 4-field leaf must carry TM RPC/REST per node (not the EVM-only shape).
	for _, n := range fe.Nodes {
		if n.TendermintRPC == "" || n.TendermintREST == "" {
			t.Errorf("node %s dropped TM fields: %+v", n.Name, n)
		}
	}
	if got := fe.EVMRPCList(); len(got) != 2 {
		t.Errorf("EVMRPCList = %v, want 2 entries", got)
	}

	// The applied SeiNodes carry the canonical labels + peer wiring.
	applied := &seiv1alpha1.SeiNode{}
	if err := p.c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: rpc0Name}, applied); err != nil {
		t.Fatalf("get applied node: %v", err)
	}
	if applied.Labels[sei.LabelRole] != sei.RoleNode || applied.Labels[sei.LabelSeiNetwork] != testNet {
		t.Errorf("applied node missing canonical labels: %v", applied.Labels)
	}
}

// TestProvisionFleet_PeerWiresToNetworkNamespace pins the cross-namespace
// peer-wiring contract: when the genesis network lives in a namespace distinct
// from the provider default, followers must discover peers in the NETWORK's
// namespace. Wiring to the provider default leaves followers unable to find the
// genesis validators — surfacing later as a misleading ClassTimeout.
func TestProvisionFleet_PeerWiresToNetworkNamespace(t *testing.T) {
	const networkNS = "genesis-ns" // deliberately != provider default (testNS)
	srv := healthyRPC(t)

	net := readyNetwork()
	net.Namespace = networkNS
	// Followers provision into testNS (the provider default) while the network
	// lives in networkNS — the case that exposes the bug.
	n0 := runningNode(rpc0Name, srv.URL, "http://rpc-0.nightly.svc:1317", srv.URL)
	p := providerWith(t, srv.Client(), net, n0)

	handle := &networkHandle{p: p, namespace: networkNS, name: testNet, net: net}
	if _, err := p.ProvisionFleet(context.Background(), handle, sei.FleetSpec{
		NamePrefix: rpcRole, Namespace: testNS, Image: testImage, Replicas: 1,
		RunningTimeout: time.Second, FirstBlockTimeout: time.Second, PollInterval: 10 * time.Millisecond,
	}); err != nil {
		t.Fatalf("ProvisionFleet: %v", err)
	}

	applied := &seiv1alpha1.SeiNode{}
	if err := p.c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: rpc0Name}, applied); err != nil {
		t.Fatalf("get applied node: %v", err)
	}
	if len(applied.Spec.Peers) != 1 || applied.Spec.Peers[0].Label == nil {
		t.Fatalf("synthesized label peer missing: %+v", applied.Spec.Peers)
	}
	if got := applied.Spec.Peers[0].Label.Namespace; got != networkNS {
		t.Errorf("peer LabelPeerSource.Namespace = %q, want %q (network's namespace, not provider default)", got, networkNS)
	}
}

func TestProvisionFleet_FailFastOnFailedNode(t *testing.T) {
	failed := runningNode(rpc0Name, "", "", "")
	failed.Status.Phase = seiv1alpha1.PhaseFailed
	p := providerWith(t, http.DefaultClient, failed)
	net := &networkHandle{p: p, namespace: testNS, name: testNet, net: readyNetwork()}
	_, err := p.ProvisionFleet(context.Background(), net, sei.FleetSpec{
		NamePrefix: rpcRole, Namespace: testNS, Image: testImage, Replicas: 1,
		RunningTimeout: time.Second, FirstBlockTimeout: time.Second, PollInterval: 10 * time.Millisecond,
	})
	if err == nil || !sei.IsFailed(err) {
		t.Fatalf("Failed node should yield ClassFailed, got %v", err)
	}
}

// TestFleetEndpoints_PreserveOrdinalOrder pins the D7 ordering contract for the
// fleet projection: fleetHandle.Endpoints() iterates h.names, so the returned
// Nodes must come back in ordinal order regardless of apiserver/map iteration
// order. A harness indexes node-0 for the aggregate-TM semantics, so a reorder
// is a silent correctness break.
func TestFleetEndpoints_PreserveOrdinalOrder(t *testing.T) {
	// Stage three nodes; the handle's names fix the order.
	objs := []runtime.Object{
		runningNode(rpc0Name, "http://rpc-0:26657", "http://rpc-0:1317", "http://rpc-0:8545"),
		runningNode(rpc1Name, "http://rpc-1:26657", "http://rpc-1:1317", "http://rpc-1:8545"),
		runningNode("rpc-2", "http://rpc-2:26657", "http://rpc-2:1317", "http://rpc-2:8545"),
	}
	p := providerWith(t, http.DefaultClient, objs...)
	h := &fleetHandle{p: p, namespace: testNS, names: []string{rpc0Name, rpc1Name, "rpc-2"}}

	fe := h.Endpoints()
	want := []string{rpc0Name, rpc1Name, "rpc-2"}
	if len(fe.Nodes) != len(want) {
		t.Fatalf("fleet Nodes len = %d, want %d", len(fe.Nodes), len(want))
	}
	for i, w := range want {
		if fe.Nodes[i].Name != w {
			t.Errorf("fleet Nodes[%d].Name = %q, want %q (ordinal order broke)", i, fe.Nodes[i].Name, w)
		}
	}
	// And EVMRPCList carries that same fleet order.
	evm := fe.EVMRPCList()
	wantEVM := []string{"http://rpc-0:8545", "http://rpc-1:8545", "http://rpc-2:8545"}
	for i, w := range wantEVM {
		if evm[i] != w {
			t.Errorf("EVMRPCList[%d] = %q, want %q", i, evm[i], w)
		}
	}
}

// TestNetworkEndpoints_PreservePerPodOrder pins the per-pod order of the network
// projection: Endpoints.Nodes must mirror the source .status.endpoints.nodes
// order (D7 [0]=aggregate semantics, per-pod order stable).
func TestNetworkEndpoints_PreservePerPodOrder(t *testing.T) {
	net := readyNetwork()
	net.Status.Endpoints.Nodes = []seiv1alpha1.NodeEndpoint{
		{Name: "chaos-net-0", EvmJsonRpc: "http://chaos-net-0:8545"},
		{Name: "chaos-net-1", EvmJsonRpc: "http://chaos-net-1:8545"},
		{Name: "chaos-net-2", EvmJsonRpc: "http://chaos-net-2:8545"},
	}
	h := &networkHandle{namespace: testNS, name: testNet, net: net}
	got := h.Endpoints()
	want := []string{"chaos-net-0", "chaos-net-1", "chaos-net-2"}
	if len(got.Nodes) != len(want) {
		t.Fatalf("network Nodes len = %d, want %d", len(got.Nodes), len(want))
	}
	for i, w := range want {
		if got.Nodes[i].Name != w {
			t.Errorf("network Nodes[%d].Name = %q, want %q (per-pod order broke)", i, got.Nodes[i].Name, w)
		}
	}
}

func TestTeardown_Idempotent(t *testing.T) {
	p := providerWith(t, http.DefaultClient, runningNode(rpc0Name, "x", "y", "z"))
	h := &fleetHandle{p: p, namespace: testNS, names: []string{rpc0Name, rpc1Name}}

	// rpc-1 never existed; teardown must still succeed (idempotent).
	if err := h.Teardown(context.Background()); err != nil {
		t.Fatalf("first teardown: %v", err)
	}
	// Second teardown after everything is gone is also a no-op.
	if err := h.Teardown(context.Background()); err != nil {
		t.Fatalf("second teardown: %v", err)
	}
	got := &seiv1alpha1.SeiNode{}
	err := p.c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: rpc0Name}, got)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("rpc-0 should be deleted, got err=%v", err)
	}
}
