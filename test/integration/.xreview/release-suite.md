# xreview ledger — TestRelease + keygen refactor (WS-I)

Class: component (integration suite + internal package refactor + additive SDK surface)
Tier: T2

Target: `test/integration/release_test.go`, `internal/keygen/*`, `internal/seitask/keygen/keygen.go`, `sdk/sei` Node.REST()
Artifact: branch `feat/test-release`

## Round 1

State: RESOLVED
OpenFindings: 0
Convergence: independent (4 blinded reviewers)
Blinded: yes
Dissenter: sei-network-specialist (DISSENT → resolved)

Slate: sei-network-specialist (dissenter), systems-engineer, kubernetes-specialist, idiomatic-reviewer.

### Findings

| Finding | Status | Evidence | Raised by | Resolution |
|---|---|---|---|---|
| Dropped envFrom / RPC_EVM_RPC_LIST | **MISMATCH → FIXED** | Scenario injects env via `envFrom: workflow-vars CM` ∪ explicit list; the CM carries RPC_EVM_RPC_LIST (+ RPC_*/CHAIN_ID/ADMIN_ADDRESS) with no explicit equivalent. A harness sub-case reading it would skip silently → exit 0 false-pass. | dissenter (headline) | Job env now reproduces the scenario superset: the RPC_*/CHAIN_ID/ADMIN_ADDRESS CM names alongside the SEI_* explicit names. |
| Verdict is exit-0-only | **MISSING → FIXED** | No record of which sub-cases ran; scenario had upload-report (S3 audit). Strictly less observable than the artifact. | dissenter | Log the harness pod-log tail on completion (success too), so a skip-but-exit-0 is forensically visible. (Full S3/report = the deferred telemetry component.) |
| Job missing securityContext/resources/ttl | **MISMATCH → FIXED** | seiload_job.yaml.tmpl sets runAsNonRoot/seccomp/drop-ALL/readOnlyRootFS + resources + ttl; releaseJob set none → restricted-PSS admission could reject. | k8s | releaseJob now matches the seiload baseline (security context, resources, ttlSecondsAfterFinished). |
| REST handed unprobed (cold-start) | **MISSING → FIXED** | rest=="" is a status-string check, not a serve-probe; LCD binds later than the EVM listener → cold-REST window. | systems, k8s, dissenter | Added sei.WaitRESTServing (GET /cosmos/base/tendermint/v1beta1/node_info), symmetric with WaitEVMServing; replaces the bare empty-check. |
| waitJob drops podLogTail on ctx/signal | **MISMATCH → FIXED** | ctx.Done() branch failed with only ctx.Err() — no harness log on Ctrl-C/SIGTERM/timeout. | systems | ctx.Done() branch now tails the pod log (fresh ctx); messages genericized from "seiload job" to "job". |
| releaseBaseConfig handed un-cloned | **flag → FIXED** | provision maps.Clone's config; release passed the package-global directly. | systems | maps.Clone at the network create. |
| REST on by default for fullNode | COMPATIBLE | Smoke-confirmed (`...:1317` populated); ModeFull → REST.Enable=true, validators → false (explains the upgrade-suite gap). | dissenter (refutes own attack) | — |
| Secret material handling | COMPATIBLE | secretKeyRef (not plain env), never logged, Data not StringData. | systems | — |
| keygen refactor behavior | COMPATIBLE | Pure extraction verified line-by-line (entropy/path/pipeline/idempotency/ownerRef preserved). | k8s, idiom | — |
| Single RPC + EVM-legacy + funding + namespace | COMPATIBLE | One-node filter consistency correct; 1e12 usei matches scenario; co-located. | dissenter, k8s | — |
| Suite SA needs secrets RBAC | RESOLVED (Brandon-authorized) | createMnemonicSecret needs secrets create/delete; smoke confirmed Forbidden without it. | k8s, systems | Granted to the harness Role; the committed manifest lands in the cutover. |

### Idiom addendum (RATIFY)
Clean. The Go-built Job (vs seiload's template) is principled (shape owned by the suite, not platform) — do NOT harmonize. Node.REST() additive-safe, mirrors EVMRPC/TendermintRPC. Nits fixed: keygen.go:9 typo. SecretMnemonicKey placement vet-and-rejected (one shared const).

### Deferred
- Full upload-report / S3 audit trail = the deferred telemetry/report component (PromQL punted to last); the pod-log tail is the interim observability.
- Secret leak on SIGKILL until the label-GC sweep ships (cutover) — documented; mnemonic is for a throwaway chain (DeletionDelete cascade).
