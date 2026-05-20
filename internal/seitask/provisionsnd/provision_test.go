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
)

const baseSpecYAML = `
apiVersion: sei.io/v1alpha1
kind: SeiNodeDeployment
spec:
  replicas: 4
  template:
    spec:
      validator: {}
  genesis: {}
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

func writeSpec(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "snd.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func testWorkflow() taskruntime.WorkflowIdentity {
	return taskruntime.WorkflowIdentity{Name: testWorkflowName, UID: "uid-test", Namespace: testNamespace}
}

func TestLoadSpec_AppliesOverridesAndOwnerRef(t *testing.T) {
	specPath := writeSpec(t, baseSpecYAML)

	snd, err := loadSpec(specPath)
	if err != nil {
		t.Fatalf("loadSpec: %v", err)
	}
	if snd.Spec.Replicas != 4 {
		t.Fatalf("base replicas: %d", snd.Spec.Replicas)
	}

	p := Params{
		Role:     testRole,
		Name:     "validator",
		SpecFile: specPath,
		ChainID:  testChainID,
		Image:    testImage,
		Replicas: 7,
		Workflow: testWorkflow(),
	}
	applyOverrides(snd, p)
	stampMetadata(snd, p)

	if snd.Spec.Template.Spec.ChainID != testChainID {
		t.Fatalf("ChainID not propagated: %q", snd.Spec.Template.Spec.ChainID)
	}
	if snd.Spec.Template.Spec.Image != testImage {
		t.Fatalf("Image not propagated: %q", snd.Spec.Template.Spec.Image)
	}
	if snd.Spec.Replicas != 7 {
		t.Fatalf("Replicas override not applied: %d", snd.Spec.Replicas)
	}
	if snd.Spec.Genesis == nil || snd.Spec.Genesis.ChainID != testChainID {
		t.Fatalf("Genesis.ChainID not propagated: %+v", snd.Spec.Genesis)
	}
	if snd.Name != "validator" || snd.Namespace != testNamespace {
		t.Fatalf("metadata wrong: %s/%s", snd.Namespace, snd.Name)
	}
	if len(snd.OwnerReferences) != 1 || snd.OwnerReferences[0].Kind != "Workflow" {
		t.Fatalf("ownerRef not stamped: %+v", snd.OwnerReferences)
	}
}

func TestLoadSpec_RejectsMalformed(t *testing.T) {
	specPath := writeSpec(t, "not: valid: yaml: ::")
	if _, err := loadSpec(specPath); err == nil {
		t.Fatalf("expected unmarshal error")
	}
}

func TestValidateParams(t *testing.T) {
	full := Params{
		Role:     testRole,
		SpecFile: "x.yaml",
		ChainID:  testChainID,
		Image:    testImage,
		Workflow: testWorkflow(),
	}
	cases := []struct {
		name string
		mut  func(*Params)
		want bool // wantErr
	}{
		{"complete", func(*Params) {}, false},
		{"missing role", func(p *Params) { p.Role = "" }, true},
		{"missing spec-file", func(p *Params) { p.SpecFile = "" }, true},
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
	specPath := writeSpec(t, baseSpecYAML)
	w := testWorkflow()

	// Pre-stage a Ready SND with endpoints so waitForReady's polling sees
	// the terminal state on the first tick. Mirrors how the controller
	// would have promoted the CR after a Create.
	prestaged := &seiv1alpha1.SeiNodeDeployment{}
	if err := loadAndDecorate(specPath, &Params{
		Role:     testRole,
		Name:     "validator",
		ChainID:  testChainID,
		Image:    testImage,
		Workflow: w,
	}, prestaged); err != nil {
		t.Fatal(err)
	}
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
	// Rewrite the prestaged SND's TM RPC to the fake server so the
	// first-block poll points at something live.
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "validator"}, prestaged); err != nil {
		t.Fatal(err)
	}
	prestaged.Status.Endpoints.TendermintRpc = srv.URL
	if err := c.Status().Update(context.Background(), prestaged); err != nil {
		t.Fatal(err)
	}

	res, err := Run(context.Background(), c, Params{
		Role:              testRole,
		Name:              "validator",
		SpecFile:          specPath,
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
	if res.Name != "validator" || res.ChainID != testChainID {
		t.Fatalf("Result: %+v", res)
	}

	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testWorkflowVarsCM}, cm); err != nil {
		t.Fatalf("get CM: %v", err)
	}
	if got := cm.Data["VALIDATOR_CHAIN_ID"]; got != testChainID {
		t.Fatalf("VALIDATOR_CHAIN_ID = %q", got)
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

// loadAndDecorate is the test-side mirror of the production loadSpec +
// applyOverrides + stampMetadata pipeline. Used to materialize the
// pre-staged SND the fake client serves.
func loadAndDecorate(specPath string, p *Params, into *seiv1alpha1.SeiNodeDeployment) error {
	parsed, err := loadSpec(specPath)
	if err != nil {
		return err
	}
	applyOverrides(parsed, *p)
	stampMetadata(parsed, *p)
	*into = *parsed
	return nil
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
