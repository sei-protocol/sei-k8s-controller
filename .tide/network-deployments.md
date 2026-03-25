# Component: Genesis Network Configuration on SeiNodeGroup

**Date:** 2026-03-22
**Status:** Implementation-Ready (cross-review round 4 resolved)
**Last revised:** 2026-03-25

---

## Owner

Platform / Infrastructure (joint: Kubernetes Specialist + Platform Engineer)

## Phase

Pre-Tide — Operational prerequisite. Spinning up isolated validator networks is required for image qualification, load testing, and (in a future iteration) upgrade rehearsal.

## Purpose

Add an optional `genesis` field to `SeiNodeGroupSpec` that bootstraps a self-contained Sei validator network. The design preserves the task engine as a pure sequencer — tasks succeed or fail, data flows through the filesystem and S3, and the controller sequences execution without routing data.

**Scope:** Genesis ceremony and validator network bootstrap only.

**Architecture principles (this iteration):**
1. **Engine stays stateless.** Tasks return success/fail only. No `TaskResult.Result` field. No inter-task output dependencies.
2. **State lives on PVC + S3.** Per-node genesis artifacts (identity, gentx) flow through the PVC and are uploaded to S3. Assembly reads from S3. Final genesis is distributed via S3.
3. **Controller is a sequencer.** It builds plans, submits tasks, and polls for completion. The one exception: after assembly, the group controller queries each node's sidecar `/v0/node-id` endpoint to build the static peer list. This is a read-only metadata query, not task output routing.

---

## Dependencies

- **seid binary** (accessible from sidecar in PreInit Job — via init container copy)
- **seictl sidecar** — three new task types: `generate-identity`, `generate-gentx`, `upload-genesis-artifacts`. One group-level task: `assemble-and-upload-genesis`. One new HTTP endpoint: `GET /v0/node-id`. No engine changes required.
- **sei-config** — `GenesisDefaults()` function for test-network genesis parameter defaults
- **S3** — artifact store for cross-node data flow; genesis distribution via existing `configure-genesis` S3 path
- **SeiNode controller** — PreInit gate for genesis nodes; Init plan deferred to gate-clear time
- **SeiNodeGroup controller** — thin coordination: observe PreInit completion, trigger assembly on node 0, collect node IDs, set genesis source + static peers on each SeiNode

**Explicit exclusions:**
- `TaskResult.Result` / typed task results — separate design deliverable
- `TaskExecutionInterface` — separate design deliverable (informs but does not block genesis)
- `UpgradePolicy` — separate design iteration
- EFS / ReadWriteMany storage — not needed (S3 is the cross-node channel)
- PreInit/Init phase merge — deferred until TaskExecutionInterface provides the abstraction to make the phase boundary invisible (see Deferred section)

---

## Interface Specification

### CRD Changes: `SeiNodeGroupSpec`

```go
type SeiNodeGroupSpec struct {
    Replicas       int32            `json:"replicas"`
    Template       SeiNodeTemplate  `json:"template"`
    DeletionPolicy DeletionPolicy   `json:"deletionPolicy,omitempty"`
    Networking     *NetworkingConfig `json:"networking,omitempty"`
    Monitoring     *MonitoringConfig `json:"monitoring,omitempty"`

    // Genesis enables bootstrapping a new validator network from scratch.
    // Requires template.spec.validator to be set.
    // +optional
    Genesis *GenesisCeremonyConfig `json:"genesis,omitempty"`
}
```

### `GenesisCeremonyConfig`

```go
type GenesisCeremonyConfig struct {
    // ChainID for the new network.
    // +kubebuilder:validation:MinLength=1
    // +kubebuilder:validation:MaxLength=64
    // +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9-]*[a-z0-9]$`
    ChainID string `json:"chainId"`

    // StakingAmount is the amount each validator self-delegates in its gentx.
    // Default: "10000000usei"
    // +optional
    StakingAmount string `json:"stakingAmount,omitempty"`

    // AccountBalance is the initial balance for each validator's genesis account.
    // Default: "1000000000000000000000usei,1000000000000000000000uusdc,1000000000000000000000uatom"
    // +optional
    AccountBalance string `json:"accountBalance,omitempty"`

    // Accounts adds non-validator genesis accounts (e.g. for load test funding).
    // +optional
    Accounts []GenesisAccount `json:"accounts,omitempty"`

    // Overrides is a flat map of dotted key paths merged on top of sei-config's
    // GenesisDefaults(). Applied BEFORE gentx generation.
    // +optional
    Overrides map[string]string `json:"overrides,omitempty"`

    // GenesisS3 configures where genesis artifacts are stored.
    // When omitted, inferred: bucket = "sei-genesis-artifacts",
    // prefix = "<chainId>/<group-name>/", region from PlatformConfig.
    // +optional
    GenesisS3 *GenesisS3Destination `json:"genesisS3,omitempty"`

    // MaxCeremonyDuration is the maximum time from group creation to genesis
    // assembly completion. Default: "15m".
    // +optional
    MaxCeremonyDuration *metav1.Duration `json:"maxCeremonyDuration,omitempty"`
}

type GenesisS3Destination struct {
    Bucket string `json:"bucket"`
    Prefix string `json:"prefix,omitempty"`
    Region string `json:"region"`
}

type GenesisAccount struct {
    Address string `json:"address"`
    Balance string `json:"balance"`
}
```

### CRD Changes: `ValidatorSpec`

```go
type ValidatorSpec struct {
    // ... existing fields (Peers, Snapshot, etc.) ...

    // GenesisCeremony indicates this validator participates in a group genesis.
    // +optional
    GenesisCeremony *GenesisCeremonyNodeConfig `json:"genesisCeremony,omitempty"`
}

type GenesisCeremonyNodeConfig struct {
    ChainID         string           `json:"chainId"`
    StakingAmount   string           `json:"stakingAmount"`
    AccountBalances []AccountBalance `json:"accountBalances"`
    GenesisParams   string           `json:"genesisParams,omitempty"`
    Index           int32            `json:"index"`

    // ArtifactS3 is the S3 location where this node uploads its genesis artifacts.
    ArtifactS3 GenesisS3Destination `json:"artifactS3"`
}

type AccountBalance struct {
    Address string `json:"address"`
    Amount  string `json:"amount"`
}
```

### CRD Changes: `PeerSource`

No new peer source types. Genesis peers are pushed by the group controller as `StaticPeerSource` entries in `nodeId@host:port` format. The existing CEL validation rule is unchanged:

```go
// +kubebuilder:validation:XValidation:rule="(has(self.ec2Tags) ? 1 : 0) + (has(self.static) ? 1 : 0) == 1",message="exactly one of ec2Tags or static must be set"
```

### `SeiNodeGroupStatus` Additions

```go
type SeiNodeGroupStatus struct {
    // ... existing fields ...

    // AssemblyPlan tracks the genesis assembly task on index 0's sidecar.
    // +optional
    AssemblyPlan *TaskPlan `json:"assemblyPlan,omitempty"`

    // GenesisHash is the SHA-256 hex digest of the assembled genesis.json.
    // +optional
    GenesisHash string `json:"genesisHash,omitempty"`

    // GenesisS3URI is the S3 URI of the uploaded genesis.
    // +optional
    GenesisS3URI string `json:"genesisS3URI,omitempty"`
}
```

### New Sidecar Task Types

Four new task types. No engine changes — all use the existing `func(ctx, params) error` handler signature.

| Task Type | Params | PVC Side Effects | S3 Side Effects |
|---|---|---|---|
| `generate-identity` | `chainId`, `moniker`, `seidPath` | Writes key material, `$SEI_HOME/.genesis/identity.json` | None |
| `generate-gentx` | `stakingAmount`, `chainId`, `accountBalances[]`, `genesisParams`, `seidPath` | Writes gentx, `$SEI_HOME/.genesis/gentx.json` | None |
| `upload-genesis-artifacts` | `s3Bucket`, `s3Prefix`, `s3Region`, `nodeName` | Reads `.genesis/identity.json`, `.genesis/gentx.json` | Uploads to `s3://<bucket>/<prefix>/<nodeName>/` |
| `assemble-and-upload-genesis` | `s3Bucket`, `s3Prefix`, `s3Region`, `chainId`, `seidPath`, `nodes[]` (see `GenesisNodeParam` below) | Writes assembled genesis locally for seid | Reads `<node.name>/identity.json` + `<node.name>/gentx.json` per node, uploads `genesis.json` |

All tasks return `error` only. Data flows through PVC files and S3. The engine never routes data between tasks.

**`GenesisNodeParam`** (wire format for `nodes[]` in `assemble-and-upload-genesis`):
```go
type GenesisNodeParam struct {
    Name string `json:"name"` // S3 key segment: <prefix>/<name>/identity.json
}
```

No S3 ListObjects required — node names are passed as task params, making key construction deterministic.

**`identity.json`** (written by `generate-identity`, read by `assemble-and-upload-genesis`):
```json
{"nodeId": "abc123...", "accountAddress": "sei1...", "consensusPubkey": "..."}
```

### New Sidecar HTTP Endpoint: `GET /v0/node-id`

A read-only metadata endpoint on the sidecar HTTP server. Returns the node's Tendermint node ID by reading `$SEI_HOME/config/node_key.json` from the PVC. Available after `generate-identity` has run.

```json
{"nodeId": "abc123def456..."}
```

This is called by the group controller after assembly — not by peer sidecars. The sidecar HTTP server is already running during PreInit (it's how the controller submits tasks).

**Implementation notes:**
- `Server` currently holds `(addr, engine, mux)` but not `homeDir`. The constructor must be extended to accept `homeDir` (or an `IdentityProvider` interface) so the endpoint can locate `node_key.json`.
- CometBFT's `node_key.json` stores a `priv_key` object, not a literal `nodeId` field. The node ID is **derived** from the Ed25519 public key (same derivation as `seid tendermint show-node-id`). Use CometBFT's `p2p.LoadNodeKey()` or equivalent to extract the ID.
- The derived ID must match the `nodeId` field in `identity.json` (both sourced from the same key material written by `generate-identity`).
- Invariant: sidecar `homeDir` == task `homeDir` for PreInit (same PVC mount, same `--home` / `SEI_HOME`).

**Controller client:** `SidecarStatusClient` needs a new `GetNodeID(ctx) (string, error)` method. The concrete `SidecarClient` implements it as `GET /v0/node-id` with JSON parsing. All mocks/fakes in tests must be updated.

### sei-config: `GenesisDefaults()`

Unchanged from prior revision — test-network genesis parameter defaults in sei-config.

---

## State Model

```
┌──────────────────────────────────────────────────────────────────────────────┐
│ Data flows through PVC (local) and S3 (cross-node). Engine is a sequencer.  │
│                                                                              │
│ Each SeiNode PreInit plan (3 tasks, all filesystem ops):                     │
│ ┌──────────────────────────────────────────────────────────────────┐         │
│ │ generate-identity        PVC: keys, identity.json               │         │
│ │       ↓                                                         │         │
│ │ generate-gentx           PVC: gentx.json, patched genesis       │         │
│ │       ↓                                                         │         │
│ │ upload-genesis-artifacts PVC→S3: identity.json, gentx.json      │         │
│ └──────────────────────────────────────────────────────────────────┘         │
│                                                                              │
│ SeiNodeGroup coordination (reconcile loop):                                  │
│  1. Create SeiNodes → PreInit plans run independently                        │
│  2. Watch: all SeiNodes PreInit plans complete?                               │
│  3. Submit assemble-and-upload-genesis to node 0's sidecar                   │
│     S3→PVC→seid→S3: reads all artifacts, assembles, uploads genesis.json     │
│  4. Query GET /v0/node-id on each node's sidecar → build peer list           │
│  5. Set spec.genesis.s3 + spec.validator.peers (static) on each SeiNode      │
│     → PreInit gate clears                                                    │
│                                                                              │
│ Each SeiNode Init plan (standard — no new peer source types):                │
│ ┌──────────────────────────────────────────────────────────────────┐         │
│ │ configure-genesis    S3→PVC: downloads assembled genesis.json   │         │
│ │       ↓                                                         │         │
│ │ config-apply         Writes node config via sei-config          │         │
│ │       ↓                                                         │         │
│ │ discover-peers       Static peers: writes persistent_peers      │         │
│ │                      (already in nodeId@host:port format)       │         │
│ │       ↓                                                         │         │
│ │ config-validate → mark-ready                                    │         │
│ └──────────────────────────────────────────────────────────────────┘         │
│                                                                              │
│ Controller reads nodeIds via sidecar HTTP (not task output).                 │
│ Peers flow through existing StaticPeerSource. No new sidecar peer logic.     │
└──────────────────────────────────────────────────────────────────────────────┘
```

---

## Internal Design

### 1. Validator Planner: Genesis PreInit

```go
func needsPreInit(node *seiv1alpha1.SeiNode) bool {
    // ... existing snapshot check ...
    if node.Spec.Validator != nil && node.Spec.Validator.GenesisCeremony != nil {
        return true
    }
    return false
}

func buildGenesisPreInitPlan() *seiv1alpha1.TaskPlan {
    return &seiv1alpha1.TaskPlan{
        Phase: seiv1alpha1.TaskPlanActive,
        Tasks: []seiv1alpha1.PlannedTask{
            {Type: taskGenerateIdentity, Status: seiv1alpha1.PlannedTaskPending},
            {Type: taskGenerateGentx, Status: seiv1alpha1.PlannedTaskPending},
            {Type: taskUploadGenesisArtifacts, Status: seiv1alpha1.PlannedTaskPending},
        },
    }
}
```

In `reconcilePending`, Init plan is deferred for genesis nodes (peers and genesis source not yet available):

```go
if isGenesisCeremonyNode(node) {
    node.Status.PreInitPlan = buildGenesisPreInitPlan()
} else {
    node.Status.PreInitPlan = buildPreInitPlan(node, planner)
    if needsPreInit(node) {
        node.Status.InitPlan = buildPostBootstrapInitPlan(node)
    } else {
        node.Status.InitPlan = planner.BuildPlan(node)
    }
}
```

### 2. Genesis PreInit Job

The existing `generatePreInitJob` / `buildPreInitPodSpec` assumes a snapshot source (dereferences `snap.S3.TargetHeight`). For genesis nodes, `snapshotSourceFor` returns nil — this would nil-panic. Genesis nodes branch to `generateGenesisPreInitJob` which builds a different pod spec:

```go
func generatePreInitJob(node *seiv1alpha1.SeiNode, platform PlatformConfig) *batchv1.Job {
    if isGenesisCeremonyNode(node) {
        return generateGenesisPreInitJob(node, platform)
    }
    // ... existing snapshot path (unchanged) ...
}
```

Genesis PreInit Job differences from snapshot PreInit:
- **No `snapshotSourceFor` call** — genesis has no snapshot
- **Init container:** `copy-seid` copies binary to PVC (genesis tasks need seid)
- **Main container:** `sleep infinity` keepalive (not `seid start --halt-height`)
- **Same IRSA service account** — S3 upload needs credentials
- Pod stays alive for group-submitted assembly task on node 0

### 3. Genesis Task Builders

```go
func generateIdentityTask(node *seiv1alpha1.SeiNode) sidecar.TaskBuilder {
    gc := node.Spec.Validator.GenesisCeremony
    return sidecar.GenerateIdentityTask{
        ChainID:  gc.ChainID,
        Moniker:  node.Name,
        SeidPath: filepath.Join(dataDir, "bin", "seid"),
    }
}

func generateGentxTask(node *seiv1alpha1.SeiNode) sidecar.TaskBuilder {
    gc := node.Spec.Validator.GenesisCeremony
    return sidecar.GenerateGentxTask{
        ChainID:         gc.ChainID,
        StakingAmount:   gc.StakingAmount,
        AccountBalances: gc.AccountBalances,
        GenesisParams:   gc.GenesisParams,
        SeidPath:        filepath.Join(dataDir, "bin", "seid"),
    }
}

func uploadGenesisArtifactsTask(node *seiv1alpha1.SeiNode) sidecar.TaskBuilder {
    gc := node.Spec.Validator.GenesisCeremony
    return sidecar.UploadGenesisArtifactsTask{
        S3Bucket: gc.ArtifactS3.Bucket,
        S3Prefix: gc.ArtifactS3.Prefix,
        S3Region: gc.ArtifactS3.Region,
        NodeName: node.Name,
    }
}
```

### 4. PreInit Gate for Genesis Nodes

The existing `reconcilePreInitializing` has two places where `plan.Phase == Complete` leads to `isJobComplete(job)` checks (lines 26-33 and 104-108 in `pre_init.go`). For genesis nodes with `sleep infinity`, `isJobComplete` is permanently false — this would deadlock the node in a requeue loop with a misleading "waiting for halt-height" log.

`handlePreInitComplete` replaces BOTH inline completion guards for genesis nodes. It must be the only exit path when the plan is complete:

```go
func (r *SeiNodeReconciler) handlePreInitComplete(ctx context.Context, node *seiv1alpha1.SeiNode, planner NodePlanner) (ctrl.Result, error) {
    if isGenesisCeremonyNode(node) {
        if node.Spec.Genesis.S3 == nil {
            log.FromContext(ctx).Info("genesis PreInit complete, waiting for group to set genesis source")
            return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
        }
        patch := client.MergeFrom(node.DeepCopy())
        node.Status.InitPlan = buildPostBootstrapInitPlan(node)
        if err := r.Status().Patch(ctx, node, patch); err != nil {
            return ctrl.Result{}, fmt.Errorf("building genesis Init plan: %w", err)
        }
    } else {
        // Snapshot: wait for halt-height (existing)
        // ...
    }

    if err := r.cleanupPreInit(ctx, node); err != nil {
        return ctrl.Result{}, fmt.Errorf("cleaning up pre-init: %w", err)
    }
    return r.transitionPhase(ctx, node, seiv1alpha1.PhaseInitializing)
}
```

### 5. SeiNodeGroup Genesis Coordination

Thin coordination layer. After assembly, the group controller collects node IDs via the sidecar HTTP endpoint and pushes static peers to each SeiNode.

```go
type SeiNodeGroupReconciler struct {
    client.Client
    Scheme   *runtime.Scheme
    Recorder record.EventRecorder
    ControllerSA string

    BuildSidecarClientFn func(baseURL string) SidecarStatusClient
}
```

**Genesis injection in `generateSeiNode`:** Beyond ChainID, the full `GenesisCeremonyNodeConfig` must be injected. The current `generateSeiNode` does a plain `DeepCopy` of the template — no genesis logic exists.

```go
if group.Spec.Genesis != nil {
    if spec.ChainID == "" {
        spec.ChainID = group.Spec.Genesis.ChainID
    }
    if spec.Validator == nil {
        spec.Validator = &seiv1alpha1.ValidatorSpec{}
    }
    s3Cfg := r.genesisS3Config(group)
    spec.Validator.GenesisCeremony = &seiv1alpha1.GenesisCeremonyNodeConfig{
        ChainID:         group.Spec.Genesis.ChainID,
        StakingAmount:   valueOrDefault(group.Spec.Genesis.StakingAmount, "10000000usei"),
        AccountBalances: buildAccountBalances(group.Spec.Genesis),
        GenesisParams:   buildGenesisParamsJSON(group.Spec.Genesis.Overrides),
        Index:           int32(ordinal),
        ArtifactS3:      s3Cfg,
    }
}
```

**Reconcile:**

```go
func (r *SeiNodeGroupReconciler) reconcileGenesisAssembly(ctx context.Context, group *SeiNodeGroup) (bool, ctrl.Result, error) {
    nodes, err := r.listChildSeiNodes(ctx, group)
    if err != nil {
        return false, ctrl.Result{}, err
    }

    // Timeout check
    if group.Spec.Genesis.MaxCeremonyDuration != nil {
        if time.Since(group.CreationTimestamp.Time) > group.Spec.Genesis.MaxCeremonyDuration.Duration {
            return false, ctrl.Result{}, fmt.Errorf("genesis ceremony timed out")
        }
    }

    // Early failure detection — surface errors before timeout
    for _, node := range nodes {
        if node.Status.PreInitPlan != nil && node.Status.PreInitPlan.Phase == seiv1alpha1.TaskPlanFailed {
            r.Recorder.Eventf(group, corev1.EventTypeWarning, "GenesisPreInitFailed",
                "Node %s PreInit failed", node.Name)
            meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
                Type: "GenesisFailed", Status: metav1.ConditionTrue,
                Reason: "PreInitFailed", Message: fmt.Sprintf("node %s PreInit failed", node.Name),
            })
            return false, ctrl.Result{}, fmt.Errorf("node %s PreInit failed", node.Name)
        }
    }

    // Wait for ALL PreInit plans to complete (all artifacts uploaded to S3)
    for _, node := range nodes {
        if node.Status.PreInitPlan == nil || node.Status.PreInitPlan.Phase != seiv1alpha1.TaskPlanComplete {
            return false, ctrl.Result{RequeueAfter: 10 * time.Second}, nil
        }
    }

    // Sort by ordinal so nodes[0] is deterministically the index-0 node.
    // listChildSeiNodes returns in API list order which is not guaranteed.
    sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })

    // Build or resume the assembly plan (targets node 0's PreInit sidecar)
    if group.Status.AssemblyPlan == nil {
        group.Status.AssemblyPlan = buildAssemblyPlan()
        patch := client.MergeFrom(group.DeepCopy())
        if err := r.Status().Patch(ctx, group, patch); err != nil {
            return false, ctrl.Result{}, err
        }
    }

    // Execute assembly plan against node 0's sidecar
    sc := r.BuildSidecarClientFn(PreInitSidecarURL(&nodes[0]))
    result, err := r.executeAssemblyPlan(ctx, group, sc, nodes)
    if err != nil {
        return false, result, err
    }
    if group.Status.AssemblyPlan.Phase != seiv1alpha1.TaskPlanComplete {
        return false, result, nil
    }

    // Assembly complete — S3 now has genesis.json.
    // Collect node IDs from each node's sidecar to build the peer list.
    peers, err := r.collectPeers(ctx, group, nodes)
    if err != nil {
        return false, ctrl.Result{}, fmt.Errorf("collecting peer node IDs: %w", err)
    }

    // Set each SeiNode's genesis source + static peers. This unblocks the PreInit gate.
    s3Cfg := r.genesisS3Config(group)
    genesisURI := fmt.Sprintf("s3://%s/%sgenesis.json", s3Cfg.Bucket, s3Cfg.Prefix)

    for i := range nodes {
        if err := r.setGenesisSource(ctx, &nodes[i], genesisURI, s3Cfg.Region, peers); err != nil {
            return false, ctrl.Result{}, err
        }
    }

    // Record completion
    patch := client.MergeFrom(group.DeepCopy())
    group.Status.GenesisS3URI = genesisURI
    if err := r.Status().Patch(ctx, group, patch); err != nil {
        return false, ctrl.Result{}, err
    }

    return true, ctrl.Result{}, nil
}
```

**`collectPeers`** queries each node's sidecar for its Tendermint node ID and builds the peer list:

```go
func (r *SeiNodeGroupReconciler) collectPeers(ctx context.Context, group *SeiNodeGroup, nodes []seiv1alpha1.SeiNode) ([]string, error) {
    peers := make([]string, 0, len(nodes))
    for i := range nodes {
        sc := r.BuildSidecarClientFn(PreInitSidecarURL(&nodes[i]))
        nodeID, err := sc.GetNodeID(ctx)
        if err != nil {
            return nil, fmt.Errorf("getting node ID for %s: %w", nodes[i].Name, err)
        }
        dns := fmt.Sprintf("%s-0.%s.%s.svc.cluster.local:%d",
            nodes[i].Name, nodes[i].Name, nodes[i].Namespace, p2pPort)
        peers = append(peers, fmt.Sprintf("%s@%s", nodeID, dns))
    }
    return peers, nil
}
```

**`setGenesisSource`** sets two fields using existing types — no new CRD peer source variants:

```go
func (r *SeiNodeGroupReconciler) setGenesisSource(ctx context.Context, node *seiv1alpha1.SeiNode, genesisURI, region string, peers []string) error {
    patch := client.MergeFrom(node.DeepCopy())
    node.Spec.Genesis = &seiv1alpha1.GenesisConfiguration{
        S3: &seiv1alpha1.GenesisS3Source{URI: genesisURI, Region: region},
    }
    node.Spec.Validator.Peers = []seiv1alpha1.PeerSource{
        {Static: &seiv1alpha1.StaticPeerSource{Addresses: peers}},
    }
    return r.Patch(ctx, node, patch)
}
```

The Init plan's `discover-peers` task receives these static peers through the existing `discoverPeersTask` builder — zero changes to the sidecar's peer discovery handler.

### 6. S3 Client Interface for Genesis

Genesis tasks need both read (GetObject) and write (PutObject) on small files (< 10KB). The existing `S3GetObjectAPI` (in `tasks/genesis.go`) handles reads; the `UploaderFactory` / `transfermanager` is designed for large multi-part uploads and is overkill for genesis artifacts.

New interface for genesis use:

```go
type S3ObjectClient interface {
    GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
    PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

type S3ObjectClientFactory func(ctx context.Context, region string) (S3ObjectClient, error)
```

`*s3.Client` natively satisfies this interface. The existing `DefaultS3ClientFactory` returns `s3.NewFromConfig(cfg)` — just widen the return type. Reserve `transfermanager` for snapshot-sized uploads. All four genesis handlers receive `S3ObjectClientFactory` via constructor injection.

### 7. S3 Artifact Convention

All genesis artifacts flow through a single S3 prefix:

```
s3://<bucket>/<prefix>/
  ├── <node-0-name>/
  │   ├── identity.json
  │   └── gentx.json
  ├── <node-1-name>/
  │   ├── identity.json
  │   └── gentx.json
  └── genesis.json          (assembled, uploaded by assemble-and-upload-genesis)
```

Default convention: `bucket = "sei-genesis-artifacts"`, `prefix = "<chainId>/<group-name>/"`. Override via `genesis.genesisS3`. Genesis artifacts are kept separate from snapshot buckets — different lifecycle, access patterns, and IAM scoping.

---

## Error Handling

| Error Case | Detection | Response |
|---|---|---|
| `template.spec.validator` not set with genesis | CEL validation | API server rejects |
| PreInit task fails | SeiNode.status.preInitPlan.phase = Failed | Group emits event, sets condition `GenesisFailed` |
| S3 artifact upload fails | `upload-genesis-artifacts` task error | SeiNode PreInit plan fails, group detects |
| Assembly task fails | AssemblyPlan task error | Group emits event, sets condition `GenesisFailed` |
| Node ID collection fails | `collectPeers` HTTP error | Group retries on next reconcile (sidecars still alive) |
| Ceremony timeout | `time.Since(creation) > maxDuration` | Group sets condition `GenesisCeremonyTimedOut` |
| Controller restart during assembly | AssemblyPlan persisted in status | Resumes poll from current task state |

### Marker File Convention

Each handler writes a marker file on completion for idempotency. Marker names MUST NOT collide with existing markers (particularly `.sei-sidecar-genesis-done` used by `configure-genesis` in Init):

| Handler | Marker |
|---|---|
| `generate-identity` | `.sei-sidecar-identity-done` |
| `generate-gentx` | `.sei-sidecar-gentx-done` |
| `upload-genesis-artifacts` | `.sei-sidecar-upload-artifacts-done` |
| `assemble-and-upload-genesis` | `.sei-sidecar-assemble-genesis-done` |

PreInit and Init share the same PVC — distinct prefixes prevent cross-phase collisions.

### Invariant: `ensureSeiNode` Spec Sync

`ensureSeiNode` syncs Labels, Annotations, Image, Entrypoint, Sidecar, PodLabels only. It MUST NOT sync fields under `spec.genesis`, `spec.validator.peers`, or `spec.validator.genesisCeremony` for genesis-managed nodes — these are set by `setGenesisSource` on a separate code path.

---

## Test Specification

### Unit Tests — Sidecar (seictl)

| Test | Setup | Action | Expected |
|---|---|---|---|
| `TestGenerateIdentity_HappyPath` | Temp SEI_HOME, seid at seidPath | Handle | identity.json written, marker file created |
| `TestGenerateIdentity_Idempotent` | SEI_HOME with marker | Handle | No seid commands, no error |
| `TestGenerateGentx_HappyPath` | SEI_HOME with identity | Handle with 2 accounts | gentx.json written, genesis patched |
| `TestGenerateGentx_Idempotent` | SEI_HOME with marker | Handle | No seid commands, no error |
| `TestUploadGenesisArtifacts_HappyPath` | identity.json + gentx.json on PVC, mock S3 | Handle | Two S3 PutObject calls at correct keys |
| `TestUploadGenesisArtifacts_Idempotent` | Marker file exists | Handle | No S3 calls |
| `TestAssembleAndUploadGenesis_HappyPath` | Mock S3 with 4 nodes' artifacts | Handle | genesis.json uploaded, marker created |
| `TestAssembleAndUploadGenesis_Idempotent` | Marker file exists | Handle | No S3 calls, no error |
| `TestNodeIDEndpoint_ReturnsNodeId` | node_key.json on PVC | GET /v0/node-id | 200 with nodeId |
| `TestNodeIDEndpoint_NoKeyFile` | Empty PVC | GET /v0/node-id | 404 or 500 |

### Unit Tests — Controller (sei-k8s-controller)

| Test | Setup | Action | Expected |
|---|---|---|---|
| `TestNeedsPreInit_GenesisCeremony` | SeiNode with genesisCeremony | needsPreInit | true |
| `TestBuildGenesisPreInitPlan` | Genesis node | buildGenesisPreInitPlan | 3 tasks: generate-identity, generate-gentx, upload-genesis-artifacts |
| `TestReconcilePending_GenesisDefersInitPlan` | Genesis node | reconcilePending | PreInit plan set, Init plan nil |
| `TestPreInitGate_WaitsForGenesisSource` | Genesis node, PreInit complete, genesis.s3 nil | handlePreInitComplete | Requeue 10s |
| `TestPreInitGate_ProceedsWhenS3Set` | Genesis node, PreInit complete, genesis.s3 set | handlePreInitComplete | Builds Init plan, cleans up, transitions |
| `TestPreInitGate_InitPlanHasDiscoverPeers` | Genesis node with static peers set | handlePreInitComplete | Init plan includes discover-peers with static source |
| `TestGenerateSeiNode_InjectsChainId` | Group with genesis, template without chainId | generateSeiNode | chainId injected |
| `TestGenerateSeiNode_InjectsGenesisCeremonyConfig` | Group with genesis | generateSeiNode | Full GenesisCeremonyNodeConfig populated |
| `TestGroupAssembly_WaitsForPreInit` | 4 nodes, 2 complete | reconcileGenesisAssembly | Requeue |
| `TestGroupAssembly_SubmitsOnAllComplete` | 4 nodes all complete, mock sidecar | reconcileGenesisAssembly | Assembly plan created, task submitted |
| `TestCollectPeers_BuildsPeerList` | 4 mock sidecars returning nodeIds | collectPeers | 4 entries in nodeId@dns:port format |
| `TestSetGenesisSource_StaticPeers` | SeiNode | setGenesisSource | genesis.s3 set, peers set as StaticPeerSource |
| `TestGroupAssembly_Timeout` | Created 20min ago, max 15m | reconcileGenesisAssembly | Error |
| `TestGroupAssembly_FailsOnPreInitFailure` | 1 node with PreInit Failed | reconcileGenesisAssembly | GenesisFailed condition, error |

### Integration Tests

| Test | Setup | Action | Expected |
|---|---|---|---|
| `TestGenesisNetworkE2E` | SeiNodeGroup replicas=4 | Wait for Running | 4 validators producing blocks |
| `TestGenesisNetworkDeletion` | Running network | Delete group | All resources cleaned up |

---

## Deployment

### Implementation Order

1. **sei-config: `GenesisDefaults()`** — Test-network genesis parameter defaults.
2. **seictl: `S3ObjectClient` interface** — Combined Get+Put for small files. Widen `DefaultS3ClientFactory` return type.
3. **seictl: `generate-identity` handler** — Runs seid init + keys add, writes identity.json, marker file. Standard `func(ctx, params) error`.
4. **seictl: `generate-gentx` handler** — Patches genesis, adds accounts, creates gentx, writes gentx.json, marker file.
5. **seictl: `upload-genesis-artifacts` handler** — Reads PVC files, uploads to S3 via `S3ObjectClient.PutObject`.
6. **seictl: `assemble-and-upload-genesis` handler** — Downloads all artifacts from S3 via `S3ObjectClient.GetObject`, runs seid collect-gentxs, uploads genesis.json via PutObject.
7. **seictl: `GET /v0/node-id` endpoint** — Thread `homeDir` into `Server`, derive node ID from `node_key.json` via CometBFT key loading, return as JSON. Add `GetNodeID` to `SidecarStatusClient` interface + concrete client implementation.
8. **sei-k8s-controller: CRD types** — `GenesisCeremonyConfig`, `GenesisCeremonyNodeConfig`, status fields. Run `make manifests`.
9. **sei-k8s-controller: Export `PreInitSidecarURL`** — From `node` package.
10. **sei-k8s-controller: Genesis PreInit Job** — `generateGenesisPreInitJob` branch in `generatePreInitJob` (avoids `snapshotSourceFor` nil-panic).
11. **sei-k8s-controller: Validator planner** — `needsPreInit`, `buildGenesisPreInitPlan`, task builders.
12. **sei-k8s-controller: PreInit gate** — `handlePreInitComplete` replaces both `isJobComplete` checkpoints for genesis nodes, deferred Init plan, guard both code paths.
13. **sei-k8s-controller: Group coordination** — `generateSeiNode` full genesis injection, `reconcileGenesisAssembly` with early failure detection, `AssemblyPlan`, `collectPeers`, `setGenesisSource`.

No seictl engine changes. No `TaskResult.Result` migration. No new peer source types. Steps 1-7 (seictl) and 8-13 (controller) can proceed in parallel.

---

## Decision Log

| # | Decision | Rationale | Reversibility |
|---|---|---|---|
| 1 | Engine stays stateless (no TaskResult.Result) | Tasks return success/fail only. Data flows through PVC + S3. Preserves the engine as a pure sequencer. Avoids inter-task output coupling and workflow-engine complexity. | Two-way |
| 2 | Dedicated genesis S3 bucket | Genesis artifacts in `sei-genesis-artifacts` (not snapshot buckets). Different lifecycle, simpler IAM: single bucket with Get+Put. Sidecar already has SDK + IRSA. | Two-way |
| 3 | Controller-pushed static peers | Group controller queries `/v0/node-id` on each sidecar after assembly, builds `nodeId@dns:port` peer list, pushes as `StaticPeerSource`. No new peer source types, no sidecar peer discovery changes, no S3 for peer data. Reuses existing `StaticPeerSource` + `discoverPeersTask` + `writePeersToConfig`. | Two-way |
| 4 | PreInit gate with deferred Init plan | Init plan built when gate clears (peers available). Prevents missing `discover-peers`. | Two-way |
| 5 | S3 URIs are deterministic (convention-based) | Controller constructs URIs from group name + genesis config. Never reads task output. | Two-way |
| 6 | Genesis as PreInit tasks | Reuses existing Job + sidecar infrastructure. | Two-way |
| 7 | Copy seid to PVC via init container | Sidecar needs seid. Future: `TaskExecutionInterface` eliminates this. | Two-way |
| 8 | `GenesisCeremonyConfig` naming | Distinguishes from existing `GenesisConfiguration`. | Two-way |
| 9 | ChainID injection in `generateSeiNode` | UX: users set `genesis.chainId`, not template.spec.chainId. | Two-way |
| 10 | `handlePreInitComplete` unified exit path | Both code paths share gate logic. No misleading halt-height log. | Two-way |
| 11 | Assembly as group `AssemblyPlan` | Task submission tracked in group status. Resumable on restart. | Two-way |

---

## Cross-Review Resolution Log (Round 4)

### Kubernetes Specialist (5 findings)

| # | Finding | Status | Resolution |
|---|---------|--------|-----------|
| 1 | PreInit sidecar liveness at `collectPeers` | OK | Genesis gate keeps all sidecars alive — `cleanupPreInit` only runs when `spec.genesis.s3` is set |
| 2 | `setGenesisSource` uses `r.Update` — stale resourceVersion risk | Low | Changed to `r.Patch(ctx, node, patch)` with `client.MergeFrom` |
| 3 | StatefulSet DNS shape `<name>-0.<name>.<ns>.svc` | OK | Correct for 1-pod-per-SeiNode StatefulSets |
| 4 | `buildPostBootstrapInitPlan` handles static peers | OK | Fixed state model diagram to match actual plan order (config-apply before discover-peers) |
| 5 | `listChildSeiNodes` unsorted — `nodes[0]` not deterministic | Medium | Added `sort.Slice` by name before assembly |

### Platform Engineer (4 findings)

| # | Finding | Status | Resolution |
|---|---------|--------|-----------|
| 1 | `Server` lacks `homeDir`; `node_key.json` stores private key, not nodeId | Spec gap | Added implementation notes: thread homeDir into Server, derive ID via CometBFT key loading, invariant on matching identity.json |
| 2 | `SidecarStatusClient` missing `GetNodeID` method | Gap | Documented as required interface addition + concrete client implementation |
| 3 | No orphaned `network-info.json` references | Clean | Confirmed |
| 4 | No orphaned `S3NetworkInfo` references | Clean | Confirmed |

---

## Deferred (Do Not Build)

| Feature | Rationale |
|---|---|
| **TaskResult.Result / typed task results** | Separate design deliverable — engine result routing is a fundamentally different problem |
| **TaskExecutionInterface** | Separate design deliverable — abstracts sidecar vs exec vs Job execution. Informs but doesn't block genesis. |
| **PreInit/Init phase merge** | Currently the phase boundary is Job→StatefulSet. Merging requires seid startup gating and changes the pod lifecycle model. Natural follow-up when TaskExecutionInterface provides the abstraction to make the boundary invisible. The current separation works, is well-tested, and doesn't create bloat. |
| **UpgradePolicy** | Separate design iteration |
| **Sidecar SQLite datastore** | Long-term persistence for task state |
| **Scale-up after genesis** | Requires on-chain governance |
| **Non-validator node types** | Start with validators |
| **Genesis from existing chain state** | New networks only |

---

## Sample Manifests

### Minimal Test Network

```yaml
apiVersion: sei.io/v1alpha1
kind: SeiNodeGroup
metadata:
  name: loadtest
spec:
  replicas: 4
  genesis:
    chainId: loadtest-1
  template:
    spec:
      image: 189176372795.dkr.ecr.us-east-2.amazonaws.com/sei/sei-chain:v5.0.0
      entrypoint:
        command: ["seid"]
        args: ["start", "--keyring-backend", "file", "--home", "/sei"]
      validator: {}
```

### Custom Parameters

```yaml
apiVersion: sei.io/v1alpha1
kind: SeiNodeGroup
metadata:
  name: custom-net
spec:
  replicas: 4
  genesis:
    chainId: custom-1
    stakingAmount: "50000000usei"
    accountBalance: "5000000000000usei,5000000000000uusdc"
    maxCeremonyDuration: "20m"
    overrides:
      "staking.params.unbonding_time": "300s"
      "gov.voting_params.voting_period": "120s"
    accounts:
      - address: "sei1loadtestfundingaddress..."
        balance: "999999999999usei"
    genesisS3:
      bucket: "sei-genesis-artifacts"
      prefix: "custom-1/custom-net/"
      region: "us-east-2"
  template:
    spec:
      image: 189176372795.dkr.ecr.us-east-2.amazonaws.com/sei/sei-chain:v5.0.0
      entrypoint:
        command: ["seid"]
        args: ["start", "--keyring-backend", "file", "--home", "/sei"]
      validator: {}
      overrides:
        "p2p.max_num_outbound_peers": "10"
```
