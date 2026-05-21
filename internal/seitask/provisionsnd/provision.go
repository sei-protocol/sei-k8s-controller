// Package provisionsnd implements `seitask provision-snd`: pick a bundled
// SeiNodeDeployment preset, apply CLI-supplied overrides, stamp an ownerRef
// to the parent Workflow, Create it, await Ready, poll the chain RPC for
// first block, then publish endpoints to workflow-vars under role-scoped
// keys (VALIDATOR_TM_RPC, RPC_EVM_RPC, etc.).
//
// Presets bundle the minimal mode-specific shape (validator vs full-node).
// Per-run shape lives in --override / --genesis-account flags so a scenario
// fully declares its SND shape in the Workflow YAML, no per-scenario asset
// files.
package provisionsnd

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/taskruntime"
)

const fieldOwner client.FieldOwner = "seitask-provision-snd"

//go:embed presets/*.yaml
var presetFS embed.FS

// Params carries the typed inputs to Run.
type Params struct {
	// Role tags the workflow-vars keys this Task writes (e.g. "validator",
	// "rpc"). Required when running multiple provision-snd Tasks in one
	// scenario; values get uppercased to compose VALIDATOR_TM_RPC etc.
	Role string

	// Name is the SeiNodeDeployment metadata.name. Defaults to
	// "<Workflow.Name>-<Role>" when empty.
	Name string

	// Preset selects the bundled base SND template by short name (e.g.
	// "validator", "full-node"). Required.
	Preset string

	// ChainID overrides spec.template.spec.chainId + spec.genesis.chainId.
	// Required.
	ChainID string

	// Image overrides spec.template.spec.image. Required.
	Image string

	// Replicas overrides spec.replicas. 0 means "use the preset's value".
	Replicas int32

	// Overrides are seid config-toml overrides merged into
	// spec.template.spec.overrides on top of whatever the preset carries.
	Overrides map[string]string

	// GenesisAccounts append to spec.genesis.accounts. Rejected when the
	// preset does not embed a genesis block (e.g. full-node).
	GenesisAccounts []seiv1alpha1.GenesisAccount

	// ReadyTimeout bounds the wait for status.phase=Ready.
	ReadyTimeout time.Duration

	// FirstBlockTimeout bounds the post-Ready wait for the chain to produce
	// its first block.
	FirstBlockTimeout time.Duration

	// PollInterval is the interval between status reads (Ready) and chain
	// RPC reads (first block). Same value used for both — tightly tunable
	// from tests.
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

// Run loads the preset, applies overrides, creates the SND with an ownerRef
// to the parent Workflow, waits for Ready, polls the chain RPC for first
// block, and writes role-scoped endpoints to workflow-vars.
func Run(ctx context.Context, c client.Client, p Params) (Result, error) {
	if err := validateParams(p); err != nil {
		return Result{}, err
	}
	p = withDefaults(p)

	snd, err := loadPreset(p.Preset)
	if err != nil {
		// Unknown preset is scenario-config error, not infra-fail.
		return Result{}, taskruntime.Task(fmt.Errorf("loading preset %q: %w", p.Preset, err))
	}
	if err := applyOverrides(snd, p); err != nil {
		return Result{}, err
	}
	stampMetadata(snd, p)

	if err := c.Create(ctx, snd, fieldOwner); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return Result{}, taskruntime.Infra(fmt.Errorf("creating SeiNodeDeployment %s/%s: %w", snd.Namespace, snd.Name, err))
		}
		// Re-runs of provision-snd land here on the existing SND — that's
		// idempotent for our purposes (the controller is reconciling it).
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

	httpClient := p.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if err := waitForFirstBlock(ctx, httpClient, endpoints.TendermintRpc, p.FirstBlockTimeout, p.PollInterval); err != nil {
		return Result{}, err
	}

	if err := publishEndpoints(ctx, c, p.Workflow, p.Role, p.ChainID, endpoints); err != nil {
		return Result{}, err
	}
	return Result{Name: snd.Name, ChainID: p.ChainID, Endpoints: endpoints}, nil
}

func validateParams(p Params) error {
	switch {
	case p.Role == "":
		return fmt.Errorf("provision-snd: --role is required")
	case p.Preset == "":
		return fmt.Errorf("provision-snd: --preset is required")
	case p.ChainID == "":
		return fmt.Errorf("provision-snd: --chain-id is required")
	case p.Image == "":
		return fmt.Errorf("provision-snd: --image is required")
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

func loadPreset(name string) (*seiv1alpha1.SeiNodeDeployment, error) {
	data, err := presetFS.ReadFile("presets/" + name + ".yaml")
	if err != nil {
		return nil, fmt.Errorf("unknown preset (available: %s): %w", availablePresets(), err)
	}
	out := &seiv1alpha1.SeiNodeDeployment{}
	if err := yaml.UnmarshalStrict(data, out); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return out, nil
}

func availablePresets() string {
	entries, _ := presetFS.ReadDir("presets")
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, strings.TrimSuffix(e.Name(), ".yaml"))
	}
	return strings.Join(names, ", ")
}

func applyOverrides(snd *seiv1alpha1.SeiNodeDeployment, p Params) error {
	snd.Spec.Template.Spec.ChainID = p.ChainID
	snd.Spec.Template.Spec.Image = p.Image
	if p.Replicas > 0 {
		snd.Spec.Replicas = p.Replicas
	}
	if snd.Spec.Genesis != nil {
		snd.Spec.Genesis.ChainID = p.ChainID
		snd.Spec.Genesis.Accounts = append(snd.Spec.Genesis.Accounts, p.GenesisAccounts...)
	} else if len(p.GenesisAccounts) > 0 {
		return fmt.Errorf("provision-snd: --genesis-account given but preset %q has no genesis block", p.Preset)
	}
	if len(p.Overrides) > 0 {
		if snd.Spec.Template.Spec.Overrides == nil {
			snd.Spec.Template.Spec.Overrides = map[string]string{}
		}
		maps.Copy(snd.Spec.Template.Spec.Overrides, p.Overrides)
	}
	// Peers that declare a sei.io/chain label selector get the per-run
	// chain-id substituted here. Lets full-node presets join any per-run
	// validator chain without per-run YAML templating.
	for i := range snd.Spec.Template.Spec.Peers {
		label := snd.Spec.Template.Spec.Peers[i].Label
		if label == nil {
			continue
		}
		if _, present := label.Selector[chainSelectorLabel]; present {
			label.Selector[chainSelectorLabel] = p.ChainID
		}
	}
	return nil
}

const chainSelectorLabel = "sei.io/chain"

func stampMetadata(snd *seiv1alpha1.SeiNodeDeployment, p Params) {
	snd.APIVersion = seiv1alpha1.GroupVersion.String()
	snd.Kind = "SeiNodeDeployment"
	snd.Name = p.Name
	snd.Namespace = p.Workflow.Namespace
	snd.OwnerReferences = append(snd.OwnerReferences, p.Workflow.OwnerRef())
}

func waitForReady(ctx context.Context, c client.Client, key types.NamespacedName, timeout, interval time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, interval, timeout, true, func(ctx context.Context) (bool, error) {
		snd := &seiv1alpha1.SeiNodeDeployment{}
		if err := c.Get(ctx, key, snd); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil // newly-created, kube-apiserver hasn't observed our Create yet
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
