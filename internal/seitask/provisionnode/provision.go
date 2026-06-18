// Package provisionnode implements `seitask provision-node`: fan out N
// standalone SeiNode follower CRs from one Go template, stamp an ownerRef
// to the parent Workflow, Create them, await PhaseRunning, run a two-stage
// per-node readiness probe (Tendermint /status height>0, then EVM
// eth_blockNumber 200), then publish role-scoped endpoints to workflow-vars
// (<ROLE>_EVM_RPC_LIST, <ROLE>_EVM_RPC, <ROLE>_TM_RPC, <ROLE>_REST, CHAIN_ID).
//
// Unlike provision-snd (genesis SeiNetwork, waits Ready, reads the fleet
// aggregate), provision-node provisions followers that join an existing chain.
// It assembles every workflow-vars key from the N per-node .status.endpoint
// scalars because a standalone SeiNode has no fleet ClusterIP to aggregate.
//
// The N CRs are named <base>-0..<base>-(N-1); the controller stamps
// sei.io/node=<CR-name> on each pod, preserving the chaos suite's pod
// selectors. provision-node also stamps sei.io/role=node (always) and
// sei.io/seinetwork=<network> (when --network is set) on each CR's
// metadata.labels — the shared object-label producer contract with
// `seictl node apply`, which the follower-discovery query
// (node list -l sei.io/seinetwork=<net>,sei.io/role=node) matches on.
package provisionnode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
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

const fieldOwner client.FieldOwner = "seitask-provision-node"

// Object-label producer contract (§2.2a) — MUST stay byte-identical to
// `seictl node apply`. The keys/values mirror the controller's canonical
// constants (noderesource: sei.io/role / "node"; seinetwork: sei.io/seinetwork),
// which are unexported, so we re-declare them. The contract test pins these
// literals so an accidental edit here fails; the controller side is independently
// pinned by noderesource_test.go.
const (
	labelRole       = "sei.io/role"
	roleValueNode   = "node"
	labelSeiNetwork = "sei.io/seinetwork"
)

// Params carries the typed inputs to Run.
type Params struct {
	// Role tags the workflow-vars keys this Task writes (e.g. "rpc").
	// Uppercased to compose RPC_EVM_RPC_LIST etc. Required.
	Role string

	// Name is the BASE name; the N followers are <Name>-0..<Name>-(N-1).
	// Defaults to "<ChainID>-<Role>" (or "<Workflow.Name>-<Role>" when no
	// CHAIN_ID var) so chaos sei.io/node selectors stay valid.
	Name string

	// TemplatePath is the on-disk path to the Go text/template producing
	// ONE kind: SeiNode YAML. Rendered once per replica with .ORDINAL and
	// .NODE_NAME injected. Required.
	TemplatePath string

	// Vars are the template's substitution context (the .KEY map). Missing
	// keys referenced by the template fail rendering. The runtime injects
	// .ORDINAL and .NODE_NAME per replica; a --var collision on either is
	// rejected (mirrors the runner's --var NODE= guard).
	Vars map[string]string

	// Replicas is N: the number of follower SeiNode CRs to fan out. >=1.
	Replicas int

	// Network is the genesis SeiNetwork to follow. When set, the runtime
	// (a) synthesizes a LabelPeerSource selecting sei.io/seinetwork=<Network>
	// and (b) stamps the sei.io/seinetwork=<Network> object label.
	Network string

	// NetworkNamespace is the namespace of the genesis SeiNetwork for the
	// synthesized peer selector. Defaults to the Workflow namespace.
	NetworkNamespace string

	// RunningTimeout bounds the wait for all N SeiNodes to reach PhaseRunning.
	RunningTimeout time.Duration

	// FirstBlockTimeout bounds the post-Running readiness probe (both the TM
	// height>0 stage and the EVM eth_blockNumber stage), per node.
	FirstBlockTimeout time.Duration

	// PollInterval is the interval between status reads and RPC reads.
	PollInterval time.Duration

	// HTTPClient overrides the RPC client; nil means http.DefaultClient.
	HTTPClient *http.Client

	// Workflow is the parent Chaos Mesh Workflow identity (downward-API).
	Workflow taskruntime.WorkflowIdentity
}

// Result is the post-Run summary, returned so main can log it before exit.
type Result struct {
	// Names are the N created SeiNode names, ordinal-ordered.
	Names []string
	// ChainID is the resolved chain ID published as CHAIN_ID.
	ChainID string
	// EVMRPCList is the assembled <ROLE>_EVM_RPC_LIST CSV.
	EVMRPCList string
}

// Run renders the template N times, creates N SeiNode followers with an
// ownerRef to the parent Workflow, waits for all to reach PhaseRunning, runs
// the per-node two-stage readiness probe, then publishes role-scoped endpoints.
func Run(ctx context.Context, c client.Client, p Params) (Result, error) {
	if err := validateParams(p); err != nil {
		return Result{}, err
	}
	p = withDefaults(p)

	names := make([]string, 0, p.Replicas)
	for ordinal := 0; ordinal < p.Replicas; ordinal++ {
		node, err := renderNode(p, ordinal)
		if err != nil {
			return Result{}, taskruntime.Task(fmt.Errorf("rendering template %s (ordinal %d): %w", p.TemplatePath, ordinal, err))
		}
		stampMetadata(node, p, ordinal)

		if err := c.Create(ctx, node, fieldOwner); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return Result{}, taskruntime.Infra(fmt.Errorf("creating SeiNode %s/%s: %w", node.Namespace, node.Name, err))
			}
			// Re-runs land here. Surface drift loudly so an operator who
			// edited the template since the original Create knows the cluster
			// is still at the original spec — we don't force-apply.
			warnIfDrift(ctx, c, node)
		}
		names = append(names, node.Name)
	}

	// Wait for all N to reach Running under one shared deadline.
	if err := waitForRunning(ctx, c, p.Workflow.Namespace, names, p.RunningTimeout, p.PollInterval); err != nil {
		return Result{}, err
	}

	// Re-read each node post-Running for its .status.endpoint, then run the
	// two-stage readiness probe before publishing.
	nodes := make([]*seiv1alpha1.SeiNode, 0, len(names))
	httpClient := p.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	for _, name := range names {
		node := &seiv1alpha1.SeiNode{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: p.Workflow.Namespace, Name: name}, node); err != nil {
			return Result{}, taskruntime.Infra(fmt.Errorf("re-reading SeiNode %s post-Running: %w", name, err))
		}
		ep := node.Status.Endpoint
		if ep == nil || ep.TendermintRpc == "" {
			return Result{}, taskruntime.Infra(fmt.Errorf("SeiNode %s Running but .status.endpoint.tendermintRpc empty", name))
		}
		// Stage 2 — TM liveness: the node has joined the chain and is syncing.
		if err := waitForFirstBlock(ctx, httpClient, ep.TendermintRpc, p.FirstBlockTimeout, p.PollInterval); err != nil {
			return Result{}, err
		}
		// Stage 3 — EVM liveness: the JSON-RPC listener is bound before its
		// URL enters RPC_EVM_RPC_LIST. height>0 on TM does NOT prove this.
		if ep.EvmJsonRpc == "" {
			return Result{}, taskruntime.Infra(fmt.Errorf("SeiNode %s Running but .status.endpoint.evmJsonRpc empty", name))
		}
		if err := waitForEVMReady(ctx, httpClient, ep.EvmJsonRpc, p.FirstBlockTimeout, p.PollInterval); err != nil {
			return Result{}, err
		}
		nodes = append(nodes, node)
	}

	chainID := p.Vars[string(taskruntime.KeyChainID)]
	if chainID == "" && len(nodes) > 0 {
		chainID = nodes[0].Spec.ChainID
	}

	evmList, err := publishEndpoints(ctx, c, p.Workflow, p.Role, chainID, nodes)
	if err != nil {
		return Result{}, err
	}
	return Result{Names: names, ChainID: chainID, EVMRPCList: evmList}, nil
}

func validateParams(p Params) error {
	switch {
	case p.Role == "":
		return fmt.Errorf("provision-node: --role is required")
	case p.TemplatePath == "":
		return fmt.Errorf("provision-node: --template is required")
	case p.Replicas < 1:
		return fmt.Errorf("provision-node: --replicas must be >= 1, got %d", p.Replicas)
	case p.Workflow.Name == "" || p.Workflow.Namespace == "":
		return fmt.Errorf("provision-node: workflow identity not loaded")
	}
	// The runtime injects .ORDINAL and .NODE_NAME per replica; a --var on
	// either would silently shadow them. Reject, mirroring the runner's
	// --var NODE= guard under --per-node-selector.
	if _, ok := p.Vars["ORDINAL"]; ok {
		return fmt.Errorf("provision-node: --var ORDINAL=... collides with the runtime-injected .ORDINAL")
	}
	if _, ok := p.Vars["NODE_NAME"]; ok {
		return fmt.Errorf("provision-node: --var NODE_NAME=... collides with the runtime-injected .NODE_NAME")
	}
	return nil
}

func withDefaults(p Params) Params {
	if p.Name == "" {
		base := p.Workflow.Name
		if cid := p.Vars[string(taskruntime.KeyChainID)]; cid != "" {
			base = cid
		}
		p.Name = base + "-" + p.Role
	}
	if p.NetworkNamespace == "" {
		p.NetworkNamespace = p.Workflow.Namespace
	}
	if p.RunningTimeout == 0 {
		p.RunningTimeout = 15 * time.Minute
	}
	if p.FirstBlockTimeout == 0 {
		p.FirstBlockTimeout = 5 * time.Minute
	}
	if p.PollInterval == 0 {
		p.PollInterval = 5 * time.Second
	}
	return p
}

// renderNode parses the template, executes it against the caller's vars plus
// the runtime-injected .ORDINAL and .NODE_NAME, then strict-unmarshals the
// rendered bytes into a SeiNode so field typos fail here, not at Create time.
func renderNode(p Params, ordinal int) (*seiv1alpha1.SeiNode, error) {
	raw, err := os.ReadFile(p.TemplatePath)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	tmpl, err := template.New(p.TemplatePath).Option("missingkey=error").Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	ctxVars := make(map[string]string, len(p.Vars)+2)
	maps.Copy(ctxVars, p.Vars)
	ctxVars["ORDINAL"] = strconv.Itoa(ordinal)
	ctxVars["NODE_NAME"] = nodeName(p.Name, ordinal)

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, ctxVars); err != nil {
		return nil, fmt.Errorf("execute: %w", err)
	}
	out := &seiv1alpha1.SeiNode{}
	if err := yaml.UnmarshalStrict(buf.Bytes(), out); err != nil {
		return nil, fmt.Errorf("unmarshal rendered yaml: %w", err)
	}
	return out, nil
}

func nodeName(base string, ordinal int) string {
	return base + "-" + strconv.Itoa(ordinal)
}

// stampMetadata overwrites metadata fields the template MUST NOT control,
// stamps the shared object-label producer contract (§2.2a), and appends the
// synthesized peer source (§3). OwnerReferences are assigned (not appended)
// so a template that smuggles a bogus ref can't leak through.
func stampMetadata(node *seiv1alpha1.SeiNode, p Params, ordinal int) {
	node.APIVersion = seiv1alpha1.GroupVersion.String()
	node.Kind = "SeiNode"
	node.Name = nodeName(p.Name, ordinal)
	node.Namespace = p.Workflow.Namespace
	node.OwnerReferences = []metav1.OwnerReference{p.Workflow.OwnerRef()}

	// Object-label producer contract — identical to `seictl node apply`.
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	node.Labels[labelRole] = roleValueNode
	if p.Network != "" {
		node.Labels[labelSeiNetwork] = p.Network
	}

	// Peer auto-wiring: synthesize the genesis-pool label source. Appended
	// (not assigned) so a template's own static seed peers compose naturally.
	if p.Network != "" {
		node.Spec.Peers = append(node.Spec.Peers, seiv1alpha1.PeerSource{
			Label: &seiv1alpha1.LabelPeerSource{
				Selector:  map[string]string{labelSeiNetwork: p.Network},
				Namespace: p.NetworkNamespace,
			},
		})
	}
}

// warnIfDrift logs when a re-run finds the on-cluster SeiNode.Spec different
// from the freshly-rendered one. Operators who edited the template since the
// original Create need to know the cluster still has the old spec.
func warnIfDrift(ctx context.Context, c client.Client, fresh *seiv1alpha1.SeiNode) {
	existing := &seiv1alpha1.SeiNode{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: fresh.Namespace, Name: fresh.Name}, existing); err != nil {
		return
	}
	if reflect.DeepEqual(existing.Spec, fresh.Spec) {
		return
	}
	fmt.Fprintf(os.Stderr, "WARN: SeiNode %s/%s exists with spec different from rendered template; reusing on-cluster spec\n", fresh.Namespace, fresh.Name)
}

// waitForRunning polls each of the named SeiNodes until .status.phase ==
// PhaseRunning, failing fast on PhaseFailed. All N share one deadline.
func waitForRunning(ctx context.Context, c client.Client, ns string, names []string, timeout, interval time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, interval, timeout, true, func(ctx context.Context) (bool, error) {
		for _, name := range names {
			node := &seiv1alpha1.SeiNode{}
			if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, node); err != nil {
				if apierrors.IsNotFound(err) {
					return false, nil
				}
				return false, taskruntime.Infra(fmt.Errorf("reading SeiNode %s: %w", name, err))
			}
			switch node.Status.Phase {
			case seiv1alpha1.PhaseRunning:
				// this node done; check the rest
			case seiv1alpha1.PhaseFailed:
				return false, taskruntime.Task(fmt.Errorf("SeiNode %s reached Failed phase", name))
			default:
				return false, nil
			}
		}
		return true, nil
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

// waitForFirstBlock polls a Tendermint /status until latest_block_height > 0,
// gating a follower's TM liveness (the node has joined and is syncing).
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

// evmRPCResponse models a JSON-RPC envelope with a string result, used to
// confirm eth_blockNumber returned a well-formed reply.
type evmRPCResponse struct {
	Result string `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// waitForEVMReady POSTs eth_blockNumber to the node's EVM JSON-RPC URL and
// requires HTTP 200 plus a non-empty, error-free result. This is the gate
// (§5.2 stage 3) that proves the EVM listener is BOUND before its URL enters
// RPC_EVM_RPC_LIST — TM height>0 does not prove the EVM listener accepts
// connections (WS-A0: .status.endpoint is discoverability, not serve-readiness).
// Non-200 / connection-refused (listener not yet up) keeps polling until the
// timeout, then infra-fails.
func waitForEVMReady(ctx context.Context, hc *http.Client, evmRPC string, timeout, interval time.Duration) error {
	const body = `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`
	if err := wait.PollUntilContextTimeout(ctx, interval, timeout, true, func(ctx context.Context) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, evmRPC, strings.NewReader(body))
		if err != nil {
			return false, taskruntime.Infra(fmt.Errorf("eth_blockNumber req: %w", err))
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			return false, nil
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return false, nil
		}
		var parsed evmRPCResponse
		if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
			return false, nil
		}
		if parsed.Error != nil || parsed.Result == "" {
			return false, nil
		}
		return true, nil
	}); err != nil {
		return taskruntime.Infra(fmt.Errorf("EVM JSON-RPC %s not ready within %s: %w", evmRPC, timeout, err))
	}
	return nil
}

// publishEndpoints assembles all five workflow-vars keys from the N per-node
// .status.endpoint scalars (a standalone SeiNode has no fleet aggregate) and
// writes them. Returns the assembled EVM CSV for the Result summary.
//
// Empty-guard (§6.4): every node's evmJsonRpc must be non-empty (a missing
// follower endpoint is a provisioning fault, not a filterable condition), and
// node-0's tendermintRpc must be non-empty before it feeds <ROLE>_TM_RPC /
// <ROLE>_REST (guards a future non-EVM role from emitting a garbage URL the
// chaos wait-for-caught-up probe would curl).
func publishEndpoints(ctx context.Context, c client.Client, w taskruntime.WorkflowIdentity, role, chainID string, nodes []*seiv1alpha1.SeiNode) (string, error) {
	if len(nodes) == 0 {
		return "", taskruntime.Infra(fmt.Errorf("provision-node: no SeiNodes to publish"))
	}

	urls := make([]string, 0, len(nodes))
	for _, n := range nodes { // nodes ordered 0..N-1
		ep := n.Status.Endpoint
		if ep == nil || ep.EvmJsonRpc == "" {
			return "", taskruntime.Infra(fmt.Errorf("SeiNode %s Running but .status.endpoint.evmJsonRpc empty", n.Name))
		}
		urls = append(urls, ep.EvmJsonRpc)
	}

	node0 := nodes[0].Status.Endpoint
	if node0.TendermintRpc == "" {
		return "", taskruntime.Infra(fmt.Errorf("SeiNode %s Running but .status.endpoint.tendermintRpc empty", nodes[0].Name))
	}
	if node0.TendermintRest == "" {
		return "", taskruntime.Infra(fmt.Errorf("SeiNode %s Running but .status.endpoint.tendermintRest empty", nodes[0].Name))
	}

	evmList := strings.Join(urls, ",")

	if err := taskruntime.EnsureWorkflowVarsCM(ctx, c, w, map[taskruntime.VarKey]string{
		taskruntime.KeyRunID: w.Name,
	}); err != nil {
		return "", err
	}
	vars := map[taskruntime.VarKey]string{
		// CHAIN_ID lives in SetVars (merge), not the EnsureWorkflowVarsCM seed
		// (no-op on AlreadyExists): the genesis provision step creates the CM
		// first, so a CHAIN_ID seed here would be silently dropped.
		taskruntime.KeyChainID: chainID,
		taskruntime.RoleScoped(role, taskruntime.KeyEVMJSONRPCList): evmList,
		taskruntime.RoleScoped(role, taskruntime.KeyEVMJSONRPC):     node0.EvmJsonRpc,
		taskruntime.RoleScoped(role, taskruntime.KeyTendermintRPC):  node0.TendermintRpc,
		taskruntime.RoleScoped(role, taskruntime.KeyTendermintREST): node0.TendermintRest,
	}
	if err := taskruntime.SetVars(ctx, c, w, vars); err != nil {
		return "", err
	}
	return evmList, nil
}
