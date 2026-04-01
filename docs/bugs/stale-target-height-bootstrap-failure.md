# Bug: Stale `targetHeight` Causes Bootstrap Job Immediate Halt

**Date:** 2026-03-29
**Severity:** Medium — blocks shadow replayer (and any bootstrap node) from initializing
**Status:** Mitigated (manual bump); root cause requires design fix

---

## Summary

When a `SeiNode` with a bootstrap Job has a `snapshot.s3.targetHeight` that is
lower than the height of the S3 snapshot it restores, the bootstrap `seid`
process immediately halts on its first block and exits with code 130 (SIGINT).
Because the Job is created with `backoffLimit: 0`, this single failure marks the
entire init plan as `Failed`.

## Reproduction

1. Deploy a `SeiNode` with `replayer.snapshot.s3.targetHeight: 198740000`
2. The `snapshot-restore` task downloads the latest snapshot from S3 (currently
   at height 200045000 — ~1.3M blocks ahead of the target)
3. The bootstrap Job starts seid with `--halt-height 198740000`
4. seid begins at height 200045000 (already past the halt height), processes
   one block, and triggers the Cosmos SDK halt-height check
5. seid sends itself SIGINT → exit code 130
6. Job fails immediately (`backoffLimit: 0`), `await-bootstrap-complete` marks
   the plan as `Failed`

## Root Cause

`targetHeight` serves two purposes in `bootstrap_resources.go`:

1. **Snapshot selection** — passed to `snapshot-restore` to find the right S3
   object. But the seictl restore task downloads the *closest available*
   snapshot, which may be far newer if snapshots have been regenerated since the
   manifest was written.

2. **Halt height** — used verbatim as `--halt-height` for the bootstrap seid
   process. This assumes the restored snapshot is at or below `targetHeight`,
   which becomes false as new snapshots are uploaded to S3.

The coupling between these two concerns means `targetHeight` silently becomes
stale as the chain advances and new snapshots replace old ones.

Relevant code path:

```
bootstrap_resources.go:142  haltHeight := snap.S3.TargetHeight
bootstrap_resources.go:143  seidCmd, seidArgs := bootstrapWaitCommand(bootstrapSidecarPort(node), haltHeight)
bootstrap_resources.go:189  exec seid start --home %s --halt-height %d
```

## Impact

- Any `SeiNode` with a bootstrap flow (shadow replayer, full node with
  `bootstrapImage`, validator with snapshot) will fail to initialize if
  `targetHeight` falls behind the latest available snapshot in S3.
- The failure is silent from the user's perspective — the node simply goes to
  `Failed` phase. Diagnosing it requires checking bootstrap Job exit codes and
  seid logs for the halt message.

## Mitigation

Bump `targetHeight` in the manifest to a value ahead of the latest S3 snapshot.
For the shadow replayer, this was changed from `198740000` → `200100000`.

This is a manual fix that will need to be repeated as the chain advances.

## Potential Fixes

1. **Decouple snapshot selection from halt height.** Add a separate
   `haltHeight` field (or compute it dynamically) so `targetHeight` only
   controls which snapshot to download. The halt height could be derived from
   the actual restored snapshot height + a configurable offset.

2. **Treat exit code 130 as success in the bootstrap Job.** The halt-height
   exit is an expected shutdown. The `await-bootstrap-complete` task (or the
   Job spec itself) could treat exit code 130 as a successful completion rather
   than a failure.

3. **Resolve `targetHeight` dynamically.** Read `latest.txt` from S3 at plan
   time and set `--halt-height` to `latest + offset`, similar to what the
   original sei-infra scripts did.

4. **Skip halt-height if restored height exceeds it.** The bootstrap wait
   script could query seid's current height after state sync and skip the
   `--halt-height` flag if already past it. This avoids the immediate-halt
   scenario entirely.
