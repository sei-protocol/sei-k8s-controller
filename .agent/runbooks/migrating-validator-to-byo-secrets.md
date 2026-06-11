# Migrating a Validator onto the Platform with BYO Consensus Secrets

**Audience:** operators cutting a live validator over from a legacy host (e.g. an `sei-infra` EC2 validator) onto the Kubernetes platform, carrying the validator's existing consensus identity via Kubernetes Secrets — the "bring-your-own-secrets" path. Written against the arctic-1 node-19 cutover but applies to any validator migration.
**Scope:** the consensus identity that must move, the SND spec shape, the controller validation surface, the **stop-before-start double-sign discipline** and the layered defenses against equivocation, the cutover sequence, rollback, and the operational gotchas surfaced by the harbor dry-run.
**Not in scope:** *why* the BYO-secrets surface exists or its controller internals — see the `validator.signingKey` / `nodeKey` / `operatorKeyring` types in `api/v1alpha1/validator_types.go`. Genesis-ceremony bring-up (keys minted on-cluster) is a different path — this runbook is for an **existing** identity moving hosts.

> **The one rule that matters:** a consensus key may be live on exactly one host at a time. Everything below exists to guarantee the legacy signer is **fully stopped** before the platform signer starts. Two hosts signing the same key at the same height is equivocation — it tombstones the validator **permanently** (a new consensus key is the only recovery).

---

## 1. Success criteria

The migration is complete when **all** of the following hold:

| Check | State |
|---|---|
| Legacy host `seid` | stopped **and** disabled (no auto-restart on reboot); key material removed from the host post-soak |
| `Secret/<name>-signing-key` | present, `data."priv_validator_key.json"` decrypts to the validator's key (address matches the legacy node) |
| `Secret/<name>-node-key` | present, `data."node_key.json"` decrypts |
| `SeiNode.status.conditions[SigningKeyReady]` | `True`, `Reason=SigningKeyValidated` |
| `SeiNode.status.conditions[NodeKeyReady]` | `True`, `Reason=NodeKeyValidated` |
| `SeiNode.status.plan` | reaches `mark-ready=Complete`, then clears (node → `Running`) |
| `SeiNodeDeployment` | `READY`. NB: for a no-genesis single validator the SND itself builds **no** plan — its `status.plan` stays nil; it reaches READY via child-node readiness, not its own plan |
| seid main container | `catching_up: false`, height climbing |
| seid logs | `signed and pushed vote ... <ADDR>` at the chain tip — signing under the **migrated** address |
| `cosmos_validators_jailed{...}` for this valoper | `0` (not jailed) |
| `tendermint_consensus_byzantine_validators{chain_id=...}` | `0` (no equivocation evidence) |

If `cosmos_validators_jailed` flips to `1` **with a tombstone**, a double-sign occurred — the identity is dead. Do not unjail; the validator needs a new consensus key. See §4.

---

## 2. What actually migrates

A validator's on-chain identity is **two files**, nothing else:

| File | Secret data key | Purpose | Migrate? |
|---|---|---|---|
| `priv_validator_key.json` | `priv_validator_key.json` | the **consensus signing key** — the thing slashing protects | **Yes — this IS the identity** |
| `node_key.json` | `node_key.json` | libp2p/Tendermint **node (P2P) identity** | Yes (keeps the NodeID stable; a fresh one also works) |
| operator/account keyring | (`operatorKeyring.secret`, optional) | signs `MsgEditValidator`, withdraw-commission, gov votes | Optional — only if the node should sign operator txs itself (see [Operator account keyring](#operator-account-keyring-optional--on-node-governance-signing)). Absent for a pure consensus validator |

Chain state (the data PVC) does **not** migrate — the platform node re-syncs. Identity lives entirely in the two Secrets, so losing/rebuilding the PVC is recoverable; losing key custody is not.

### Extract + encrypt

The KMS key is chosen by **which cluster directory the secret lives in**, not by repo — `sops` discovers the `.sops.yaml` nearest the file (CWD-upward). The platform repo has **no root `.sops.yaml`**; each cluster carries its own:

| Secret lands under | `.sops.yaml` | KMS key | Region |
|---|---|---|---|
| `clusters/prod/...` | `clusters/prod/.sops.yaml` | `alias/prod` | eu-central-1 |
| `clusters/dev/...` | `clusters/dev/.sops.yaml` | `alias/dev` | **us-east-2** |

Both use `path_regex: .*` (every file under the dir) and `encrypted_regex: ^(data|stringData|.+=)$`. The arctic-1 prod migration lands under `clusters/prod/arctic-1/` → `alias/prod`. (Two notes: the platform repo has **no** `clusters/harbor/.sops.yaml` — in-repo harbor secrets are encrypted with an explicit `--kms alias/harbor`, not an auto-discovered rule; and the separate harbor *engineering-workspace* repo is its own world, using a `*.enc.yaml` path_regex + `alias/harbor` — that's where the dry-run ran, not the platform repo.)

```bash
# From the legacy host (or its backup of the config dir):
#   $SEID_HOME/config/priv_validator_key.json
#   $SEID_HOME/config/node_key.json

# Build the Secret manifests UNDER the target cluster dir. Filenames follow the
# repo's *.secret.yaml convention; metadata.name is arctic-1-node-19-*. The
# data keys must be EXACTLY priv_validator_key.json / node_key.json — the
# controller's validate-signing-key / validate-node-key tasks look them up.
cd clusters/prod/arctic-1     # so sops auto-discovers clusters/prod/.sops.yaml
kubectl create secret generic arctic-1-node-19-signing-key -n arctic-1 \
  --from-file=priv_validator_key.json=/path/to/priv_validator_key.json \
  --dry-run=client -o yaml > node-19-signing-key.secret.yaml
kubectl create secret generic arctic-1-node-19-node-key -n arctic-1 \
  --from-file=node_key.json=/path/to/node_key.json \
  --dry-run=client -o yaml > node-19-node-key.secret.yaml

# Encrypt in place before the files ever touch git. With CWD inside
# clusters/prod/, sops finds clusters/prod/.sops.yaml automatically (or pass
# --config clusters/prod/.sops.yaml from elsewhere):
AWS_PROFILE=sei sops -e -i node-19-signing-key.secret.yaml
AWS_PROFILE=sei sops -e -i node-19-node-key.secret.yaml

# VERIFY before commit: values are ENC[AES256_GCM,...], a sops: block is
# present (kms arn → alias/prod), and NO plaintext key fragment survives.
grep -E "priv_validator_key.json:|node_key.json:|sops:|alias/" *.secret.yaml   # → ENC[...]
AWS_PROFILE=sei sops -d node-19-signing-key.secret.yaml | grep priv_validator_key.json  # round-trips
```

**A plaintext `priv_validator_key.json` must never reach git, logs, or a terminal that's scrollback-captured.** Treat the extraction host as sensitive.

### Operator account keyring (optional — on-node governance signing)

Mount this **only** if the node should sign operator txs itself — governance/upgrade votes, `MsgEditValidator`, withdraw-commission. The validator signs blocks and runs fine without it; the operator key is otherwise used off-node. (node-19 mounts it so upgrade votes can be cast from the node.)

**Security tradeoff — decide before mounting.** The operator account key controls the validator's *treasury and governance* (withdraw-commission, move self-stake, change withdraw address, vote, edit, unjail) — a strictly larger blast radius than the consensus key, which only risks slashing. The pod runs `shareProcessNamespace: true`, so a compromise of the internet-exposed seid container can read the unlocked key out of sidecar memory. **If you only need voting, the safer pattern is an `authz` `MsgVote`-only grant to a dedicated low-privilege key** — mount *that*, keep the full operator key offline; vote inheritance still attributes to the operator account (`MsgExec` sets `voter = granter`). Mount the full key only with eyes open.

**1. Identify the operator account on-chain.** It's distinct from the consensus key. Resolve it from the consensus pubkey (the public `pub_key.value` in `priv_validator_key.json`), then check whether authority is delegated (`REST=https://rest-arctic-1.sei-apis.com`):

```bash
# consensus pubkey -> on-chain validator -> operator (valoper); the operator ACCOUNT (sei1…) shares the valoper's address bytes
curl -s "$REST/cosmos/staking/v1beta1/validators?pagination.limit=300" \
  | jq -r --arg pk "<pub_key.value>" '.validators[] | select(.consensus_pubkey.key==$pk) | "\(.description.moniker)\t\(.operator_address)"'
curl -s "$REST/cosmos/authz/v1beta1/grants/granter/<operator-sei1>"                       # delegated authority? (empty = none)
curl -s "$REST/cosmos/distribution/v1beta1/delegators/<operator-sei1>/withdraw_address"   # redirected?
```
A non-empty authz grant or a redirected withdraw address means operator authority is delegated/elsewhere — account for it before assuming this keyring is the whole story.

**2. Convert `admin_key.json` → file-backend keyring.** Sei's `admin_key.json` is a `seid keys add --output json` export (`address`/`mnemonic`/`name`/`pubkey`/`type`). `operatorKeyring` needs a *file keyring* (`<keyName>.info` + `<hex>.address` + `keyhash`), not the raw json — import it on the host (touches the private key):

```bash
KR=$(mktemp -d); PASS=$(openssl rand -base64 32); echo "SAVE: $PASS"
jq -r .address admin_key.json    # gate: must equal the operator account from step 1

# `seid keys add --recover` reads BOTH the mnemonic AND the passphrase from stdin —
# feed all three lines or the passphrase prompts hit EOF ("too many failed attempts"):
printf '%s\n%s\n%s\n' "$(jq -r .mnemonic admin_key.json)" "$PASS" "$PASS" \
  | seid keys add node_admin --recover --keyring-backend file --keyring-dir "$KR"

seid keys show node_admin -a --keyring-backend file --keyring-dir "$KR"   # == operator account, or STOP
ls "$KR/keyring-file"            # node_admin.info  <hex>.address  keyhash
```

**3. Two SOPS Secrets (keyring + passphrase, kept distinct).** Use `data:` (base64), **not** `stringData` — `.info`/`keyhash` are exact-byte sensitive (a YAML block-scalar newline breaks passphrase verification, unlike the JSON keys above which tolerate it):

```yaml
# node-19-operator-keyring.secret.yaml — data keys ARE the file-keyring filenames
data:
  node_admin.info: <base64 -w0 "$KR/keyring-file/node_admin.info">
  <hex>.address:   <base64 -w0 "$KR/keyring-file/<hex>.address">
  keyhash:         <base64 -w0 "$KR/keyring-file/keyhash">
# node-19-operator-passphrase.secret.yaml
data:
  passphrase: <printf '%s' "$PASS" | base64 -w0>
```
`sops -e -i` both from the cluster dir, confirm `ENC[`, then validate the encoding decodes to the right shapes before committing:
```bash
yq -r '.data."node_admin.info"' …keyring.secret.yaml | base64 -d | cut -c1-3   # eyJ  (JWE)
yq -r '.data.keyhash'            …keyring.secret.yaml | base64 -d | cut -c1-3   # $2a  (bcrypt)
rm -rf "$KR"                                                                    # wipe cleartext keyring
```
(Encryption is **not** done until `sops -e -i` runs — base64 alone is not encryption. Don't commit a file whose `data` values aren't `ENC[…]`.)

**4. Wire into the SND** — four DISTINCT secretNames (CEL-enforced: keyring ≠ passphrase ≠ signing ≠ node):
```yaml
validator:
  operatorKeyring:
    secret:
      secretName: arctic-1-node-19-operator-keyring
      keyName: node_admin
      passphraseSecretRef:
        secretName: arctic-1-node-19-operator-passphrase
        key: passphrase
```
Controller ≥ `5d03f33` runs `validate-operator-keyring` on bring-up. On the node, vote with: `seid tx gov vote <id> <option> --from node_admin --keyring-backend file --keyring-dir $SEI_HOME/keyring-file`.

---

## 3. SND spec shape

The migrated validator is `signingKey` + `nodeKey`, no genesis ceremony, no snapshot (block-syncs):

```yaml
apiVersion: sei.io/v1alpha1
kind: SeiNodeDeployment
metadata:
  name: node-19
  namespace: <ns>
spec:
  replicas: 1                 # LOAD-BEARING — see §4. The CEL guard rejects >1.
  deletionPolicy: Delete      # NB: deleting the SeiNode cascades to the data PVC — see §6 finding 3
  template:
    metadata:
      labels:
        sei.io/chain: arctic-1
        sei.io/role: validator
    spec:
      chainId: arctic-1
      image: <ecr>/sei/sei-chain:<tag>
      overrides:
        storage.state_commit.write_mode: <mode>   # MUST match the image — see §6 finding 0
      peers:
        - label:
            namespace: <ns>
            selector:
              sei.io/chain: arctic-1
      validator:
        signingKey:
          secret:
            secretName: node-19-signing-key   # data key: priv_validator_key.json
        nodeKey:
          secret:
            secretName: node-19-node-key       # data key: node_key.json
        # operatorKeyring: OMITTED — independently optional; only set if the
        # operator account key is sourced from a Secret.
  networking:
    tcp: {}                   # per-pod internet-facing NLB for a public validator — see §6 finding 1
```

### Controller validation surface

The bootstrap plan gates on the key Secrets **before** any StatefulSet mutation, so a missing/malformed Secret fails controller-side with a clear condition instead of a kubelet mount error:

| Task | Requires | Failure condition |
|---|---|---|
| `validate-signing-key` | Secret exists; data key `priv_validator_key.json` is valid Tendermint JSON (`address`, `pub_key.{type,value}`, `priv_key.{type,value}`) | `SigningKeyReady=False/SigningKeyInvalid` (terminal) |
| `validate-node-key` | Secret exists; data key `node_key.json` has `priv_key.{type,value}` | `NodeKeyReady=False/NodeKeyInvalid` (terminal) |
| `validate-operator-keyring` | **only planned when `operatorKeyring` is set** | n/a when unset |

> Requires controller image **`5d03f33` or a known-later build** (sei-k8s-controller#380, shipped to platform via platform#826). Earlier builds validated `operatorKeyring` unconditionally in the *update* plan, so a `signingKey`+`nodeKey` validator with no `operatorKeyring` bricked on its first image bump with `validate-operator-keyring: secretName is empty`. Confirm the running image before migrating — this is a human check that the tag is `5d03f33` (or later per platform#826), not a string comparison: `kubectl -n sei-k8s-controller-system get deploy sei-k8s-controller-manager -o jsonpath='{.spec.template.spec.containers[*].image}'`.

---

## 4. Double-sign safety — the core of the procedure

Equivocation = the same consensus key signing two different things at one height. The chain records permanent evidence and **tombstones** the validator (jail + permanent ban; the key can never validate again). The migration's entire risk surface is the window where both the legacy signer and the platform signer could be live.

**Three layers of defense, in order of when they act:**

1. **Procedure (primary, preventive).** Stop-before-start: the legacy `seid` is stopped *and disabled* and confirmed not signing **before** the platform SND is applied/unpaused. This is the only layer that *prevents* the double-sign. The others only *catch* it.
2. **CEL admission guard (preventive, in-cluster case).** A `signingKey` validator is pinned to `replicas: 1` at admission — every replica would mount the same `priv_validator_key.json`, so >1 is an instant in-cluster equivocation. Rejection message:
   > `a validator with a signingKey must have replicas: 1 — every replica mounts the same priv_validator_key.json, so >1 replica double-signs (equivocation) and tombstones/slashes the validator`

   *Verified live:* `kubectl apply --dry-run=server` of a `signingKey` SND with `replicas: 2` is rejected with the above; `replicas: 1` is accepted. This guard does **not** protect against a key live on a *different host* (the legacy EC2) — only layer 1 does.
3. **Alerting (detective, last resort).** Two platform alerts (sei-protocol/platform `alerts-validator-observability.yaml`):
   - `ValidatorDoubleSignEvidenceObserved` — `max by (chain_id) (max_over_time(tendermint_consensus_byzantine_validators[10m])) > 0`, `for: 0m`. Best-effort *leading* signal; fires within ~scrape interval of evidence landing.
   - `ValidatorNewlyJailed` — the *durable* detector; a tombstone shows up as a jailing. `for: 5m`, with a 30m lookback in the `unless`-delta (jailed-now `unless` jailed-30m-ago) — the 30m is the detection window, not the fire delay.

   During a migration, treat **either** firing as "a consensus key is live on two hosts — stop one now."

### The `systemctl enable seid` auto-restart seam

The sharpest trap (flagged in the migration cross-review): if the legacy host has `seid` enabled as a systemd unit, a **reboot or operator `start` re-launches it** — and if the platform validator is already signing, that's a double-sign. `systemctl stop seid` alone is **not** sufficient.

```bash
# On the legacy host — stop AND disable, then confirm it cannot come back:
systemctl disable --now seid
systemctl is-enabled seid    # → "disabled"
systemctl status seid        # → inactive (dead)
# Post-soak, remove the key material from the host so a manual start can't sign:
#   shred -u $SEID_HOME/config/priv_validator_key.json
```

---

## 5. Cutover sequence

Ordered. Do not reorder steps 3 and 4.

```text
1. PRE-STAGE (no consensus impact)
   - BYO Secrets committed (SOPS-encrypted, §2) to the platform repo.
   - node-19 SND committed but NOT yet reconciling (kept out of the kustomization
     resources list, or spec.paused: true).
   - sei-infra removal PR staged (sei-infra#1034). Mechanism: node-19 is the TAIL of
     its eu-central-1 region in inventory.json (regionDistributions nodeStartId 10,
     instanceCount 10 -> covers node-10..19), so removal = drop that region's
     instanceCount 10 -> 9 (drops node-19). The explicit nodeStartId pins (0/10/20)
     keep the FOLLOWING region's NODE_ID base fixed so nodes 20-29 do NOT reindex
     into 19-28. tools/sei-infra/terraform.py honors the per-region nodeStartId.
     (This is a count decrement on the right region, NOT terraform state surgery.)
   - Confirm controller image is 5d03f33+ (§3).

2. STOP THE LEGACY SIGNER (begins the only unsafe window)
   - systemctl disable --now seid on the legacy host (§4).
   - Verify: no new "signed and pushed vote" in the host's seid logs; the validator
     starts MISSING blocks on explorers / cosmos_validators_missed_blocks climbs.
   - This is the point of no return for "both could sign" — from here, exactly zero
     hosts are signing this key. The validator is DOWN (acceptable; <1/3 of set).
   - Keep this window SHORT: a prolonged down period risks a DOWNTIME jail (distinct
     from a tombstone — downtime is recoverable via unjail). Stay well under the
     chain's signed-blocks window; don't let bring-up debugging drag the gap out.

3. BRING UP THE PLATFORM SIGNER (only after step 2 is confirmed)
   - Add node-19 to the kustomization resources (or spec.paused: false); merge.
   - Flux decrypts the Secrets and the SeiNode's bootstrap plan runs (no-snapshot
     validator): ensure-data-pvc -> validate-signing-key -> validate-node-key ->
     apply-rbac-proxy-config -> apply-statefulset -> apply-service ->
     configure-genesis -> config-apply -> discover-peers -> config-validate ->
     mark-ready (config-apply writes the base config; discover-peers then writes
     persistent-peers; config-validate checks the assembled result last). seid then
     block-syncs once the pod is up (there is no "block-sync" task — it's seid
     catching up, observed via catching_up/height, not the plan).
   - Expect the networking.tcp DNS race on cold start (§6 finding 1): ~6-8 min of
     seid CrashLoopBackOff until external-DNS publishes the P2P record and CoreDNS's
     negative cache expires. This SELF-HEALS. Do not "fix" it by dropping tcp.

4. VERIFY REJOIN
   - seid: catching_up=false, "signed and pushed vote ... <migrated ADDR>".
   - cosmos_validators_jailed = 0; tendermint_consensus_byzantine_validators = 0.
   - SND READY, plan cleared.

5. DECOMMISSION (after a soak, e.g. 24h signing cleanly)
   - shred the key material on the legacy host (§4).
   - Merge the sei-infra PR to terraform-destroy node-19.
```

The unsafe window is **between steps 2 and 4-confirmed**. Keep it short, but never shorten it by overlapping step 2 and step 3.

---

## 6. Findings from the harbor dry-run

Numbered as encountered. 1–4 are the ones that change operator behavior.

**0. write-mode must match the image.** `storage.state_commit.write_mode` is version-coupled: `cosmos_only` (e.g. seid v6.5.1) vs `memiavl_only` (recent/nightly). A mismatch crashloops seid at startup (`invalid state-commit.sc-write-mode`). Set the override to match the image you're deploying.

**1. `networking.tcp` cold-start DNS race (self-heals).** A `tcp` validator gets a per-pod internet-facing NLB + an external-DNS Route53 record, and seid is configured with that hostname as its P2P `external-address`. On first boot seid resolves its own external address *before* external-DNS has published it → `lookup ...: no such host` → CrashLoopBackOff, made worse by CoreDNS negative-cache TTL. It clears on its own in ~6–8 min. **This is expected; do not drop `networking.tcp` to "fix" it** — a public arctic-1 validator needs the NLB.

**2. Deploy clean — never blow-away-and-recreate a running chain's SND.** This bit the dry-run's genesis chain, whose nodes use **controller-generated** `node_key.json`: recreating the SND regenerates those keys per pod, but peers keep the *old* NodeIDs in `persistent_peers` → every P2P dial is rejected (`peer NodeID = X, want Y`) and the chain wedges (never produces a block). A fresh deploy's `discover-peers` wires correct NodeIDs the first time. **For *this* BYO validator the NodeID is pinned by the `nodeKey` Secret and is stable across recreation** — so its NodeID won't churn, but recreation is still hazardous for a different reason: finding 3 (it destroys the data PVC). Bottom line: don't recreate a running validator's SND; if you must replace, do it clean.

**3. Deleting the SeiNode destroys the data PVC.** A `Failed` SeiNode does **not** self-heal from a spec edit — the controller treats `Failed` as terminal and emits "Delete and recreate the resource to retry", so the only way to replan is to delete the SeiNode. But the data PVC carries a controller ownerRef **directly to the SeiNode** (the SeiNode owns the StatefulSet→Pod *and* the PVC in parallel — the PVC is **not** a StatefulSet `volumeClaimTemplate`, so it is not protected by the STS's `WhenDeleted=Retain`). Deleting the SeiNode therefore GC-deletes the PVC, destroying chain state and forcing a full re-sync. The **consensus identity survives** (it's in the Secrets), so the validator comes back as itself — but budget for the resync. Note the scope of `deletionPolicy: Retain`: it governs the **SND→child** cascade only — it protects the PVC when the *SND* is deleted, but a manual `kubectl delete seinode <child>` (the delete-to-replan action) still GC-deletes the PVC via the SeiNode ownerRef regardless.

**4. Controller operatorKeyring guard (fixed).** Pre-`5d03f33`, the *update* plan (`buildRunningPlan`) validated `operatorKeyring` unconditionally, so a no-operatorKeyring validator failed its first image/sidecar bump with `validate-operator-keyring: secretName is empty` (the node ran and signed fine; the controller reconcile wedged at the gate, blocking mark-ready and future updates). Fixed in sei-k8s-controller#380; ensure the cluster runs ≥ `5d03f33` (§3) before migrating.

---

## 7. Rollback

Before step 5 (key material still on the legacy host), rollback is symmetric to cutover — and carries the **same double-sign rule**:

```text
1. Stop the platform signer: spec.paused: true (or remove from kustomization); merge.
   Verify the platform pod's seid is no longer signing.
2. ONLY THEN restart the legacy signer: systemctl enable --now seid on the host.
3. Verify the legacy host resumes signing; jailed=0; byzantine=0.
```

Never run step 2 before step 1 is confirmed. After step 5 (key shredded on the host), rollback means restoring the key from secure backup onto a host — same discipline.

---

## 8. Pre-flight checklist

- [ ] Controller image ≥ `5d03f33` on the target cluster (§3).
- [ ] `write_mode` override matches the seid image (§6.0).
- [ ] BYO Secrets committed SOPS-encrypted; decrypt round-trips; no plaintext in git (§2).
- [ ] SND staged but **not** reconciling (paused or out of kustomization) (§5.1).
- [ ] `replicas: 1` (the CEL guard enforces this, but verify) (§4).
- [ ] sei-infra removal PR staged with the `nodeStartId` pin (so remaining validators don't reindex).
- [ ] Legacy `seid` will be **disabled**, not just stopped (§4).
- [ ] A human is watching `ValidatorDoubleSignEvidenceObserved` / `ValidatorNewlyJailed` through the cutover window.

---

## References

- Migration PR / SND: `sei-protocol/platform#816` (arctic-1 node-19).
- Umbrella issue: `sei-protocol/platform#801` (migrate all arctic-1 nodes onto K8s).
- Legacy removal: `sei-protocol/sei-infra#1034` (node-19 removal + `nodeStartId` pin).
- Controller fix: `sei-protocol/sei-k8s-controller#380` (operatorKeyring guard), shipped via `sei-protocol/platform#826` (image `5d03f33`).
- Alerts: `alerts-validator-observability.yaml` in `sei-protocol/platform` — `ValidatorDoubleSignEvidenceObserved`, `ValidatorNewlyJailed`.
- BYO-secrets types: `api/v1alpha1/validator_types.go`.
