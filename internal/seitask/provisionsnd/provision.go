// Package provisionsnd implements `seitask provision-snd`: render a Go
// template to a SeiNodeDeployment YAML, stamp an ownerRef to the parent
// Workflow, Create it, await Ready, poll the chain RPC for first block,
// then publish endpoints to workflow-vars under role-scoped keys
// (VALIDATOR_TM_RPC, RPC_EVM_RPC, etc.).
//
// Templates are scenario-intrinsic: the full SND shape (mode, overrides,
// peers, genesis ceremony) lives in the template body as proper YAML.
// Per-run scalars (CHAIN_ID, IMAGE, ADMIN_ADDRESS, ...) flow in via --var
// and resolve at render time. Same `--template + --var` contract as the
// runner subcommand.
package provisionsnd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"text/template"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/taskruntime"
)

const fieldOwner client.FieldOwner = "seitask-provision-snd"

// Params carries the typed inputs to Run.
type Params struct {
	// Role tags the workflow-vars keys this Task writes (e.g. "validator",
	// "rpc"). Required for scenarios with multiple provision-snd Tasks;
	// values get uppercased to compose VALIDATOR_TM_RPC etc.
	Role string

	// Name is the SeiNodeDeployment metadata.name. Defaults to
	// "<Workflow.Name>-<Role>" when empty.
	Name string

	// TemplatePath is the on-disk path to the Go text/template producing
	// a SeiNodeDeployment YAML. Required.
	TemplatePath string

	// Vars are the template's substitution context (the .KEY map in
	// template syntax). Missing keys referenced by the template fail
	// rendering rather than silently expanding to empty strings.
	Vars map[string]string

	// ReadyTimeout bounds the wait for status.phase=Ready.
	ReadyTimeout time.Duration

	// FirstBlockTimeout bounds the post-Ready wait for the chain to produce
	// its first block.
	FirstBlockTimeout time.Duration

	// PollInterval is the interval between status reads and chain RPC reads.
	PollInterval time.Duration

	// HTTPClient overrides the chain-RPC client; nil means http.DefaultClient.
	// Tests use this seam.
	HTTPClient *http.Client

	// Workflow is the parent Chaos Mesh Workflow identity (downward-API).
	Workflow taskruntime.WorkflowIdentity
}

// Result is the post-Run summary, returned so main can log it before exit.
type Result struct {
	Name      string
	ChainID   string
	Endpoints seiv1alpha1.Endpoints
}

// Run renders the template, creates the SND with an ownerRef to the parent
// Workflow, waits for Ready, polls the chain RPC for first block, and
// writes role-scoped endpoints to workflow-vars.
func Run(ctx context.Context, c client.Client, p Params) (Result, error) {
	if err := validateParams(p); err != nil {
		return Result{}, err
	}
	p = withDefaults(p)

	snd, err := renderTemplate(p.TemplatePath, p.Vars)
	if err != nil {
		return Result{}, taskruntime.Task(fmt.Errorf("rendering template %s: %w", p.TemplatePath, err))
	}
	stampMetadata(snd, p)

	if err := c.Create(ctx, snd, fieldOwner); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return Result{}, taskruntime.Infra(fmt.Errorf("creating SeiNodeDeployment %s/%s: %w", snd.Namespace, snd.Name, err))
		}
		// Re-runs land here. Surface drift loudly so an operator who edited
		// the template since the original Create knows the cluster is still
		// at the original spec — we don't force-apply to avoid clobbering
		// hand-edits or in-flight reconciliation.
		warnIfDrift(ctx, c, snd)
	}

	if err := waitForReady(ctx, c, types.NamespacedName{Namespace: snd.Namespace, Name: snd.Name}, p.ReadyTimeout, p.PollInterval); err != nil {
		return Result{}, err
	}

	current := &seiv1alpha1.SeiNodeDeployment{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: snd.Namespace, Name: snd.Name}, current); err != nil {
		return Result{}, taskruntime.Infra(fmt.Errorf("re-reading SND post-Ready: %w", err))
	}
	if current.Status.Endpoints == nil || current.Status.Endpoints.TendermintRpc == "" {
		return Result{}, taskruntime.Infra(fmt.Errorf("SND %s reached Ready but .status.endpoints.tendermintRpc is empty", current.Name))
	}
	endpoints := *current.Status.Endpoints
	chainID := current.Spec.Template.Spec.ChainID

	httpClient := p.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if err := waitForFirstBlock(ctx, httpClient, endpoints.TendermintRpc, p.FirstBlockTimeout, p.PollInterval); err != nil {
		return Result{}, err
	}

	if err := publishEndpoints(ctx, c, p.Workflow, p.Role, chainID, endpoints); err != nil {
		return Result{}, err
	}
	return Result{Name: snd.Name, ChainID: chainID, Endpoints: endpoints}, nil
}

func validateParams(p Params) error {
	switch {
	case p.Role == "":
		return fmt.Errorf("provision-snd: --role is required")
	case p.TemplatePath == "":
		return fmt.Errorf("provision-snd: --template is required")
	case p.Workflow.Name == "" || p.Workflow.Namespace == "":
		return fmt.Errorf("provision-snd: workflow identity not loaded")
	}
	return nil
}

func withDefaults(p Params) Params {
	if p.Name == "" {
		p.Name = p.Workflow.Name + "-" + p.Role
	}
	if p.ReadyTimeout == 0 {
		p.ReadyTimeout = 15 * time.Minute
	}
	if p.FirstBlockTimeout == 0 {
		p.FirstBlockTimeout = 5 * time.Minute
	}
	if p.PollInterval == 0 {
		p.PollInterval = 5 * time.Second
	}
	return p
}

// renderTemplate parses the file at path as a Go text/template, executes it
// against vars (missing keys fail the render — `missingkey=error` option),
// then strict-unmarshals the rendered bytes into a SeiNodeDeployment so
// typos in field names fail here, not at apiserver-Create time.
func renderTemplate(path string, vars map[string]string) (*seiv1alpha1.SeiNodeDeployment, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	tmpl, err := template.New(path).Option("missingkey=error").Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		return nil, fmt.Errorf("execute: %w", err)
	}
	out := &seiv1alpha1.SeiNodeDeployment{}
	if err := yaml.UnmarshalStrict(buf.Bytes(), out); err != nil {
		return nil, fmt.Errorf("unmarshal rendered yaml: %w", err)
	}
	return out, nil
}

// stampMetadata overwrites metadata fields the template MUST NOT control.
// OwnerReferences are assigned (not appended) so a template that smuggles
// a bogus ref can't leak through.
func stampMetadata(snd *seiv1alpha1.SeiNodeDeployment, p Params) {
	snd.APIVersion = seiv1alpha1.GroupVersion.String()
	snd.Kind = "SeiNodeDeployment"
	snd.Name = p.Name
	snd.Namespace = p.Workflow.Namespace
	snd.OwnerReferences = []metav1.OwnerReference{p.Workflow.OwnerRef()}
}

// warnIfDrift logs when a re-run finds the on-cluster SND.Spec different
// from the freshly-rendered one. Operators who edited the template since
// the original Create need to know the cluster still has the old spec.
func warnIfDrift(ctx context.Context, c client.Client, fresh *seiv1alpha1.SeiNodeDeployment) {
	existing := &seiv1alpha1.SeiNodeDeployment{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: fresh.Namespace, Name: fresh.Name}, existing); err != nil {
		return
	}
	if reflect.DeepEqual(existing.Spec, fresh.Spec) {
		return
	}
	fmt.Fprintf(os.Stderr, "WARN: SND %s/%s exists with spec different from rendered template; reusing on-cluster spec\n", fresh.Namespace, fresh.Name)
}

func waitForReady(ctx context.Context, c client.Client, key types.NamespacedName, timeout, interval time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, interval, timeout, true, func(ctx context.Context) (bool, error) {
		snd := &seiv1alpha1.SeiNodeDeployment{}
		if err := c.Get(ctx, key, snd); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, taskruntime.Infra(fmt.Errorf("reading SND %s: %w", key, err))
		}
		switch snd.Status.Phase {
		case seiv1alpha1.GroupPhaseReady:
			return true, nil
		case seiv1alpha1.GroupPhaseFailed:
			return false, taskruntime.Task(fmt.Errorf("SND %s reached Failed phase", key))
		}
		return false, nil
	})
}

// tendermintStatusResponse models the subset of Tendermint /status we need.
// Sei's CometBFT fork sometimes returns the body unwrapped (no JSON-RPC
// envelope), so we accept both shapes and fall back via Result/SyncInfo.
type tendermintStatusResponse struct {
	Result *struct {
		SyncInfo struct {
			LatestBlockHeight string `json:"latest_block_height"`
		} `json:"sync_info"`
	} `json:"result,omitempty"`
	SyncInfo struct {
		LatestBlockHeight string `json:"latest_block_height"`
	} `json:"sync_info"`
}

func (r *tendermintStatusResponse) latestHeight() string {
	if r.Result != nil && r.Result.SyncInfo.LatestBlockHeight != "" {
		return r.Result.SyncInfo.LatestBlockHeight
	}
	return r.SyncInfo.LatestBlockHeight
}

func waitForFirstBlock(ctx context.Context, hc *http.Client, tmRPC string, timeout, interval time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, interval, timeout, true, func(ctx context.Context) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, tmRPC+"/status", nil)
		if err != nil {
			return false, taskruntime.Infra(fmt.Errorf("status req: %w", err))
		}
		resp, err := hc.Do(req)
		if err != nil {
			return false, nil
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return false, nil
		}
		var parsed tendermintStatusResponse
		if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
			return false, nil
		}
		h := parsed.latestHeight()
		if h == "" || h == "0" {
			return false, nil
		}
		return true, nil
	})
}

// publishEndpoints assumes one chain-id per Workflow. CHAIN_ID is seeded on
// first create and silently retained on AlreadyExists; a scenario that
// provisions two distinct chains needs an explicit conflict check here.
func publishEndpoints(ctx context.Context, c client.Client, w taskruntime.WorkflowIdentity, role, chainID string, ep seiv1alpha1.Endpoints) error {
	if err := taskruntime.EnsureWorkflowVarsCM(ctx, c, w, map[taskruntime.VarKey]string{
		taskruntime.KeyRunID:   w.Name,
		taskruntime.KeyChainID: chainID,
	}); err != nil {
		return err
	}
	vars := map[taskruntime.VarKey]string{
		taskruntime.RoleScoped(role, taskruntime.KeyTendermintRPC):  ep.TendermintRpc,
		taskruntime.RoleScoped(role, taskruntime.KeyTendermintREST): ep.TendermintRest,
	}
	if len(ep.Nodes) > 0 {
		vars[taskruntime.RoleScoped(role, taskruntime.KeyEVMJSONRPC)] = ep.Nodes[0].EvmJsonRpc
	}
	return taskruntime.SetVars(ctx, c, w, vars)
}
