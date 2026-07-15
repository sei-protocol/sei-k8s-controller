//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

// Gov tx parameters for the upgrade flow. usei-only; the deposit must clear the
// chain's min_deposit so the proposal enters voting immediately (not the deposit
// period).
const (
	upgradeDeposit = "20000000usei"
	govFees        = "10000usei"
	proposeGas     = 500000
	voteGas        = 200000
)

const (
	// defaultUpgradeHeightDelta schedules the upgrade this many blocks ahead of
	// the current height — wide enough that the proposal clears the shortened
	// voting period and passes before the chain reaches it. At Sei's ~600ms-1s
	// blocks, 200 blocks comfortably outlasts the 60s voting period + tally;
	// env-overridable for faster/slower chains. The whole flow's safety rests on
	// this margin, so it must stay generous (see TestNightlyChainUpgrade).
	defaultUpgradeHeightDelta = 200
	// minUpgradeHeightDelta guards against a misconfigured delta too small for the
	// pre-halt poll margin below.
	minUpgradeHeightDelta = 30
	// haltPollMargin is how many blocks BEFORE the upgrade height the suite stops
	// polling: it polls the (load-balanced) aggregate RPC only while the chain is
	// still serving, then settles. At the halt itself every validator stops
	// serving RPC simultaneously, so the halt height is unpollable — polling to a
	// pre-halt height keeps the endpoint alive while still confirming the chain is
	// about to halt.
	haltPollMargin = 10
	// haltSettle bounds the wait, after the chain reaches the pre-halt height, for
	// the remaining blocks to commit and every validator to halt at the upgrade
	// height. Over-waiting is free (the chain sits halted until the image bump);
	// the only failure is waiting too short, so this is sized with slack.
	haltSettle = 90 * time.Second
	// postUpgradeProgress is how many blocks past the upgrade height each
	// validator must reach to prove it resumed on the new binary.
	postUpgradeProgress = 10
)

// votingPeriodGenesis shortens the genesis gov voting period so the proposal
// passes within the upgrade-height window rather than the multi-day default.
var votingPeriodGenesis = map[string]string{
	"gov.voting_params.voting_period": "60s",
}

// upgradeConfig are the seid runtime overrides the upgrade flow needs: the REST
// API serves the gov proposal queries (off by default), and kv tx-indexing lets
// the proposal-submission tx be found.
var upgradeConfig = map[string]string{
	"api.rest.enable":  "true",
	"tx_index.indexer": "kv",
}

// restUnreachable is the last-seen note when a gov REST poll gets no 200.
const restUnreachable = "REST unreachable / non-200"

// TestNightlyChainUpgrade drives a Sei major software upgrade end-to-end through the SDK
// task surface: provision a 4-validator chain on the pre-upgrade image -> submit a
// GovSoftwareUpgrade proposal and read its minted ID off the task output ->
// vote yes from every validator -> wait for it to pass -> let the chain halt at
// the upgrade height -> bump the SeiNetwork image to the post-upgrade build ->
// assert the upgrade handler ran and every validator resumed past the upgrade
// height.
//
// The upgrade height is scheduled far enough ahead (defaultUpgradeHeightDelta)
// that the proposal passes before the chain reaches it; the upgrade-applied check
// (waitUpgradeApplied) is the safety net that fails loud if a too-fast chain
// passed the height while still voting (no plan scheduled, no real upgrade).
//
// Inputs (env): SEI_CHAIN_ID (base), SEID_UPGRADE_FROM_IMAGE (pre-upgrade)
// [required], SEID_UPGRADE_TO_IMAGE (post-upgrade) [required], SEI_UPGRADE_NAME
// (the upgrade handler name registered in the post image) [required];
// SEI_NAMESPACE, UPGRADE_HEIGHT_DELTA [optional]. Run with -test.timeout 0 (see
// TestNightlyBenchmark).
func TestNightlyChainUpgrade(t *testing.T) {
	requireCluster(t)
	chainID := runChainID(mustEnv(t, "SEI_CHAIN_ID"))
	preImage := mustEnv(t, "SEID_UPGRADE_FROM_IMAGE")
	postImage := mustEnv(t, "SEID_UPGRADE_TO_IMAGE")
	upgradeName := mustEnv(t, "SEI_UPGRADE_NAME")
	ns := envOr("SEI_NAMESPACE", "")
	delta := int64(envInt(t, "UPGRADE_HEIGHT_DELTA", defaultUpgradeHeightDelta))
	if delta < minUpgradeHeightDelta {
		t.Fatalf("UPGRADE_HEIGHT_DELTA %d below minimum %d", delta, minUpgradeHeightDelta)
	}

	const validators = 4
	s := spec{
		chainID:    chainID,
		runID:      chainID,
		namespace:  ns,
		seidImage:  preImage,
		validators: validators,
		// No RPC followers: the aggregate network endpoints serve the gov REST
		// queries + pre-halt height polls, and the validators are the vote targets
		// and the per-node post-upgrade liveness targets.
		rpcNodes: 0,
		// No storageConfig override: the upgrade/migration path is what this suite
		// exercises, so it runs the controller's default storage mode.
	}
	runLabels := map[string]string{runLabelKey: s.runID}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	c := openClient(ctx, t)

	// Provision on the pre-upgrade image with a shortened voting period so the
	// proposal can pass inside the upgrade-height window.
	ch := &chain{}
	cleanupChain(t, ch)
	net, err := c.CreateNetwork(ctx, networkSpec(s, preImage, runLabels))
	if err != nil {
		t.Fatalf("create network %q: %v", chainID, err)
	}
	ch.network = net
	if err := net.WaitReady(ctx); err != nil {
		t.Fatalf("network %q ready: %v", chainID, err)
	}
	t.Logf("network %s: ready (%d validators, pre-upgrade image)", chainID, validators)

	tmRPC := ch.network.TendermintRPC() // aggregate endpoint; name/namespace-derived, stable across the image bump
	rest := ch.network.REST()
	hc := &http.Client{Timeout: 15 * time.Second}

	// Schedule the upgrade comfortably ahead of the current height so the 60s
	// voting period elapses and the proposal passes before the chain halts.
	cur, ok := sei.LatestHeight(ctx, hc, tmRPC)
	if !ok {
		t.Fatalf("read current height from %s", tmRPC)
	}
	upgradeHeight := cur + delta
	t.Logf("current height %d; scheduling upgrade %q at height %d", cur, upgradeName, upgradeHeight)

	// Submit the upgrade proposal from validator-0 and read the minted
	// proposal ID straight off the task output (no chain-as-medium scan).
	proposer := validatorName(chainID, 0)
	out := runTaskComplete(ctx, t, c, sei.TaskSpec{
		Name:      taskName(chainID, "propose"),
		Namespace: ns,
		Node:      proposer,
		Kind:      sei.TaskGovSoftwareUpgrade,
		Timeout:   8 * time.Minute,
		Labels:    runLabels,
		GovSoftwareUpgrade: &sei.GovSoftwareUpgrade{
			ChainID:        chainID,
			Title:          "integration upgrade " + upgradeName,
			Description:    "harness major-upgrade scenario",
			UpgradeName:    upgradeName,
			UpgradeHeight:  upgradeHeight,
			InitialDeposit: upgradeDeposit,
			Fees:           govFees,
			Gas:            proposeGas,
		},
	})
	if out == nil || out.GovSoftwareUpgrade == nil || out.GovSoftwareUpgrade.ProposalID == 0 {
		t.Fatalf("upgrade task did not surface a proposal ID: %+v", out)
	}
	proposalID := out.GovSoftwareUpgrade.ProposalID
	t.Logf("upgrade proposal submitted from %s; proposal id %d (from task output)", proposer, proposalID)

	// Vote yes from every validator in parallel.
	voteAllValidators(ctx, t, c, chainID, ns, validators, proposalID, runLabels)
	t.Logf("all %d validators voted yes on proposal %d", validators, proposalID)

	// Wait for the proposal to pass.
	if err := waitProposalPassed(ctx, hc, rest, proposalID); err != nil {
		t.Fatalf("proposal %d did not pass: %v", proposalID, err)
	}
	t.Logf("proposal %d passed", proposalID)

	// Wait for the chain to approach the upgrade height, then settle for the
	// halt. The halt height itself is unpollable (all validators stop serving RPC
	// at once), so poll the aggregate only to a pre-halt height (still serving),
	// then settle a bounded time for the final blocks + halt. The image bump must
	// land only after the halt: the post binary panics if it processes any block
	// below the upgrade height.
	preHalt := upgradeHeight - haltPollMargin
	approachCtx, cancelApproach := context.WithTimeout(ctx, 15*time.Minute)
	err = pollHeightAtLeast(approachCtx, hc, tmRPC, preHalt)
	cancelApproach()
	if err != nil {
		t.Fatalf("chain did not approach the upgrade height (target %d): %v", preHalt, err)
	}
	t.Logf("chain reached pre-halt height %d; settling %s for the halt at %d", preHalt, haltSettle, upgradeHeight)
	select {
	case <-time.After(haltSettle):
	case <-ctx.Done():
		t.Fatalf("interrupted while settling for the halt: %v", ctx.Err())
	}

	// Bump the image by re-applying the SeiNetwork with the post-upgrade build.
	// A SeiNetwork re-apply (not per-node UpdateNodeImage) lets the controller
	// cascade the new binary to every validator; the controller never writes the
	// parent spec itself, so the SDK's server-side apply does not conflict and the
	// unchanged fields re-apply idempotently (the genesis ceremony is latched).
	if _, err := c.CreateNetwork(ctx, networkSpec(s, postImage, runLabels)); err != nil {
		t.Fatalf("bump network image to %q: %v", postImage, err)
	}
	t.Logf("SeiNetwork %s image bumped to %s", chainID, postImage)

	// Assert the upgrade actually executed: the x/upgrade module records the
	// applied plan only when the handler ran at the scheduled height. This fails
	// loud if a too-fast chain sailed past the upgrade height while still voting
	// (no plan scheduled), which a liveness-only check would green.
	if err := waitUpgradeApplied(ctx, hc, rest, upgradeName); err != nil {
		t.Fatalf("upgrade %q was not applied on-chain: %v", upgradeName, err)
	}
	t.Logf("upgrade %q applied on-chain", upgradeName)

	// Recovery: every validator resumed on the new binary and advanced past the
	// upgrade height. AwaitNodesAtHeight polls each validator's own sidecar
	// (co-located, not the aggregate RPC), so it is immune to the halt black-hole
	// and proves each validator individually — not merely "a pod is Ready".
	target := upgradeHeight + postUpgradeProgress
	awaitAllValidatorsAtHeight(ctx, t, c, chainID, ns, validators, target, runLabels)
	t.Logf("all %d validators advanced past the upgrade to height %d — TestNightlyChainUpgrade OK", validators, target)
}

// networkSpec builds the SeiNetwork spec for the upgrade chain. provision and the
// image bump share it so a re-apply changes ONLY the image — a field added here
// can never be silently stripped by the bump's server-side apply.
func networkSpec(s spec, image string, labels map[string]string) sei.NetworkSpec {
	return sei.NetworkSpec{
		Name:           s.chainID,
		Namespace:      s.namespace,
		Image:          image,
		Validators:     s.validators,
		Labels:         labels,
		Config:         upgradeConfig,
		Genesis:        votingPeriodGenesis,
		DeletionPolicy: sei.DeletionDelete,
	}
}

// voteAllValidators runs a GovVote task against each validator in parallel and
// fails the suite (surfacing every failure via errors.Join) if any vote does not
// complete. A proposal passes once 2/3 have voted; the suite votes all of them so
// a single unhealthy validator is caught rather than masked.
func voteAllValidators(
	ctx context.Context, t *testing.T, c *sei.Client, chainID, ns string,
	validators int, proposalID uint64, labels map[string]string,
) {
	t.Helper()
	runTasksAcrossValidators(ctx, t, c, validators, "votes", func(i int) sei.TaskSpec {
		return sei.TaskSpec{
			Name:      taskName(chainID, fmt.Sprintf("vote-%d", i)),
			Namespace: ns,
			Node:      validatorName(chainID, i),
			Kind:      sei.TaskGovVote,
			Timeout:   8 * time.Minute,
			Labels:    labels,
			GovVote: &sei.GovVote{
				ChainID:    chainID,
				ProposalID: proposalID,
				Option:     sei.VoteYes,
				Fees:       govFees,
				Gas:        voteGas,
			},
		}
	})
}

// awaitAllValidatorsAtHeight runs an AwaitNodesAtHeight task against each
// validator in parallel — the semantic recovery gate: each validator's sidecar
// confirms its own local height crossed target on the new binary.
func awaitAllValidatorsAtHeight(
	ctx context.Context, t *testing.T, c *sei.Client, chainID, ns string,
	validators int, target int64, labels map[string]string,
) {
	t.Helper()
	what := fmt.Sprintf("post-upgrade progress to %d", target)
	runTasksAcrossValidators(ctx, t, c, validators, what, func(i int) sei.TaskSpec {
		return sei.TaskSpec{
			Name:      taskName(chainID, fmt.Sprintf("await-%d", i)),
			Namespace: ns,
			Node:      validatorName(chainID, i),
			Kind:      sei.TaskAwaitNodesAtHeight,
			Timeout:   12 * time.Minute,
			Labels:    labels,
			// A validator halted at the upgrade height is still phase=Running
			// (the controller does not demote phase on a transient halt), so the
			// default RequirePhase=Running is correct here.
			AwaitNodesAtHeight: &sei.AwaitNodesAtHeight{TargetHeight: target},
		}
	})
}

// runTaskWait runs one task to completion, registering its deletion for cleanup,
// and returns its typed outputs (nil for kinds that emit none) or an error. It
// never calls t.Fatal, so it is safe from a spawned goroutine — the sole owner of
// the run -> cleanup -> wait sequence.
func runTaskWait(ctx context.Context, t *testing.T, c *sei.Client, ts sei.TaskSpec) (*sei.TaskOutputs, error) {
	t.Helper()
	task, err := c.RunTask(ctx, ts)
	if err != nil {
		return nil, fmt.Errorf("run task %s (%s): %w", ts.Name, ts.Kind, err)
	}
	registerTaskCleanup(t, task)
	out, err := task.WaitComplete(ctx)
	if err != nil {
		return nil, fmt.Errorf("task %s (%s): %w", ts.Name, ts.Kind, err)
	}
	return out, nil
}

// runTaskComplete runs one task to completion and fails the suite if it does not
// complete. Main-goroutine only (it calls t.Fatal). Returns the task's typed
// outputs (nil for kinds that emit none).
func runTaskComplete(ctx context.Context, t *testing.T, c *sei.Client, ts sei.TaskSpec) *sei.TaskOutputs {
	t.Helper()
	out, err := runTaskWait(ctx, t, c, ts)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// runTasksAcrossValidators runs one task per validator in parallel, building each
// spec via specFn(i), and fails the suite (surfacing every failure via
// errors.Join) if any does not complete. The single t.Fatalf runs on the main
// goroutine after Wait — Fatal from a spawned goroutine would not fail the test.
func runTasksAcrossValidators(
	ctx context.Context, t *testing.T, c *sei.Client,
	validators int, what string, specFn func(i int) sei.TaskSpec,
) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make([]error, validators)
	for i := range validators {
		wg.Go(func() {
			_, errs[i] = runTaskWait(ctx, t, c, specFn(i))
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// registerTaskCleanup best-effort deletes a task on a fresh context at test end.
func registerTaskCleanup(t *testing.T, task *sei.Task) {
	t.Helper()
	t.Cleanup(func() {
		delCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = task.Delete(delCtx)
	})
}

// validatorName is the SeiNetwork child-naming contract (controller labels.go):
// validator SeiNodes are <network>-<ordinal>, 0-based.
func validatorName(chainID string, ordinal int) string {
	return fmt.Sprintf("%s-%d", chainID, ordinal)
}

// taskName builds a stable, DNS-safe SeiNodeTask name for a run + step.
func taskName(chainID, step string) string {
	return fmt.Sprintf("%s-%s", chainID, step)
}

// --- chain gov queries (harness-level: Cosmos gov REST, not SDK) ---

// waitProposalPassed polls a single proposal until it reaches PASSED, failing
// fast if it lands in a terminal non-passed state (REJECTED/FAILED).
func waitProposalPassed(ctx context.Context, hc *http.Client, rest string, id uint64) error {
	endpoint := fmt.Sprintf("%s/cosmos/gov/v1beta1/proposals/%d", rest, id)
	return pollREST(ctx, fmt.Sprintf("proposal %d passed", id), func(ctx context.Context) (bool, string, error) {
		var body struct {
			Proposal struct {
				Status string `json:"status"`
			} `json:"proposal"`
		}
		if !getJSONInto(ctx, hc, endpoint, &body) {
			return false, restUnreachable, nil
		}
		switch body.Proposal.Status {
		case "PROPOSAL_STATUS_PASSED":
			return true, "", nil
		case "PROPOSAL_STATUS_REJECTED", "PROPOSAL_STATUS_FAILED":
			return false, "", fmt.Errorf("proposal %d reached terminal status %s", id, body.Proposal.Status)
		default:
			return false, "status " + body.Proposal.Status, nil
		}
	})
}

// waitUpgradeApplied polls the x/upgrade applied-plan endpoint until the named
// upgrade reports a non-zero applied height — proof the upgrade handler ran at the
// scheduled height (not merely that the chain is alive).
func waitUpgradeApplied(ctx context.Context, hc *http.Client, rest, upgradeName string) error {
	endpoint := fmt.Sprintf("%s/cosmos/upgrade/v1beta1/applied_plan/%s", rest, upgradeName)
	return pollREST(ctx, fmt.Sprintf("upgrade %q applied", upgradeName), func(ctx context.Context) (bool, string, error) {
		var body struct {
			Height string `json:"height"`
		}
		if !getJSONInto(ctx, hc, endpoint, &body) {
			return false, restUnreachable, nil
		}
		h, err := strconv.ParseInt(body.Height, 10, 64)
		if err != nil || h <= 0 {
			return false, "applied height " + body.Height, nil
		}
		return true, "", nil
	})
}

// --- height polling (harness-level, via the SDK's dual-shape /status reader) ---

// pollHeightAtLeast blocks until the chain's latest height reaches target. Unlike
// sei.WaitHeightAdvances (a relative +delta) this waits for an absolute height,
// reading via sei.LatestHeight so it handles both /status shapes the Sei fork
// emits.
func pollHeightAtLeast(ctx context.Context, hc *http.Client, tmRPC string, target int64) error {
	return pollREST(ctx, fmt.Sprintf("height >= %d", target), func(ctx context.Context) (bool, string, error) {
		h, ok := sei.LatestHeight(ctx, hc, tmRPC)
		if !ok {
			return false, "RPC unreachable", nil
		}
		return h >= target, fmt.Sprintf("height %d", h), nil
	})
}

// --- small polling + HTTP helpers ---

// pollREST ticks done() every 3s until it returns true or ctx fires. done returns
// (satisfied, lastSeen, err): a non-nil err aborts (a terminal condition); else
// false keeps polling and lastSeen is folded into the timeout error so an
// unattended deadline is debuggable (e.g. "height 1187" or "status VOTING").
func pollREST(ctx context.Context, what string, done func(context.Context) (bool, string, error)) error {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	var lastSeen string
	for {
		ok, seen, err := done(ctx)
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		if ok {
			return nil
		}
		if seen != "" {
			lastSeen = seen
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: not satisfied before deadline (last: %s): %w", what, lastSeen, ctx.Err())
		case <-tick.C:
		}
	}
}

// getJSONInto GETs url and decodes a 200 body into out, returning false on any
// transient failure (unreachable, non-200, decode error) so the caller keeps
// polling.
func getJSONInto(ctx context.Context, hc *http.Client, url string, out any) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := hc.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	return json.NewDecoder(resp.Body).Decode(out) == nil
}
