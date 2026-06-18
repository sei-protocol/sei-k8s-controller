package provisionnode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/taskruntime"
)

const (
	testNamespace      = "nightly"
	testWorkflowName   = "wf-test"
	testWorkflowVarsCM = "workflow-vars-wf-test"
	testRole           = "rpc"
	testChainID        = "bench-1"
	testImage          = "ghcr.io/sei/sei-chain:abc123"
	testNetwork        = "bench-1"
	varKeyChainID      = "CHAIN_ID"
	varKeyImage        = "IMAGE"

	testBase  = testChainID + "-" + testRole // "bench-1-rpc"
	testNode0 = testBase + "-0"              // "bench-1-rpc-0"
	testNode1 = testBase + "-1"              // "bench-1-rpc-1"

	tmSyncInfoField = "sync_info"
	tmHeightField   = "latest_block_height"
)

const fullNodeTmpl = `apiVersion: sei.io/v1alpha1
kind: SeiNode
metadata:
  name: PLACEHOLDER
spec:
  chainId: {{ .CHAIN_ID }}
  image: {{ .IMAGE }}
  fullNode: {}
`

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		seiv1alpha1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func writeTmpl(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "node.yaml.tmpl")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func testWorkflow() taskruntime.WorkflowIdentity {
	return taskruntime.WorkflowIdentity{Name: testWorkflowName, UID: "uid-test", Namespace: testNamespace}
}

func baseParams() Params {
	return Params{
		Role:         testRole,
		Name:         testChainID + "-" + testRole,
		TemplatePath: "x.yaml.tmpl",
		Replicas:     2,
		Workflow:     testWorkflow(),
	}
}

// --- renderNode -----------------------------------------------------------

func TestRenderNode_SubstitutesVarsAndInjectsOrdinal(t *testing.T) {
	path := writeTmpl(t, `apiVersion: sei.io/v1alpha1
kind: SeiNode
metadata:
  name: {{ .NODE_NAME }}
spec:
  chainId: {{ .CHAIN_ID }}
  image: {{ .IMAGE }}
  fullNode: {}
  overrides:
    ordinal: "{{ .ORDINAL }}"
`)
	p := baseParams()
	p.Name = testBase
	p.TemplatePath = path
	p.Vars = map[string]string{varKeyChainID: testChainID, varKeyImage: testImage}

	node, err := renderNode(p, 1)
	if err != nil {
		t.Fatalf("renderNode: %v", err)
	}
	if node.Spec.ChainID != testChainID {
		t.Errorf("ChainID = %q", node.Spec.ChainID)
	}
	if node.Spec.Image != testImage {
		t.Errorf("Image = %q", node.Spec.Image)
	}
	// .NODE_NAME and .ORDINAL injected for ordinal 1.
	if node.Name != testNode1 {
		t.Errorf("NODE_NAME injection: name = %q, want bench-1-rpc-1", node.Name)
	}
	if got := node.Spec.Overrides["ordinal"]; got != "1" {
		t.Errorf("ORDINAL injection: overrides[ordinal] = %q, want 1", got)
	}
}

func TestRenderNode_MissingVarFailsRender(t *testing.T) {
	path := writeTmpl(t, fullNodeTmpl)
	p := baseParams()
	p.TemplatePath = path
	p.Vars = map[string]string{varKeyChainID: testChainID} // IMAGE missing
	if _, err := renderNode(p, 0); err == nil {
		t.Fatalf("expected error: IMAGE not provided")
	}
}

func TestRenderNode_StrictUnmarshalCatchesTypos(t *testing.T) {
	path := writeTmpl(t, `apiVersion: sei.io/v1alpha1
kind: SeiNode
metadata:
  name: PLACEHOLDER
spec:
  chainId: {{ .CHAIN_ID }}
  imagge: {{ .IMAGE }}
  fullNode: {}
`)
	p := baseParams()
	p.TemplatePath = path
	p.Vars = map[string]string{varKeyChainID: testChainID, varKeyImage: testImage}
	if _, err := renderNode(p, 0); err == nil {
		t.Fatalf("expected strict-unmarshal error on `imagge` typo")
	}
}

// --- validateParams (collision guards) ------------------------------------

func TestValidateParams(t *testing.T) {
	full := Params{
		Role:         testRole,
		TemplatePath: "x.yaml.tmpl",
		Replicas:     2,
		Workflow:     testWorkflow(),
	}
	cases := []struct {
		name string
		mut  func(*Params)
		want bool
	}{
		{"complete", func(*Params) {}, false},
		{"missing role", func(p *Params) { p.Role = "" }, true},
		{"missing template", func(p *Params) { p.TemplatePath = "" }, true},
		{"replicas zero", func(p *Params) { p.Replicas = 0 }, true},
		{"missing workflow.Name", func(p *Params) { p.Workflow.Name = "" }, true},
		{"ORDINAL collision", func(p *Params) { p.Vars = map[string]string{"ORDINAL": "x"} }, true},
		{"NODE_NAME collision", func(p *Params) { p.Vars = map[string]string{"NODE_NAME": "x"} }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := full
			tc.mut(&p)
			err := validateParams(p)
			if (err != nil) != tc.want {
				t.Fatalf("validateParams err=%v wantErr=%v", err, tc.want)
			}
		})
	}
}

// --- stampMetadata (object labels + peer wiring) --------------------------

func TestStampMetadata_NamingAndOwnerRef(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			// Template author smuggling a bogus ownerRef: must be overwritten.
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "evil/v1", Kind: "Bogus", Name: "smuggled", UID: "bad",
			}},
		},
	}
	p := baseParams()
	p.Name = testBase
	stampMetadata(node, p, 1)

	if node.Name != testNode1 {
		t.Fatalf("name = %q, want bench-1-rpc-1", node.Name)
	}
	if node.Namespace != testNamespace {
		t.Fatalf("namespace = %q", node.Namespace)
	}
	if len(node.OwnerReferences) != 1 || node.OwnerReferences[0].Kind != "Workflow" {
		t.Fatalf("ownerRef not replaced: %+v", node.OwnerReferences)
	}
}

func TestStampMetadata_ObjectLabels_WithNetwork(t *testing.T) {
	node := &seiv1alpha1.SeiNode{}
	p := baseParams()
	p.Network = testNetwork
	stampMetadata(node, p, 0)

	if got := node.Labels[labelRole]; got != roleValueNode {
		t.Errorf("label %s = %q, want %q", labelRole, got, roleValueNode)
	}
	if got := node.Labels[labelSeiNetwork]; got != testNetwork {
		t.Errorf("label %s = %q, want %q", labelSeiNetwork, got, testNetwork)
	}
	// Producer-contract literals — must match WS-A's seictl node apply.
	if labelRole != "sei.io/role" || roleValueNode != "node" || labelSeiNetwork != "sei.io/seinetwork" {
		t.Fatalf("object-label producer contract drifted: %s=%s, %s", labelRole, roleValueNode, labelSeiNetwork)
	}
}

func TestStampMetadata_ObjectLabels_NoNetwork_OmitsNetworkLabel(t *testing.T) {
	node := &seiv1alpha1.SeiNode{}
	p := baseParams()
	p.Network = "" // no --network
	stampMetadata(node, p, 0)

	if got := node.Labels[labelRole]; got != roleValueNode {
		t.Errorf("label %s = %q, want %q (unconditional)", labelRole, got, roleValueNode)
	}
	if _, ok := node.Labels[labelSeiNetwork]; ok {
		t.Errorf("label %s present without --network; must be OMITTED, not stamped empty", labelSeiNetwork)
	}
}

func TestStampMetadata_PeerWiring_WithNetwork(t *testing.T) {
	node := &seiv1alpha1.SeiNode{}
	p := baseParams()
	p.Network = testNetwork
	p.NetworkNamespace = "genesis-ns"
	stampMetadata(node, p, 0)

	if len(node.Spec.Peers) != 1 {
		t.Fatalf("peers = %d, want 1 synthesized", len(node.Spec.Peers))
	}
	lbl := node.Spec.Peers[0].Label
	if lbl == nil {
		t.Fatalf("synthesized peer is not a LabelPeerSource: %+v", node.Spec.Peers[0])
	}
	if got := lbl.Selector[labelSeiNetwork]; got != testNetwork {
		t.Errorf("peer selector %s = %q, want %q", labelSeiNetwork, got, testNetwork)
	}
	if lbl.Namespace != "genesis-ns" {
		t.Errorf("peer namespace = %q, want genesis-ns", lbl.Namespace)
	}
}

func TestStampMetadata_NoNetwork_NoSynthesizedPeer(t *testing.T) {
	node := &seiv1alpha1.SeiNode{}
	p := baseParams()
	p.Network = ""
	stampMetadata(node, p, 0)
	if len(node.Spec.Peers) != 0 {
		t.Fatalf("peers = %d, want 0 (no --network)", len(node.Spec.Peers))
	}
}

func TestStampMetadata_PreservesTemplatePeer_Appends(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		Spec: seiv1alpha1.SeiNodeSpec{
			Peers: []seiv1alpha1.PeerSource{{
				Static: &seiv1alpha1.StaticPeerSource{Addresses: []string{"id@1.2.3.4:26656"}},
			}},
		},
	}
	p := baseParams()
	p.Network = testNetwork
	stampMetadata(node, p, 0)

	if len(node.Spec.Peers) != 2 {
		t.Fatalf("peers = %d, want 2 (template static + synthesized label)", len(node.Spec.Peers))
	}
	if node.Spec.Peers[0].Static == nil {
		t.Errorf("template static peer not preserved as first element: %+v", node.Spec.Peers[0])
	}
	if node.Spec.Peers[1].Label == nil {
		t.Errorf("synthesized label peer not appended as second element: %+v", node.Spec.Peers[1])
	}
}

// --- waitForRunning -------------------------------------------------------

func TestWaitForRunning(t *testing.T) {
	cases := []struct {
		name    string
		phase   seiv1alpha1.SeiNodePhase
		wantErr bool
		taskErr bool // expect a task-class (terminal) error
	}{
		{"running", seiv1alpha1.PhaseRunning, false, false},
		{"failed", seiv1alpha1.PhaseFailed, true, true},
		{"pending times out", seiv1alpha1.PhasePending, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := &seiv1alpha1.SeiNode{
				ObjectMeta: metav1.ObjectMeta{Name: testNode0, Namespace: testNamespace},
				Status:     seiv1alpha1.SeiNodeStatus{Phase: tc.phase},
			}
			c := fake.NewClientBuilder().
				WithScheme(newScheme(t)).
				WithObjects(node).
				WithStatusSubresource(&seiv1alpha1.SeiNode{}).
				Build()

			err := waitForRunning(context.Background(), c, testNamespace,
				[]string{testNode0}, 200*time.Millisecond, 20*time.Millisecond)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if tc.taskErr && taskruntime.ExitCodeFor(err) != taskruntime.ExitTaskFailure {
				t.Fatalf("Failed phase should yield a task-class error, got exit code %d", taskruntime.ExitCodeFor(err))
			}
		})
	}
}

// --- readiness probe (stage 2 TM, stage 3 EVM) ----------------------------

func TestTendermintStatusResponse_LatestHeight(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"jsonrpc envelope", `{"result":{"sync_info":{"latest_block_height":"42"}}}`, "42"},
		{"bare", `{"sync_info":{"latest_block_height":"7"}}`, "7"},
		{"empty", `{}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var r tendermintStatusResponse
			if err := json.Unmarshal([]byte(tc.body), &r); err != nil {
				t.Fatal(err)
			}
			if got := r.latestHeight(); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWaitForFirstBlock_HeightGtZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			tmSyncInfoField: map[string]any{tmHeightField: "5"},
		})
	}))
	defer srv.Close()
	if err := waitForFirstBlock(context.Background(), srv.Client(), srv.URL, time.Second, 10*time.Millisecond); err != nil {
		t.Fatalf("waitForFirstBlock: %v", err)
	}
}

func TestWaitForEVMReady_BoundListener(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": "0x10"})
	}))
	defer srv.Close()
	if err := waitForEVMReady(context.Background(), srv.Client(), srv.URL, time.Second, 10*time.Millisecond); err != nil {
		t.Fatalf("waitForEVMReady: %v", err)
	}
}

func TestWaitForEVMReady_NotBound_InfraFailsAfterTimeout(t *testing.T) {
	// 503 == listener not yet up; must keep polling, then infra-fail.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	err := waitForEVMReady(context.Background(), srv.Client(), srv.URL, 150*time.Millisecond, 20*time.Millisecond)
	if err == nil {
		t.Fatalf("expected infra-fail on never-bound EVM listener")
	}
	if taskruntime.ExitCodeFor(err) != taskruntime.ExitInfraError {
		t.Fatalf("EVM-not-ready should be infra-class, got exit code %d", taskruntime.ExitCodeFor(err))
	}
}

// TestRun_PublishBlockedWhileEVMDialFails is the finding-2 gate: TM reports
// height>0 but the EVM listener never binds, so publish must NOT proceed and
// no workflow-vars are written.
func TestRun_PublishBlockedWhileEVMDialFails(t *testing.T) {
	w := testWorkflow()
	tmplPath := writeTmpl(t, fullNodeTmpl)
	vars := map[string]string{varKeyChainID: testChainID, varKeyImage: testImage}

	// TM /status answers height>0; EVM POST always 503 (never bound).
	var tmHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet { // TM /status
			tmHits.Add(1)
			_ = json.NewEncoder(rw).Encode(map[string]any{
				tmSyncInfoField: map[string]any{tmHeightField: "9"},
			})
			return
		}
		rw.WriteHeader(http.StatusServiceUnavailable) // EVM eth_blockNumber
	}))
	defer srv.Close()

	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testChainID + "-" + testRole + "-0", Namespace: testNamespace},
		Spec:       seiv1alpha1.SeiNodeSpec{ChainID: testChainID, FullNode: &seiv1alpha1.FullNodeSpec{}},
		Status: seiv1alpha1.SeiNodeStatus{
			Phase: seiv1alpha1.PhaseRunning,
			Endpoint: &seiv1alpha1.NodeEndpointStatus{
				TendermintRpc:  srv.URL,
				TendermintRest: "http://rest.svc:1317",
				EvmJsonRpc:     srv.URL, // 503 path
			},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(node).
		WithStatusSubresource(&seiv1alpha1.SeiNode{}).
		Build()

	_, err := Run(context.Background(), c, Params{
		Role:              testRole,
		TemplatePath:      tmplPath,
		Vars:              vars,
		Replicas:          1,
		RunningTimeout:    time.Second,
		FirstBlockTimeout: 150 * time.Millisecond,
		PollInterval:      20 * time.Millisecond,
		HTTPClient:        srv.Client(),
		Workflow:          w,
	})
	if err == nil {
		t.Fatalf("Run should fail when EVM never binds even at TM height>0")
	}
	if tmHits.Load() == 0 {
		t.Fatalf("TM stage was never reached")
	}
	// No workflow-vars CM must be written — publish was blocked.
	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testWorkflowVarsCM}, cm); err == nil {
		t.Fatalf("workflow-vars CM was written despite blocked publish: %+v", cm.Data)
	}
}

// --- publish assembly (the contract test) ---------------------------------

func fakeNode(name, evm, tmRPC, tmREST string) *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Status: seiv1alpha1.SeiNodeStatus{
			Phase: seiv1alpha1.PhaseRunning,
			Endpoint: &seiv1alpha1.NodeEndpointStatus{
				EvmJsonRpc:     evm,
				TendermintRpc:  tmRPC,
				TendermintRest: tmREST,
			},
		},
	}
}

func TestPublishEndpoints_AssemblesAllFiveKeys(t *testing.T) {
	w := testWorkflow()
	nodes := []*seiv1alpha1.SeiNode{
		fakeNode("bench-1-rpc-0", "http://bench-1-rpc-0.nightly.svc:8545", "http://bench-1-rpc-0.nightly.svc:26657", "http://bench-1-rpc-0.nightly.svc:1317"),
		fakeNode("bench-1-rpc-1", "http://bench-1-rpc-1.nightly.svc:8545", "http://bench-1-rpc-1.nightly.svc:26657", "http://bench-1-rpc-1.nightly.svc:1317"),
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()

	evmList, err := publishEndpoints(context.Background(), c, w, testRole, testChainID, nodes)
	if err != nil {
		t.Fatalf("publishEndpoints: %v", err)
	}
	wantList := "http://bench-1-rpc-0.nightly.svc:8545,http://bench-1-rpc-1.nightly.svc:8545"
	if evmList != wantList {
		t.Fatalf("returned EVM list = %q, want %q", evmList, wantList)
	}

	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testWorkflowVarsCM}, cm); err != nil {
		t.Fatalf("get CM: %v", err)
	}
	want := map[string]string{
		"CHAIN_ID":         testChainID,
		"RPC_EVM_RPC_LIST": wantList,
		"RPC_EVM_RPC":      "http://bench-1-rpc-0.nightly.svc:8545", // node-0 scalar
		"RPC_TM_RPC":       "http://bench-1-rpc-0.nightly.svc:26657",
		"RPC_REST":         "http://bench-1-rpc-0.nightly.svc:1317",
	}
	for k, v := range want {
		if cm.Data[k] != v {
			t.Errorf("CM[%s] = %q, want %q", k, cm.Data[k], v)
		}
	}
}

func TestPublishEndpoints_EmptyGuards(t *testing.T) {
	w := testWorkflow()
	cases := []struct {
		name  string
		nodes []*seiv1alpha1.SeiNode
	}{
		{
			"nil endpoint",
			[]*seiv1alpha1.SeiNode{{ObjectMeta: metav1.ObjectMeta{Name: "n0"}, Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseRunning}}},
		},
		{
			"empty evmJsonRpc on node-1",
			[]*seiv1alpha1.SeiNode{
				fakeNode("n0", "http://n0:8545", "http://n0:26657", "http://n0:1317"),
				fakeNode("n1", "", "http://n1:26657", "http://n1:1317"),
			},
		},
		{
			"empty tendermintRpc on node-0 (finding 6c)",
			[]*seiv1alpha1.SeiNode{fakeNode("n0", "http://n0:8545", "", "http://n0:1317")},
		},
		{
			"empty tendermintRest on node-0",
			[]*seiv1alpha1.SeiNode{fakeNode("n0", "http://n0:8545", "http://n0:26657", "")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
			_, err := publishEndpoints(context.Background(), c, w, testRole, testChainID, tc.nodes)
			if err == nil {
				t.Fatalf("expected infra-fail empty-guard error")
			}
			if taskruntime.ExitCodeFor(err) != taskruntime.ExitInfraError {
				t.Fatalf("empty-guard should be infra-class, got exit code %d", taskruntime.ExitCodeFor(err))
			}
		})
	}
}

// --- Run end-to-end fan-out (naming + happy publish) ----------------------

func TestRun_FanOutNamingAndPublish(t *testing.T) {
	w := testWorkflow()
	tmplPath := writeTmpl(t, fullNodeTmpl)
	vars := map[string]string{varKeyChainID: testChainID, varKeyImage: testImage}

	// Healthy TM + EVM for every node.
	srv := healthyRPCServer(t)
	defer srv.Close()

	// Pre-stage N=2 SeiNodes already Running with endpoints (the controller's
	// job; we test seitask's wait+probe+publish, not reconcile).
	objs := make([]*seiv1alpha1.SeiNode, 0, 2)
	for i := range 2 {
		n := &seiv1alpha1.SeiNode{
			ObjectMeta: metav1.ObjectMeta{Name: nodeName(testChainID+"-"+testRole, i), Namespace: testNamespace},
			Spec:       seiv1alpha1.SeiNodeSpec{ChainID: testChainID, FullNode: &seiv1alpha1.FullNodeSpec{}},
			Status: seiv1alpha1.SeiNodeStatus{
				Phase: seiv1alpha1.PhaseRunning,
				Endpoint: &seiv1alpha1.NodeEndpointStatus{
					EvmJsonRpc:     srv.URL,
					TendermintRpc:  srv.URL,
					TendermintRest: "http://rest.svc:1317",
				},
			},
		}
		objs = append(objs, n)
	}
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(objs[0], objs[1]).
		WithStatusSubresource(&seiv1alpha1.SeiNode{}).
		Build()

	res, err := Run(context.Background(), c, Params{
		Role:              testRole,
		Name:              testChainID + "-" + testRole,
		TemplatePath:      tmplPath,
		Vars:              vars,
		Replicas:          2,
		Network:           testNetwork,
		RunningTimeout:    time.Second,
		FirstBlockTimeout: time.Second,
		PollInterval:      10 * time.Millisecond,
		HTTPClient:        srv.Client(),
		Workflow:          w,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantNames := []string{testNode0, testNode1}
	if len(res.Names) != 2 || res.Names[0] != wantNames[0] || res.Names[1] != wantNames[1] {
		t.Fatalf("fan-out names = %v, want %v", res.Names, wantNames)
	}

	// Pre-staged objects already exist (the AlreadyExists path), so the
	// object-label producer contract is exercised by the stampMetadata unit
	// tests; here we assert fan-out naming (above) and the publish CM (below).
	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testWorkflowVarsCM}, cm); err != nil {
		t.Fatalf("get CM: %v", err)
	}
	if cm.Data["CHAIN_ID"] != testChainID {
		t.Errorf("CHAIN_ID = %q", cm.Data["CHAIN_ID"])
	}
	if cm.Data["RPC_EVM_RPC_LIST"] == "" {
		t.Errorf("RPC_EVM_RPC_LIST empty")
	}
}

func healthyRPCServer(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodGet { // TM /status
			_ = json.NewEncoder(w).Encode(map[string]any{
				tmSyncInfoField: map[string]any{tmHeightField: "12"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": "0x10"})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestBundledTemplates_RenderClean ensures every bundled SeiNode (rpc follower)
// scenario template renders + strict-unmarshals against a representative --var
// set. Guards against template-vs-schema drift after a CRD field rename. The
// SeiNetwork genesis/validator templates are validated in provisionsnd.
func TestBundledTemplates_RenderClean(t *testing.T) {
	repoRoot, err := filepath.Abs("../../../")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"load-test", "release-test"} {
		t.Run(scenario+"/rpc.yaml.tmpl", func(t *testing.T) {
			p := Params{
				TemplatePath: filepath.Join(repoRoot, "scenarios", scenario, "rpc.yaml.tmpl"),
				Vars:         map[string]string{varKeyChainID: testChainID, varKeyImage: testImage},
			}
			node, err := renderNode(p, 0)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if node.Spec.ChainID != testChainID {
				t.Fatalf("chainId = %q, want %q", node.Spec.ChainID, testChainID)
			}
			if node.Spec.FullNode == nil {
				t.Fatalf("rpc template must render a fullNode SeiNode; spec = %+v", node.Spec)
			}
		})
	}
}
