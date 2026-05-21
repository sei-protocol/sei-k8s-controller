package provisionsnd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
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
	testRole           = "validator"
	testPresetFullNode = "full-node"
	testChainID        = "bench-1"
	testImage          = "ghcr.io/sei/sei-chain:abc123"
	testAccountAddr    = "sei1abc"
)

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

func testWorkflow() taskruntime.WorkflowIdentity {
	return taskruntime.WorkflowIdentity{Name: testWorkflowName, UID: "uid-test", Namespace: testNamespace}
}

func TestLoadPreset_KnownPresets(t *testing.T) {
	for _, name := range []string{testRole, testPresetFullNode} {
		t.Run(name, func(t *testing.T) {
			snd, err := loadPreset(name)
			if err != nil {
				t.Fatalf("loadPreset(%q): %v", name, err)
			}
			if snd.Spec.Replicas < 1 {
				t.Fatalf("preset %q replicas < 1: %d", name, snd.Spec.Replicas)
			}
		})
	}
}

func TestLoadPreset_UnknownNameErrors(t *testing.T) {
	if _, err := loadPreset("does-not-exist"); err == nil {
		t.Fatalf("expected error for unknown preset")
	}
}

func TestApplyOverrides_StampsChainImageReplicas(t *testing.T) {
	snd, err := loadPreset(testRole)
	if err != nil {
		t.Fatalf("loadPreset: %v", err)
	}
	if err := applyOverrides(snd, Params{
		Role: testRole, Name: testRole, ChainID: testChainID, Image: testImage, Replicas: 7, Workflow: testWorkflow(),
	}); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}
	if snd.Spec.Template.Spec.ChainID != testChainID {
		t.Errorf("ChainID: %q", snd.Spec.Template.Spec.ChainID)
	}
	if snd.Spec.Template.Spec.Image != testImage {
		t.Errorf("Image: %q", snd.Spec.Template.Spec.Image)
	}
	if snd.Spec.Replicas != 7 {
		t.Errorf("Replicas: %d", snd.Spec.Replicas)
	}
	if snd.Spec.Genesis.ChainID != testChainID {
		t.Errorf("Genesis.ChainID: %q", snd.Spec.Genesis.ChainID)
	}
}

func TestApplyOverrides_MergesSeidOverrides(t *testing.T) {
	snd, err := loadPreset(testRole)
	if err != nil {
		t.Fatalf("loadPreset: %v", err)
	}
	if err := applyOverrides(snd, Params{
		Role: testRole, ChainID: testChainID, Image: testImage, Workflow: testWorkflow(),
		Overrides: map[string]string{
			"tx_index.indexer":     "kv",
			"mempool.ttl_duration": "60s",
		},
	}); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}
	got := snd.Spec.Template.Spec.Overrides
	if got["tx_index.indexer"] != "kv" || got["mempool.ttl_duration"] != "60s" {
		t.Fatalf("overrides not merged: %v", got)
	}
}

func TestApplyOverrides_AppendsGenesisAccountsOnValidatorPreset(t *testing.T) {
	snd, err := loadPreset(testRole)
	if err != nil {
		t.Fatalf("loadPreset: %v", err)
	}
	if err := applyOverrides(snd, Params{
		Role: testRole, ChainID: testChainID, Image: testImage, Workflow: testWorkflow(),
		GenesisAccounts: []seiv1alpha1.GenesisAccount{{Address: testAccountAddr, Balance: "1000usei"}},
	}); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}
	if len(snd.Spec.Genesis.Accounts) != 1 || snd.Spec.Genesis.Accounts[0].Address != testAccountAddr {
		t.Fatalf("genesis accounts: %+v", snd.Spec.Genesis.Accounts)
	}
}

func TestApplyOverrides_RejectsGenesisAccountsOnFullNodePreset(t *testing.T) {
	snd, err := loadPreset(testPresetFullNode)
	if err != nil {
		t.Fatalf("loadPreset: %v", err)
	}
	err = applyOverrides(snd, Params{
		Role: "rpc", ChainID: testChainID, Image: testImage, Workflow: testWorkflow(), Preset: testPresetFullNode,
		GenesisAccounts: []seiv1alpha1.GenesisAccount{{Address: testAccountAddr, Balance: "1usei"}},
	})
	if err == nil {
		t.Fatalf("expected error: --genesis-account on a preset without genesis block")
	}
}

func TestApplyOverrides_SubstitutesChainIntoFullNodePeerSelector(t *testing.T) {
	snd, err := loadPreset(testPresetFullNode)
	if err != nil {
		t.Fatalf("loadPreset: %v", err)
	}
	if err := applyOverrides(snd, Params{
		Role: "rpc", ChainID: testChainID, Image: testImage, Workflow: testWorkflow(),
	}); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}
	peers := snd.Spec.Template.Spec.Peers
	if len(peers) != 1 || peers[0].Label == nil {
		t.Fatalf("full-node preset's peer block: %+v", peers)
	}
	if got := peers[0].Label.Selector["sei.io/chain"]; got != testChainID {
		t.Fatalf("peers[0].label.selector[sei.io/chain] = %q; want %q", got, testChainID)
	}
}

func TestApplyOverrides_ValidatorPreset_NoPeers(t *testing.T) {
	snd, err := loadPreset(testRole)
	if err != nil {
		t.Fatalf("loadPreset: %v", err)
	}
	if err := applyOverrides(snd, Params{
		Role: testRole, ChainID: testChainID, Image: testImage, Workflow: testWorkflow(),
	}); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}
	if len(snd.Spec.Template.Spec.Peers) != 0 {
		t.Fatalf("peers materialized from nothing: %+v", snd.Spec.Template.Spec.Peers)
	}
}

func TestValidateParams(t *testing.T) {
	full := Params{
		Role:     testRole,
		Preset:   testRole,
		ChainID:  testChainID,
		Image:    testImage,
		Workflow: testWorkflow(),
	}
	cases := []struct {
		name string
		mut  func(*Params)
		want bool
	}{
		{"complete", func(*Params) {}, false},
		{"missing role", func(p *Params) { p.Role = "" }, true},
		{"missing preset", func(p *Params) { p.Preset = "" }, true},
		{"missing chain-id", func(p *Params) { p.ChainID = "" }, true},
		{"missing image", func(p *Params) { p.Image = "" }, true},
		{"missing workflow.Name", func(p *Params) { p.Workflow.Name = "" }, true},
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

func TestRun_EndToEnd_FakeClient(t *testing.T) {
	w := testWorkflow()

	// Pre-stage a Ready SND that the controller would have produced from a
	// Create. We re-use the production preset+overlay pipeline so the
	// pre-staged shape stays in sync with the path Run exercises.
	prestaged, err := loadPreset(testRole)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyOverrides(prestaged, Params{
		Role: testRole, Name: testRole, Preset: "validator", ChainID: testChainID, Image: testImage, Workflow: w,
	}); err != nil {
		t.Fatal(err)
	}
	stampMetadata(prestaged, Params{Role: testRole, Name: testRole, Workflow: w})
	prestaged.Status.Phase = seiv1alpha1.GroupPhaseReady
	prestaged.Status.Endpoints = &seiv1alpha1.Endpoints{
		TendermintRpc:  "http://tm.svc:26657",
		TendermintRest: "http://rest.svc:1317",
		Nodes:          []seiv1alpha1.NodeEndpoint{{Name: "validator-0", EvmJsonRpc: "http://evm.svc:8545"}},
	}

	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(prestaged).
		WithStatusSubresource(&seiv1alpha1.SeiNodeDeployment{}).
		Build()

	srv := fakeStatusServer(t, "42")
	defer srv.Close()
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testRole}, prestaged); err != nil {
		t.Fatal(err)
	}
	prestaged.Status.Endpoints.TendermintRpc = srv.URL
	if err := c.Status().Update(context.Background(), prestaged); err != nil {
		t.Fatal(err)
	}

	res, err := Run(context.Background(), c, Params{
		Role:              testRole,
		Name:              testRole,
		Preset:            "validator",
		ChainID:           testChainID,
		Image:             testImage,
		ReadyTimeout:      2 * time.Second,
		FirstBlockTimeout: 2 * time.Second,
		PollInterval:      10 * time.Millisecond,
		HTTPClient:        srv.Client(),
		Workflow:          w,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Name != testRole || res.ChainID != testChainID {
		t.Fatalf("Result: %+v", res)
	}

	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testWorkflowVarsCM}, cm); err != nil {
		t.Fatalf("get CM: %v", err)
	}
	if got := cm.Data["CHAIN_ID"]; got != testChainID {
		t.Fatalf("CHAIN_ID = %q", got)
	}
	if cm.Data["VALIDATOR_TM_RPC"] == "" {
		t.Fatalf("VALIDATOR_TM_RPC empty")
	}
	if cm.Data["VALIDATOR_EVM_RPC"] != "http://evm.svc:8545" {
		t.Fatalf("VALIDATOR_EVM_RPC = %q", cm.Data["VALIDATOR_EVM_RPC"])
	}
}

// fakeStatusServer returns a /status endpoint that reports the given height.
// Tests the JSON-RPC-envelope-or-bare tolerance: server emits the bare
// `{"sync_info": ...}` shape (Sei CometBFT's default).
func fakeStatusServer(t *testing.T, height string) *httptest.Server {
	t.Helper()
	var (
		mu    sync.Mutex
		calls int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sync_info": map[string]any{
				"latest_block_height": height,
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

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
