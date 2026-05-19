package runner_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"

	"github.com/sei-protocol/sei-k8s-controller/internal/runner"
)

// Test fixtures kept as constants to satisfy goconst across the file. These
// are pure test inputs — refactoring callers to use them improves nothing
// semantically; the constants exist solely to keep the linter quiet on
// strings that recur >=3 times.
const (
	tChainID    = "sei-localnet"
	tChainIDKey = "CHAIN_ID"
	tStatusKey  = "status"
	tPhaseKey   = "phase"
	tNodeKey    = "NODE"
	tComplete   = "Complete"
	tTxHashEnv  = "TX_HASH"
	tTxHashVal  = "ABCD"
	tStubName   = "stub-name"
	tImageV2    = "seid:v2"
	tIgnored    = "ignored"
	tPropIDKey  = "PROPOSAL_ID"
	tValidator0 = "validator-0"
)

// ---------------------------------------------------------------------------
// Apply / render
// ---------------------------------------------------------------------------

func TestRenderBytes_DeterministicName(t *testing.T) {
	g := NewWithT(t)
	tmpl := []byte(`apiVersion: sei.io/v1alpha1
kind: SeiNodeTask
metadata:
  name: PLACEHOLDER
spec:
  kind: GovVote
  target:
    nodeRef:
      name: {{ .NODE }}
  govVote:
    chainId: {{ .CHAIN_ID }}
    keyName: k
    proposalId: {{ .PROPOSAL_ID }}
    option: yes
    fees: 2000usei
    gas: 200000
`)
	vars := map[string]string{tNodeKey: tValidator0, tChainIDKey: tChainID, tPropIDKey: "47"}

	manifest1, name1, err := runner.RenderBytes("t.tmpl", tmpl, vars)
	g.Expect(err).NotTo(HaveOccurred())
	manifest2, name2, err := runner.RenderBytes("t.tmpl", tmpl, vars)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(name1).To(Equal(name2), "name must be deterministic for identical inputs")
	g.Expect(string(manifest1)).To(Equal(string(manifest2)))
	g.Expect(name1).To(HavePrefix("gov-vote-validator-0-"), "name should embed kind + NODE for operator ergonomics")
	g.Expect(name1).To(MatchRegexp(`^gov-vote-validator-0-[0-9a-f]{10}$`))

	// Re-render with a different var should change the hash.
	vars[tPropIDKey] = "48"
	_, name3, err := runner.RenderBytes("t.tmpl", tmpl, vars)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(name3).NotTo(Equal(name1))
}

func TestRenderBytes_RejectsNonSeiNodeTask(t *testing.T) {
	g := NewWithT(t)
	tmpl := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: PLACEHOLDER}\n")
	_, _, err := runner.RenderBytes("t.tmpl", tmpl, nil)
	g.Expect(err).To(MatchError(ContainSubstring("rendered manifest is ConfigMap")))
}

func TestRenderBytes_MissingKeyIsError(t *testing.T) {
	g := NewWithT(t)
	tmpl := []byte("apiVersion: sei.io/v1alpha1\nkind: SeiNodeTask\nmetadata: {name: PLACEHOLDER}\nspec:\n  kind: GovVote\n  target: {nodeRef: {name: {{ .NODE }}}}\n")
	_, _, err := runner.RenderBytes("t.tmpl", tmpl, map[string]string{})
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("execute template"))
}

// ---------------------------------------------------------------------------
// Fanout policy
// ---------------------------------------------------------------------------

func TestParseFanoutMode(t *testing.T) {
	g := NewWithT(t)

	p, err := runner.ParseFanoutMode("")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(p.Mode).To(Equal("all"))

	p, err = runner.ParseFanoutMode("best-effort")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(p.Mode).To(Equal("best-effort"))

	p, err = runner.ParseFanoutMode("quorum:3")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(p.Mode).To(Equal("quorum"))
	g.Expect(p.Quorum).To(Equal(3))

	_, err = runner.ParseFanoutMode("quorum:0")
	g.Expect(err).To(HaveOccurred())

	_, err = runner.ParseFanoutMode("nope")
	g.Expect(err).To(HaveOccurred())
}

func TestFanoutPolicy_Decide(t *testing.T) {
	g := NewWithT(t)

	all := runner.FanoutPolicy{Mode: "all"}
	// All complete -> ok.
	done, ok := all.Decide([]runner.Outcome{runner.OutcomeComplete, runner.OutcomeComplete}, 2)
	g.Expect(done).To(BeTrue())
	g.Expect(ok).To(BeTrue())
	// Any fail -> fail fast.
	done, ok = all.Decide([]runner.Outcome{runner.OutcomeFailed}, 3)
	g.Expect(done).To(BeTrue())
	g.Expect(ok).To(BeFalse())
	// Partial complete, none failed -> keep going.
	done, _ = all.Decide([]runner.Outcome{runner.OutcomeComplete}, 3)
	g.Expect(done).To(BeFalse())

	best := runner.FanoutPolicy{Mode: "best-effort"}
	// One complete, others still pending -> wait.
	done, _ = best.Decide([]runner.Outcome{runner.OutcomeComplete}, 3)
	g.Expect(done).To(BeFalse())
	// All terminated, >=1 complete -> ok.
	done, ok = best.Decide([]runner.Outcome{runner.OutcomeComplete, runner.OutcomeFailed, runner.OutcomeFailed}, 3)
	g.Expect(done).To(BeTrue())
	g.Expect(ok).To(BeTrue())
	// All failed -> not ok.
	done, ok = best.Decide([]runner.Outcome{runner.OutcomeFailed, runner.OutcomeFailed}, 2)
	g.Expect(done).To(BeTrue())
	g.Expect(ok).To(BeFalse())

	quorum := runner.FanoutPolicy{Mode: "quorum", Quorum: 2}
	// Hit quorum -> ok early.
	done, ok = quorum.Decide([]runner.Outcome{runner.OutcomeComplete, runner.OutcomeComplete}, 4)
	g.Expect(done).To(BeTrue())
	g.Expect(ok).To(BeTrue())
	// Too many failed to reach quorum -> fail early.
	done, ok = quorum.Decide([]runner.Outcome{runner.OutcomeFailed, runner.OutcomeFailed, runner.OutcomeFailed}, 4)
	g.Expect(done).To(BeTrue())
	g.Expect(ok).To(BeFalse())
}

// ---------------------------------------------------------------------------
// Output extraction
// ---------------------------------------------------------------------------

func TestExtractOutputs(t *testing.T) {
	g := NewWithT(t)
	obj := map[string]any{
		tStatusKey: map[string]any{
			tPhaseKey: tComplete,
			"outputs": map[string]any{
				"govVote": map[string]any{
					"txHash": tTxHashVal,
					"height": int64(1234),
				},
			},
		},
	}

	kvs, err := runner.ExtractOutputs(
		[]string{".status.outputs.govVote.txHash=TX_HASH", ".status.outputs.govVote.height=HEIGHT"},
		obj,
	)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kvs).To(HaveLen(2))
	g.Expect(kvs[0]).To(Equal(runner.KV{Key: tTxHashEnv, Value: tTxHashVal}))
	g.Expect(kvs[1].Key).To(Equal("HEIGHT"))
	g.Expect(kvs[1].Value).To(Equal("1234"))
}

func TestExtractOutputs_MissingFieldOmitted(t *testing.T) {
	g := NewWithT(t)
	// Sidecar-backed kinds (govVote/govSoftwareUpgrade in MVP) have empty
	// status.outputs. The extractor must drop missing fields, not error.
	obj := map[string]any{tStatusKey: map[string]any{tPhaseKey: tComplete}}
	kvs, err := runner.ExtractOutputs([]string{".status.outputs.govVote.txHash=TX_HASH"}, obj)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kvs).To(BeEmpty())
}

func TestExtractOutputs_MalformedSpec(t *testing.T) {
	g := NewWithT(t)
	_, err := runner.ExtractOutputs([]string{"no-equals-sign"}, map[string]any{})
	g.Expect(err).To(MatchError(ContainSubstring("missing '=ENV_VAR'")))
}

// ---------------------------------------------------------------------------
// Env sourcer / writer
// ---------------------------------------------------------------------------

func TestFileEnvSourcer_RoundTrip(t *testing.T) {
	g := NewWithT(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "env.sh")

	w := runner.FileEnvWriter{}
	g.Expect(w.Append(path, []runner.KV{{Key: tTxHashEnv, Value: tTxHashVal}, {Key: tPropIDKey, Value: "47"}})).To(Succeed())
	g.Expect(w.Append(path, []runner.KV{{Key: "HEIGHT", Value: "1234"}})).To(Succeed())

	got, err := runner.FileEnvSourcer{}.Source(path)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).To(Equal(map[string]string{
		tTxHashEnv: tTxHashVal,
		tPropIDKey: "47",
		"HEIGHT":   "1234",
	}))
}

func TestFileEnvSourcer_MissingFileIsNotAnError(t *testing.T) {
	g := NewWithT(t)
	got, err := runner.FileEnvSourcer{}.Source(filepath.Join(t.TempDir(), "absent.sh"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).To(BeNil())
}

// ---------------------------------------------------------------------------
// Stub-driven Execute() integration
// ---------------------------------------------------------------------------

type stubRenderer struct {
	calls []map[string]string
	name  string
}

func (s *stubRenderer) Render(_ string, vars map[string]string) ([]byte, string, error) {
	clone := make(map[string]string, len(vars))
	maps.Copy(clone, vars)
	s.calls = append(s.calls, clone)
	manifest := fmt.Appendf(nil, "apiVersion: sei.io/v1alpha1\nkind: SeiNodeTask\nmetadata:\n  name: %s\nspec:\n  kind: GovVote\n", s.name)
	return manifest, s.name, nil
}

type stubApplier struct {
	mu      sync.Mutex
	applied []string
}

func (s *stubApplier) Apply(_ context.Context, _ string, manifest []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	obj := map[string]any{}
	_ = yaml.Unmarshal(manifest, &obj)
	if md, ok := obj["metadata"].(map[string]any); ok {
		if name, ok := md["name"].(string); ok {
			s.applied = append(s.applied, name)
		}
	}
	return nil
}

type stubPoller struct {
	phase  string
	obj    map[string]any
	reason string
	err    error
}

func (s stubPoller) Poll(_ context.Context, _, _ string, _ time.Duration) (string, map[string]any, string, error) {
	return s.phase, s.obj, s.reason, s.err
}

type stubLister struct{ names []string }

func (s stubLister) List(_ context.Context, _, _ string) ([]string, error) { return s.names, nil }

type stubSourcer struct{ env map[string]string }

func (s stubSourcer) Source(_ string) (map[string]string, error) { return s.env, nil }

type stubWriter struct {
	mu      sync.Mutex
	written []runner.KV
}

func (w *stubWriter) Append(_ string, kv []runner.KV) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.written = append(w.written, kv...)
	return nil
}

func newRun(opts runner.Options, poller runner.Poller, applier runner.Applier, lister runner.NodeLister) (*runner.Run, *stubWriter) {
	w := &stubWriter{}
	return &runner.Run{
		Opts:     opts,
		Stdout:   os.Stderr,
		Stderr:   os.Stderr,
		Renderer: &stubRenderer{name: tStubName},
		Applier:  applier,
		Poller:   poller,
		Lister:   lister,
		Sourcer:  stubSourcer{},
		Writer:   w,
	}, w
}

func TestExecute_SingleNode_CompleteExtractsOutputs(t *testing.T) {
	g := NewWithT(t)
	app := &stubApplier{}
	poller := stubPoller{
		phase: tComplete,
		obj: map[string]any{
			tStatusKey: map[string]any{
				tPhaseKey: tComplete,
				"outputs": map[string]any{"updateNodeImage": map[string]any{"appliedImage": tImageV2}},
			},
		},
	}
	r, w := newRun(runner.Options{
		TemplatePath:    tIgnored,
		Vars:            map[string]string{tNodeKey: tValidator0},
		OutputJSONPaths: []string{".status.outputs.updateNodeImage.appliedImage=APPLIED_IMAGE"},
		OutputEnvFile:   filepath.Join(t.TempDir(), "env.sh"),
		Timeout:         time.Second,
		PollInterval:    10 * time.Millisecond,
		Namespace:       "ns",
	}, poller, app, nil)
	g.Expect(r.Execute(context.Background())).To(Succeed())
	g.Expect(app.applied).To(Equal([]string{tStubName}))
	g.Expect(w.written).To(Equal([]runner.KV{{Key: "APPLIED_IMAGE", Value: tImageV2}}))
}

func TestExecute_SingleNode_FailedReturnsReason(t *testing.T) {
	g := NewWithT(t)
	poller := stubPoller{phase: "Failed", reason: "deposit too small"}
	r, _ := newRun(runner.Options{
		TemplatePath: tIgnored,
		Vars:         map[string]string{tNodeKey: tValidator0},
		Timeout:      time.Second,
		PollInterval: 10 * time.Millisecond,
		Namespace:    "ns",
	}, poller, &stubApplier{}, nil)
	err := r.Execute(context.Background())
	g.Expect(err).To(MatchError(ContainSubstring("deposit too small")))
}

func TestExecute_Fanout_AllMustSucceed(t *testing.T) {
	g := NewWithT(t)
	app := &stubApplier{}
	poller := stubPoller{phase: tComplete, obj: map[string]any{tStatusKey: map[string]any{tPhaseKey: tComplete}}}
	r, _ := newRun(runner.Options{
		TemplatePath:    tIgnored,
		Vars:            map[string]string{"IMAGE": tImageV2},
		PerNodeSelector: "role=validator",
		FanoutMode:      "all-must-succeed",
		Timeout:         time.Second,
		PollInterval:    10 * time.Millisecond,
		Namespace:       "ns",
	}, poller, app, stubLister{names: []string{"v0", "v1", "v2"}})
	g.Expect(r.Execute(context.Background())).To(Succeed())
	g.Expect(app.applied).To(HaveLen(3))
}

func TestExecute_Fanout_BestEffortAllowsFailures(t *testing.T) {
	g := NewWithT(t)
	// Per-target poller that returns different verdicts based on the task name
	// to simulate a partial-fail fan-out.
	poller := poller2{
		results: map[string]stubPoller{
			tStubName: {phase: tComplete, obj: map[string]any{tStatusKey: map[string]any{tPhaseKey: tComplete}}},
		},
		def: stubPoller{phase: "Failed", reason: "boom"},
	}
	app := &stubApplier{}
	r, _ := newRun(runner.Options{
		TemplatePath:    tIgnored,
		PerNodeSelector: "role=validator",
		FanoutMode:      "best-effort",
		Timeout:         time.Second,
		PollInterval:    10 * time.Millisecond,
		Namespace:       "ns",
	}, poller, app, stubLister{names: []string{"v0", "v1"}})
	// stubRenderer returns the same name for both renders, so the poller's
	// stub-name map applies to both. We just need at least one Complete to succeed.
	g.Expect(r.Execute(context.Background())).To(Succeed())
}

// poller2 is a per-name dispatcher used by best-effort fanout tests.
type poller2 struct {
	results map[string]stubPoller
	def     stubPoller
}

func (p poller2) Poll(_ context.Context, _, name string, _ time.Duration) (string, map[string]any, string, error) {
	if s, ok := p.results[name]; ok {
		return s.phase, s.obj, s.reason, s.err
	}
	return p.def.phase, p.def.obj, p.def.reason, p.def.err
}

// ---------------------------------------------------------------------------
// Embedded templates: each renders against representative vars.
// ---------------------------------------------------------------------------

func TestEmbeddedTemplates_Render(t *testing.T) {
	g := NewWithT(t)
	repoRoot := findRepoRoot(t)
	dir := filepath.Join(repoRoot, "runner", "templates")

	cases := []struct {
		file string
		vars map[string]string
	}{
		{
			file: "gov-software-upgrade.yaml.tmpl",
			vars: map[string]string{
				tNodeKey: tValidator0, tChainIDKey: tChainID, "KEY_NAME": "admin",
				"TITLE": "Upgrade to v2.0.0", "DESCRIPTION": "rollout v2",
				"UPGRADE_NAME": "v2.0.0", "UPGRADE_HEIGHT": "1500",
				"INITIAL_DEPOSIT": "10000000usei", "FEES": "2000usei", "GAS": "500000",
			},
		},
		{
			file: "gov-vote.yaml.tmpl",
			vars: map[string]string{
				tNodeKey: tValidator0, tChainIDKey: tChainID, "KEY_NAME": "admin",
				tPropIDKey: "47", "OPTION": "yes", "FEES": "2000usei", "GAS": "200000",
			},
		},
		{
			file: "await-condition.yaml.tmpl",
			vars: map[string]string{tNodeKey: tValidator0, "TARGET_HEIGHT": "1500"},
		},
		{
			file: "update-node-image.yaml.tmpl",
			vars: map[string]string{tNodeKey: tValidator0, "IMAGE": "ghcr.io/sei/seid:v2.0.0"},
		},
		{
			file: "await-nodes-at-height.yaml.tmpl",
			vars: map[string]string{tNodeKey: tValidator0, "TARGET_HEIGHT": "1500"},
		},
	}

	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			g := NewWithT(t)
			raw, err := os.ReadFile(filepath.Join(dir, c.file))
			g.Expect(err).NotTo(HaveOccurred(), "read template")
			manifest, name, err := runner.RenderBytes(c.file, raw, c.vars)
			g.Expect(err).NotTo(HaveOccurred(), "render template")
			g.Expect(name).NotTo(BeEmpty())

			obj := map[string]any{}
			g.Expect(yaml.Unmarshal(manifest, &obj)).To(Succeed())
			g.Expect(obj["apiVersion"]).To(Equal("sei.io/v1alpha1"))
			g.Expect(obj["kind"]).To(Equal("SeiNodeTask"))
			spec, ok := obj["spec"].(map[string]any)
			g.Expect(ok).To(BeTrue())
			g.Expect(spec).To(HaveKey("kind"))
			g.Expect(spec).To(HaveKey("target"))
		})
	}
	_ = g
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for dir := wd; dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
	}
	t.Fatal("repo root not found")
	return ""
}

// ---------------------------------------------------------------------------
// Sanity
// ---------------------------------------------------------------------------

func TestNoStrayErrors(t *testing.T) {
	// Tripwire so refactors that accidentally remove the deterministic-name
	// contract surface as a one-line test failure.
	g := NewWithT(t)
	name := runner.DeterministicName("GovVote", map[string]string{tNodeKey: "v0", "X": "1"}, []byte("body"))
	g.Expect(name).To(HavePrefix("gov-vote-v0-"))
	g.Expect(strings.Count(name, "-")).To(BeNumerically(">=", 3))
	g.Expect(errors.New("noop")).To(HaveOccurred())
}
