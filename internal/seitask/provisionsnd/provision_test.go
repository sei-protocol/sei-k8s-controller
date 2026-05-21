package provisionsnd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
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
	testRole           = "validator"
	testChainID        = "bench-1"
	testImage          = "ghcr.io/sei/sei-chain:abc123"
	testAdminAddress   = "sei1admin"
	varKeyChainID      = "CHAIN_ID"
	varKeyImage        = "IMAGE"
	varKeyAdminAddress = "ADMIN_ADDRESS"
)

const validatorTmpl = `apiVersion: sei.io/v1alpha1
kind: SeiNodeDeployment
metadata:
  name: PLACEHOLDER
spec:
  replicas: 4
  template:
    spec:
      chainId: {{ .CHAIN_ID }}
      image: {{ .IMAGE }}
      validator: {}
  genesis:
    chainId: {{ .CHAIN_ID }}
    accounts:
      - address: {{ .ADMIN_ADDRESS }}
        balance: 1000000000000usei
  updateStrategy:
    type: InPlace
`

const rpcTmpl = `apiVersion: sei.io/v1alpha1
kind: SeiNodeDeployment
metadata:
  name: PLACEHOLDER
spec:
  replicas: 2
  template:
    spec:
      chainId: {{ .CHAIN_ID }}
      image: {{ .IMAGE }}
      fullNode: {}
      peers:
        - label:
            selector:
              sei.io/chain: {{ .CHAIN_ID }}
  updateStrategy:
    type: InPlace
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
	p := filepath.Join(dir, "snd.yaml.tmpl")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func testWorkflow() taskruntime.WorkflowIdentity {
	return taskruntime.WorkflowIdentity{Name: testWorkflowName, UID: "uid-test", Namespace: testNamespace}
}

func TestRenderTemplate_SubstitutesVars(t *testing.T) {
	path := writeTmpl(t, validatorTmpl)
	snd, err := renderTemplate(path, map[string]string{
		varKeyChainID:      testChainID,
		varKeyImage:        testImage,
		varKeyAdminAddress: testAdminAddress,
	})
	if err != nil {
		t.Fatalf("renderTemplate: %v", err)
	}
	if snd.Spec.Template.Spec.ChainID != testChainID {
		t.Errorf("ChainID: %q", snd.Spec.Template.Spec.ChainID)
	}
	if snd.Spec.Template.Spec.Image != testImage {
		t.Errorf("Image: %q", snd.Spec.Template.Spec.Image)
	}
	if snd.Spec.Genesis.ChainID != testChainID {
		t.Errorf("Genesis.ChainID: %q", snd.Spec.Genesis.ChainID)
	}
	if len(snd.Spec.Genesis.Accounts) != 1 || snd.Spec.Genesis.Accounts[0].Address != testAdminAddress {
		t.Errorf("Genesis.Accounts: %+v", snd.Spec.Genesis.Accounts)
	}
}

func TestRenderTemplate_MissingVarFailsRender(t *testing.T) {
	path := writeTmpl(t, validatorTmpl)
	if _, err := renderTemplate(path, map[string]string{varKeyChainID: testChainID}); err == nil {
		t.Fatalf("expected error: IMAGE and ADMIN_ADDRESS not provided")
	}
}

func TestRenderTemplate_StrictUnmarshalCatchesTypos(t *testing.T) {
	tmpl := `apiVersion: sei.io/v1alpha1
kind: SeiNodeDeployment
metadata:
  name: PLACEHOLDER
spec:
  replcas: 4
  template:
    spec:
      chainId: {{ .CHAIN_ID }}
      image: {{ .IMAGE }}
      validator: {}
  updateStrategy:
    type: InPlace
`
	path := writeTmpl(t, tmpl)
	if _, err := renderTemplate(path, map[string]string{varKeyChainID: testChainID, varKeyImage: testImage}); err == nil {
		t.Fatalf("expected strict-unmarshal error on `replcas` typo")
	}
}

func TestRenderTemplate_FullNodePeerSelectorSubstitution(t *testing.T) {
	path := writeTmpl(t, rpcTmpl)
	snd, err := renderTemplate(path, map[string]string{
		varKeyChainID: testChainID,
		varKeyImage:   testImage,
	})
	if err != nil {
		t.Fatalf("renderTemplate: %v", err)
	}
	peers := snd.Spec.Template.Spec.Peers
	if len(peers) != 1 || peers[0].Label == nil {
		t.Fatalf("peers: %+v", peers)
	}
	if got := peers[0].Label.Selector["sei.io/chain"]; got != testChainID {
		t.Errorf("peers[0].label.selector[sei.io/chain] = %q; want %q", got, testChainID)
	}
}

// TestBundledTemplates_RenderClean ensures every bundled scenario template
// renders + strict-unmarshals against a representative --var set. Guards
// against template-vs-schema drift after a CRD field rename.
func TestBundledTemplates_RenderClean(t *testing.T) {
	repoRoot, err := filepath.Abs("../../../")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path string
		vars map[string]string
	}{
		{
			path: filepath.Join(repoRoot, "scenarios", "release-test", "validator.yaml.tmpl"),
			vars: map[string]string{varKeyChainID: "rel-test", varKeyImage: "img:1", varKeyAdminAddress: testAdminAddress},
		},
		{
			path: filepath.Join(repoRoot, "scenarios", "release-test", "rpc.yaml.tmpl"),
			vars: map[string]string{varKeyChainID: "rel-test", varKeyImage: "img:1"},
		},
	}
	for _, tc := range cases {
		t.Run(filepath.Base(tc.path), func(t *testing.T) {
			snd, err := renderTemplate(tc.path, tc.vars)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if snd.Spec.Template.Spec.ChainID == "" {
				t.Fatalf("ChainID empty after render: %+v", snd.Spec.Template.Spec)
			}
		})
	}
}

func TestStampMetadata_AssignsOwnerRefsNotAppend(t *testing.T) {
	snd := &seiv1alpha1.SeiNodeDeployment{
		// Template author smuggling a bogus ownerRef: stampMetadata MUST
		// overwrite, not append.
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "evil/v1", Kind: "Bogus", Name: "smuggled", UID: "bad",
			}},
		},
	}
	stampMetadata(snd, Params{Role: testRole, Name: testRole, Workflow: testWorkflow()})
	if len(snd.OwnerReferences) != 1 {
		t.Fatalf("ownerReferences: want 1, got %d (%+v)", len(snd.OwnerReferences), snd.OwnerReferences)
	}
	if snd.OwnerReferences[0].Kind != "Workflow" {
		t.Fatalf("ownerRef not replaced: %+v", snd.OwnerReferences[0])
	}
}

func TestValidateParams(t *testing.T) {
	full := Params{
		Role:         testRole,
		TemplatePath: "x.yaml.tmpl",
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
	tmplPath := writeTmpl(t, validatorTmpl)
	vars := map[string]string{varKeyChainID: testChainID, varKeyImage: testImage, varKeyAdminAddress: testAdminAddress}

	prestaged, err := renderTemplate(tmplPath, vars)
	if err != nil {
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
		TemplatePath:      tmplPath,
		Vars:              vars,
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
