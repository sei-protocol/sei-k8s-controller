# Execution Plan: Genesis Network Configuration

**Source:** [network-deployments.md](./network-deployments.md)
**Created:** 2026-03-25

---

## Work Streams

Two parallel work streams plus a platform prerequisite.

```
PR 0: Terraform (platform)
  ├── S3 bucket: sei-genesis-artifacts
  └── IAM: s3:GetObject + s3:PutObject on sei-genesis-artifacts/*

Stream A (seictl)                  Stream B (sei-k8s-controller)
─────────────────                  ─────────────────────────────
PR 1: S3ObjectClient               PR 5: CRD types + manifests
       ↓                                  ↓
PR 2: Genesis task handlers         PR 6: PreInit Job + planner
       ↓                                  ↓
PR 3: Assembly handler              PR 7: PreInit gate
       ↓                                  ↓
PR 4: /v0/node-id endpoint          PR 8: Group coordination
       ↓                                  ↓
       └──────────── PR 9: E2E ────────────┘
```

---

## Prerequisite: Platform

### PR 0 — S3 bucket + IAM policy (Terraform)

**Scope:** Create the genesis artifact bucket and grant the `seid-node` ServiceAccount read+write access.

**Files:**
- `platform/terraform/aws/189176372795/us-east-2/dev/eks.tf` — new IAM statement + S3 bucket
- Equivalent for any other environments (staging, prod) as needed

**Changes:**

S3 bucket:
```hcl
resource "aws_s3_bucket" "genesis_artifacts" {
  bucket = "sei-genesis-artifacts"
  tags   = local.tags
}
```

IAM statement added to `aws_iam_policy.seid_node`:
```hcl
{
  Sid      = "GenesisArtifacts"
  Effect   = "Allow"
  Action   = ["s3:GetObject", "s3:PutObject"]
  Resource = "arn:aws:s3:::sei-genesis-artifacts/*"
}
```

**Why this is a prerequisite:**
The current `seid-node` IAM policy only has `s3:PutObject` on `sei-node-mvp/shadow-results/*` (for result export). Genesis handlers upload to a different bucket entirely. Without this change, `upload-genesis-artifacts` and `assemble-and-upload-genesis` fail with `AccessDenied` at runtime even if the code is correct.

**Acceptance:**
- [ ] Bucket exists in target account
- [ ] `seid-node` SA can `PutObject` and `GetObject` to `sei-genesis-artifacts/*`
- [ ] Existing snapshot download and result export permissions unchanged

**Dependencies:** None — can land immediately
**Estimated effort:** Small (< 1 day, mostly review + apply)

---

## Stream A: seictl (Sidecar)

### PR 1 — S3ObjectClient interface

**Scope:** New `S3ObjectClient` interface with Get+Put for small files.

**Files:**
- `seictl/sidecar/tasks/s3.go` (new) — `S3ObjectClient` interface, `S3ObjectClientFactory` type, `DefaultS3ObjectClientFactory` using `s3.NewFromConfig`
- `seictl/sidecar/tasks/s3_test.go` (new) — factory returns client satisfying interface

**Changes:**
- Define `S3ObjectClient` with `GetObject` + `PutObject` (both from `aws-sdk-go-v2/service/s3`)
- `DefaultS3ObjectClientFactory` loads config with region, returns `*s3.Client` (natively satisfies both)
- Existing `S3GetObjectAPI` / `S3ClientFactory` in `genesis.go` are left in place (no migration)

**Acceptance:**
- [ ] Interface compiles with `*s3.Client` as implementation
- [ ] Factory returns working client in unit test with mocked config

**Estimated effort:** Small (< 1 day)

---

### PR 2 — Genesis task handlers (generate-identity, generate-gentx, upload-genesis-artifacts)

**Scope:** Three new task handlers + TaskType constants + client-side TaskBuilders.

**Files:**
- `seictl/sidecar/engine/types.go` — add 3 `TaskType` constants
- `seictl/sidecar/tasks/genesis_ceremony.go` (new) — `IdentityGenerator`, `GentxGenerator`, `ArtifactUploader` structs + `Handler()` methods
- `seictl/sidecar/tasks/genesis_ceremony_test.go` (new) — unit tests per design spec
- `seictl/sidecar/client/tasks.go` — add `GenerateIdentityTask`, `GenerateGentxTask`, `UploadGenesisArtifactsTask` builders
- `seictl/cmd/serve.go` — register handlers in engine `handlers` map

**Pre-implementation: seid CLI contract verification**

Before writing handler code, run the exact command sequence manually against the target seid Docker image and document:
1. `seid init` — directory structure, output format, `node_key.json` location
2. `seid keys add --output json` — JSON schema for address/pubkey extraction
3. `seid add-genesis-account` — exact positional arg syntax for the target SDK version
4. `seid gentx` — flag names (e.g., `--amount` vs positional), keyring flags
5. `seid collect-gentxs` — expected input directory, output genesis.json location

This becomes the test fixture and the contract the handlers code against. The seid binary comes from the node image (version-matched via `copy-seid` init container), but the handler code must know the output format at compile time.

Key concern: `seid add-genesis-account` syntax changed in Cosmos SDK v0.47+ (moved to `genesis add-genesis-account` subcommand). Verify which pattern the target seid version uses.

**Handler details:**

`generate-identity`:
1. Check marker `.sei-sidecar-identity-done` → return nil if exists
2. Run `seid init --chain-id <chainId> --home <homeDir> --moniker <moniker>`
3. Run `seid keys add <moniker> --keyring-backend file --home <homeDir>`
4. Parse key output, read `config/node_key.json` for nodeId
5. Write `$SEI_HOME/.genesis/identity.json` with `{nodeId, accountAddress, consensusPubkey}`
6. Write marker

`generate-gentx`:
1. Check marker `.sei-sidecar-gentx-done` → return nil if exists
2. Load `GenesisDefaults()` from sei-config, merge with `genesisParams` overrides
3. Write patched genesis to `config/genesis.json`
4. For each `accountBalance`, run `seid add-genesis-account`
5. Run `seid gentx <moniker> <stakingAmount> --chain-id <chainId> --keyring-backend file`
6. Write `$SEI_HOME/.genesis/gentx.json` (copy of gentx file)
7. Write marker

`upload-genesis-artifacts`:
1. Check marker `.sei-sidecar-upload-artifacts-done` → return nil if exists
2. Read `.genesis/identity.json` and `.genesis/gentx.json`
3. `PutObject` to `s3://<bucket>/<prefix>/<nodeName>/identity.json`
4. `PutObject` to `s3://<bucket>/<prefix>/<nodeName>/gentx.json`
5. Write marker

**Acceptance:**
- [ ] `TestGenerateIdentity_HappyPath` — identity.json written, marker created
- [ ] `TestGenerateIdentity_Idempotent` — marker exists, no seid calls
- [ ] `TestGenerateGentx_HappyPath` — gentx.json written, genesis patched
- [ ] `TestGenerateGentx_Idempotent` — marker exists, no seid calls
- [ ] `TestUploadGenesisArtifacts_HappyPath` — 2 PutObject calls at correct keys
- [ ] `TestUploadGenesisArtifacts_Idempotent` — marker exists, no S3 calls
- [ ] All three TaskBuilders serialize/validate correctly

**Dependencies:** PR 1 (S3ObjectClient for upload handler)
**Estimated effort:** Medium (2-3 days)

---

### PR 3 — Assembly handler (assemble-and-upload-genesis)

**Scope:** Group-level assembly task that runs on node 0's sidecar.

**Files:**
- `seictl/sidecar/engine/types.go` — add `TaskAssembleAndUploadGenesis` constant
- `seictl/sidecar/tasks/genesis_ceremony.go` — add `GenesisAssembler` struct + `Handler()`
- `seictl/sidecar/tasks/genesis_ceremony_test.go` — assembly tests
- `seictl/sidecar/client/tasks.go` — add `AssembleAndUploadGenesisTask` builder with `GenesisNodeParam`
- `seictl/cmd/serve.go` — register handler

**Handler details:**
1. Check marker `.sei-sidecar-assemble-genesis-done` → return nil if exists
2. For each node in `nodes[]` param:
   - `GetObject` from `s3://<bucket>/<prefix>/<node.name>/identity.json`
   - `GetObject` from `s3://<bucket>/<prefix>/<node.name>/gentx.json`
   - Write gentx to `$SEI_HOME/config/gentx/`
   - Run `seid add-genesis-account` for each validator's account
3. Run `seid collect-gentxs --home <homeDir>`
4. Hash genesis.json (SHA-256)
5. `PutObject` assembled genesis to `s3://<bucket>/<prefix>/genesis.json`
6. Write marker

**Acceptance:**
- [ ] `TestAssembleAndUploadGenesis_HappyPath` — genesis.json uploaded, marker created
- [ ] `TestAssembleAndUploadGenesis_Idempotent` — marker exists, no S3/seid calls

**Dependencies:** PR 1 (S3ObjectClient), PR 2 (identity.json format)
**Estimated effort:** Medium (2-3 days)

---

### PR 4 — /v0/node-id endpoint + SidecarStatusClient extension

**Scope:** Read-only HTTP endpoint + client interface addition.

**Files:**
- `seictl/sidecar/server/server.go` — extend `Server` to accept `homeDir`, add `handleNodeID`
- `seictl/sidecar/server/server_test.go` — endpoint tests
- `seictl/cmd/serve.go` — pass `homeDir` to `NewServer`
- `seictl/sidecar/client/client.go` — add `GetNodeID(ctx) (string, error)` method
- `sei-k8s-controller/internal/controller/node/plan_execution.go` — add `GetNodeID` to `SidecarStatusClient` interface

**Implementation:**
- `NewServer(addr, eng, homeDir)` — thread homeDir
- Handler reads `$homeDir/config/node_key.json`, derives node ID via CometBFT `p2p.LoadNodeKey()` (or equivalent Ed25519 pubkey → address derivation)
- Returns `{"nodeId": "abc123..."}`

**Acceptance:**
- [ ] `TestNodeIDEndpoint_ReturnsNodeId` — node_key.json on disk, returns 200 with correct nodeId
- [ ] `TestNodeIDEndpoint_NoKeyFile` — no file, returns error
- [ ] `SidecarStatusClient` compiles with new method
- [ ] Client `GetNodeID` makes correct HTTP call

**Dependencies:** None (can start in parallel with PR 2-3)
**Estimated effort:** Small (1 day)

---

## Stream B: sei-k8s-controller

### PR 5 — CRD types + manifests

**Scope:** New Go types, validation, generated manifests.

**Files:**
- `api/v1alpha1/seinodegroup_types.go` — add `GenesisCeremonyConfig`, `GenesisS3Destination`, `GenesisAccount` to `SeiNodeGroupSpec`; add `AssemblyPlan`, `GenesisHash`, `GenesisS3URI` to status
- `api/v1alpha1/seinode_types.go` — add `GenesisCeremonyNodeConfig`, `AccountBalance` to `ValidatorSpec`
- `api/v1alpha1/common_types.go` — no changes (existing `GenesisS3Source` reused)
- Run `make manifests generate`

**Acceptance:**
- [ ] `make manifests` succeeds
- [ ] `make generate` succeeds
- [ ] CRD YAML includes new fields with correct validation markers
- [ ] CEL rules unchanged on `PeerSource`

**Dependencies:** None
**Estimated effort:** Small (1 day)

---

### PR 6 — Genesis PreInit Job + planner

**Scope:** Branch in `generatePreInitJob`, genesis PreInit plan, task builders.

**Files:**
- `internal/controller/node/job.go` — `generateGenesisPreInitJob` (branch from `generatePreInitJob`)
- `internal/controller/node/planner_validator.go` — `isGenesisCeremonyNode`, update `Validate`
- `internal/controller/node/planner_bootstrap.go` — `buildGenesisPreInitPlan`
- `internal/controller/node/planner.go` — update `needsPreInit` for genesis
- `internal/controller/node/task_builders.go` — `generateIdentityTask`, `generateGentxTask`, `uploadGenesisArtifactsTask`
- `internal/controller/node/controller.go` — genesis branch in `reconcilePending` (defer Init plan)
- Tests: `resources_test.go`, `plan_execution_test.go`

**Key implementation details:**

`generateGenesisPreInitJob`:
- Init container: `copy-seid` — copies seid binary from node image to PVC at `$dataDir/bin/seid`
- Sidecar container: `seictl serve` (restartable init container)
- Main container: `sleep infinity` keepalive
- Same IRSA service account as snapshot PreInit
- No `snapshotSourceFor` call

`reconcilePending` genesis branch:
```go
if isGenesisCeremonyNode(node) {
    node.Status.PreInitPlan = buildGenesisPreInitPlan()
    // Init plan deferred — peers and genesis source not yet available
} else { ... }
```

**Acceptance:**
- [ ] `TestNeedsPreInit_GenesisCeremony` — returns true
- [ ] `TestBuildGenesisPreInitPlan` — 3 tasks in correct order
- [ ] `TestReconcilePending_GenesisDefersInitPlan` — PreInit set, Init nil
- [ ] `TestGenerateGenesisPreInitJob_NoPanic` — no nil dereference from missing snapshot
- [ ] Genesis PreInit Job pod spec has copy-seid init container, sleep infinity main, sidecar

**Dependencies:** PR 5 (CRD types)
**Estimated effort:** Medium (2-3 days)

---

### PR 7 — PreInit gate (handlePreInitComplete)

**Scope:** Unified PreInit completion handler that bypasses `isJobComplete` for genesis.

**Files:**
- `internal/controller/node/pre_init.go` — extract `handlePreInitComplete`, refactor both `isJobComplete` checkpoints
- Tests: `plan_execution_test.go`

**Key implementation details:**

In `reconcilePreInitializing`, when `plan.Phase == Complete`:
- If `isGenesisCeremonyNode(node)` → route to `handlePreInitComplete` (skip `isJobComplete`)
- Otherwise → existing halt-height wait logic

`handlePreInitComplete` for genesis:
1. If `node.Spec.Genesis.S3 == nil` → requeue 10s (waiting for group)
2. If set → `buildPostBootstrapInitPlan(node)` (static peers are on `spec.validator.peers`)
3. `cleanupPreInit` → delete Job + headless service
4. Transition to `PhaseInitializing`

Must handle BOTH code paths in `reconcilePreInitializing` — the early return at line 26-33 AND the post-`executePlan` check at line 104-108.

**Acceptance:**
- [ ] `TestPreInitGate_WaitsForGenesisSource` — requeue when genesis.s3 nil
- [ ] `TestPreInitGate_ProceedsWhenS3Set` — builds Init plan, cleans up, transitions
- [ ] `TestPreInitGate_InitPlanHasDiscoverPeers` — static peers → discover-peers in Init plan
- [ ] Snapshot PreInit flow unchanged (no regression)

**Dependencies:** PR 6 (PreInit Job + planner)
**Estimated effort:** Medium (1-2 days)

---

### PR 8 — Group coordination

**Scope:** Full genesis orchestration in the SeiNodeGroup controller.

**Files:**
- `internal/controller/nodegroup/nodes.go` — update `generateSeiNode` with genesis injection
- `internal/controller/nodegroup/genesis.go` (new) — `reconcileGenesisAssembly`, `collectPeers`, `setGenesisSource`, `buildAssemblyPlan`, `executeAssemblyPlan`, `genesisS3Config`
- `internal/controller/nodegroup/controller.go` — add `BuildSidecarClientFn`, call `reconcileGenesisAssembly` in reconcile loop
- `internal/controller/node/plan_execution.go` — export `PreInitSidecarURL`
- Tests: `nodes_test.go`, `genesis_test.go` (new)

**Key implementation details:**

`generateSeiNode` genesis injection:
- Set `spec.chainId` from `genesis.chainId` if template omits it
- Populate full `GenesisCeremonyNodeConfig` (chainId, stakingAmount, accountBalances, genesisParams, index, artifactS3)

`reconcileGenesisAssembly`:
1. Sort nodes by name
2. Timeout check
3. Early failure detection (PreInit Failed)
4. Wait for all PreInit complete
5. Build/resume `AssemblyPlan` in group status
6. Execute assembly plan against node 0's sidecar
7. `collectPeers` — query `/v0/node-id` on each sidecar, build `nodeId@dns:port`
8. `setGenesisSource` — patch each SeiNode with `spec.genesis.s3` + `spec.validator.peers` (static)
9. Record completion in group status

**Acceptance:**
- [ ] `TestGenerateSeiNode_InjectsChainId`
- [ ] `TestGenerateSeiNode_InjectsGenesisCeremonyConfig`
- [ ] `TestGroupAssembly_WaitsForPreInit` — 2 of 4 complete → requeue
- [ ] `TestGroupAssembly_SubmitsOnAllComplete` — assembly plan created
- [ ] `TestCollectPeers_BuildsPeerList` — 4 entries in nodeId@dns:port
- [ ] `TestSetGenesisSource_StaticPeers` — genesis.s3 + peers set correctly
- [ ] `TestGroupAssembly_Timeout` — error on expiry
- [ ] `TestGroupAssembly_FailsOnPreInitFailure` — GenesisFailed condition

**Dependencies:** PR 5 (CRD types), PR 6 (planner), PR 7 (gate). Also requires PR 4 (node-id endpoint) for the `SidecarStatusClient.GetNodeID` interface.
**Estimated effort:** Large (3-4 days)

---

### PR 9 — Integration (E2E)

**Scope:** End-to-end integration test + sei-config dependency.

**Files:**
- `sei-config` — `GenesisDefaults()` function (may be its own PR in sei-config repo)
- Integration test in sei-k8s-controller or a dedicated test harness

**Acceptance:**
- [ ] `TestGenesisNetworkE2E` — SeiNodeGroup replicas=4, all reach Running, produce blocks
- [ ] `TestGenesisNetworkDeletion` — clean deletion of all resources

**Dependencies:** All prior PRs merged
**Estimated effort:** Medium (2-3 days)

---

## Dependency Graph

```
PR 0: Terraform (land first — blocks E2E testing)

sei-config: GenesisDefaults()  (can start immediately, needed by PR 2)

Stream A:  PR 1 → PR 2 → PR 3
                              \
           PR 4 (parallel) ────→ PR 9 (E2E, requires PR 0 applied)
                              /
Stream B:  PR 5 → PR 6 → PR 7 → PR 8
```

**Critical path:** PR 0 (Terraform) + PR 5 → PR 6 → PR 7 → PR 8 → PR 9
**Parallelism:** Stream A and Stream B run concurrently. PR 0 and PR 4 can start immediately.

---

## Estimated Timeline

| Week | Stream A (seictl) | Stream B (controller) |
|------|-------------------|----------------------|
| 1 | PR 1 (S3ObjectClient) + PR 2 (task handlers) | PR 5 (CRD types) + PR 6 (PreInit Job) |
| 2 | PR 3 (assembly handler) + PR 4 (node-id endpoint) | PR 7 (PreInit gate) + PR 8 (group coordination) |
| 3 | — | PR 9 (E2E integration) |

**Total:** ~3 weeks with two parallel contributors, ~4-5 weeks single-threaded.

---

## Risk Register

| Risk | Impact | Likelihood | Mitigation |
|------|--------|------------|-----------|
| `seid` CLI syntax differs from expected contract | Handler fails to parse output or execute commands | Medium | **Pre-implementation CLI verification** (added to PR 2). Run exact commands against target seid image. Use `--output json` where available. Key concern: `add-genesis-account` syntax changed in Cosmos SDK v0.47+ (`genesis add-genesis-account` subcommand). |
| IAM policy missing `s3:PutObject` on genesis bucket | `upload-genesis-artifacts` and `assemble-and-upload-genesis` fail with AccessDenied | **Confirmed** — current policy lacks this | **PR 0 (Terraform)** creates `sei-genesis-artifacts` bucket + IAM grant. Must land before E2E testing. |
| PVC RWO handoff timing on cleanup | StatefulSet pod can't mount PVC after PreInit Job deletion | Low | `cleanupPreInit` already polls for Job full deletion before proceeding; no change needed |
| Large genesis (many validators) exceeds ceremony timeout | Genesis ceremony fails | Low | `maxCeremonyDuration` is configurable; default 15m is generous for ≤50 validators |
| sei-config `GenesisDefaults()` not ready when PR 2 starts | PR 2 blocked on external dependency | Low | Use inline defaults initially; swap to sei-config when ready (the function is small) |
| Pod identity namespace binding (`default` only) | Genesis nodes in non-default namespace get no AWS credentials | Low | Current setup uses `default` for all workloads; document the constraint |
