//go:build integration

package integration

import (
	"context"
	"encoding/json"
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

// Gov tx parameters for the upgrade flow, ported from the major-upgrade scenario
// (scenarios/major-upgrade.yaml). usei-only; the deposit must clear the chain's
// min_deposit so the proposal enters voting immediately.
const (
	upgradeDeposit = "20000000usei"
	govFees        = "10000usei"
	proposeGas     = 500000
	voteGas        = 200000
)

// upgradeHeightDelta is how many blocks ahead of the current height the upgrade
// is scheduled — wide enough that the proposal clears the (shortened) voting
// period and passes before the chain reaches it. Env-overridable for faster or
// slower chains.
const defaultUpgradeHeightDelta = 200

// votingPeriodOverride shortens the genesis gov voting period so the proposal
// passes within the upgradeHeightDelta window rather than the multi-day default.
var votingPeriodGenesis = map[string]string{
	"gov.voting_params.voting_period": "60s",
}

// TestChainUpgrade drives a Sei major software upgrade end-to-end through the SDK
// task surface, replacing the Chaos-Mesh Workflow DAG: provision a 4-validator
// chain on the pre-upgrade image -> submit a GovSoftwareUpgrade proposal ->
// resolve its ID from the chain (chain-as-medium) -> vote yes from every
// validator -> wait for it to pass -> let the chain halt at the upgrade height ->
// bump the SeiNetwork image to the post-upgrade build -> assert the chain
// resumes past the upgrade height. The upgrade height is chosen so the proposal
// passes before the chain reaches it.
//
// Inputs (env): SEI_CHAIN_ID (base), SEID_IMAGE (pre-upgrade) [required],
// SEID_UPGRADE_IMAGE (post-upgrade) [required], SEI_UPGRADE_NAME (the upgrade
// handler name registered in the post image) [required]; SEI_NAMESPACE,
// UPGRADE_HEIGHT_DELTA [optional]. Run with -test.timeout 0 (see TestBenchmark).
func TestChainUpgrade(t *testing.T) {
	requireCluster(t)
	chainID := mustEnv(t, "SEI_CHAIN_ID")
	preImage := mustEnv(t, "SEID_IMAGE")
	postImage := mustEnv(t, "SEID_UPGRADE_IMAGE")
	upgradeName := mustEnv(t, "SEI_UPGRADE_NAME")
	ns := envOr("SEI_NAMESPACE", "")
	delta := int64(envInt(t, "UPGRADE_HEIGHT_DELTA", defaultUpgradeHeightDelta))

	const validators = 4
	s := spec{
		chainID:    chainID,
		runID:      chainID,
		namespace:  ns,
		seidImage:  preImage,
		validators: validators,
		// No RPC followers: the aggregate network endpoints serve the gov REST
		// queries + height polls, and the validators are the vote targets.
		rpcNodes: 0,
		timeout:  60 * time.Minute,
		// No storageConfig override: the upgrade/migration path is what this suite
		// exercises, so it runs the controller's default storage mode.
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	c := openClient(ctx, t)
	cs := clientset(t)

	// Provision on the pre-upgrade image with a shortened voting period so the
	// proposal can pass inside the upgrade-height window.
	ch, err := provisionUpgrade(ctx, t, c, s)
	cleanupChain(t, ch)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	netNS := ch.network.Namespace()
	tmRPC := ch.network.TendermintRPC()
	rest := ch.network.REST()
	hc := &http.Client{Timeout: 15 * time.Second}

	// Schedule the upgrade comfortably ahead of the current height so the 60s
	// voting period elapses and the proposal passes before the chain halts.
	cur, err := pollHeight(ctx, hc, tmRPC)
	if err != nil {
		t.Fatalf("read current height: %v", err)
	}
	upgradeHeight := cur + delta
	t.Logf("current height %d; scheduling upgrade %q at height %d", cur, upgradeName, upgradeHeight)

	// 1) Submit the upgrade proposal from validator-0.
	proposer := validatorName(chainID, 0)
	runTaskComplete(ctx, t, c, sei.TaskSpec{
		Name:      taskName(chainID, "propose"),
		Namespace: ns,
		Node:      proposer,
		Kind:      sei.TaskGovSoftwareUpgrade,
		Timeout:   8 * time.Minute,
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
	t.Logf("upgrade proposal submitted from %s", proposer)

	// 2) Resolve the proposal ID from the chain — the controller does not surface
	// it as a task output (chain-as-medium).
	proposalID, err := resolveProposalID(ctx, hc, rest, upgradeName)
	if err != nil {
		t.Fatalf("resolve proposal id: %v", err)
	}
	t.Logf("resolved proposal id %d", proposalID)

	// 3) Vote yes from every validator in parallel.
	voteAllValidators(ctx, t, c, chainID, ns, validators, proposalID)
	t.Logf("all %d validators voted yes on proposal %d", validators, proposalID)

	// 4) Wait for the proposal to pass.
	if err := waitProposalPassed(ctx, hc, rest, proposalID); err != nil {
		t.Fatalf("proposal %d did not pass: %v", proposalID, err)
	}
	t.Logf("proposal %d passed", proposalID)

	// 5) Let the chain reach the upgrade height and halt (it commits up to
	// upgradeHeight-1 and stops producing the upgrade block).
	haltCtx, cancelHalt := context.WithTimeout(ctx, 15*time.Minute)
	err = pollHeightAtLeast(haltCtx, hc, tmRPC, upgradeHeight-1)
	cancelHalt()
	if err != nil {
		t.Fatalf("chain did not reach the upgrade halt height %d: %v", upgradeHeight-1, err)
	}
	t.Logf("chain reached the upgrade height %d and halted", upgradeHeight)

	// 6) Bump the image by re-applying the SeiNetwork with the post-upgrade build.
	// A single SeiNetwork patch (not per-node UpdateNodeImage) lets the controller
	// cascade the new binary to every validator without fighting its own image
	// re-assertion — the canonical major-upgrade mechanism.
	bumpNetworkImage(ctx, t, c, s, postImage)
	t.Logf("SeiNetwork %s image bumped to %s", chainID, postImage)

	// 7) Recovery: every validator returns to Ready on the new binary, and the
	// chain produces blocks past the upgrade height.
	recCtx, cancelRec := context.WithTimeout(ctx, 15*time.Minute)
	waitValidatorsReady(recCtx, t, cs, netNS, chainID, validators)
	cancelRec()

	progCtx, cancelProg := context.WithTimeout(ctx, 10*time.Minute)
	err = sei.WaitHeightAdvances(progCtx, hc, tmRPC, 5)
	cancelProg()
	if err != nil {
		t.Fatalf("chain did not produce blocks past the upgrade: %v", err)
	}
	t.Logf("chain advanced past the upgrade — TestChainUpgrade OK")
}

// provisionUpgrade provisions the genesis chain with the shortened voting period.
// It mirrors provision() but stamps the gov genesis override (CreateNetwork's
// Genesis map, not Config) and creates no RPC followers.
func provisionUpgrade(ctx context.Context, t *testing.T, c *sei.Client, s spec) (*chain, error) {
	t.Helper()
	ch := &chain{}
	net, err := c.CreateNetwork(ctx, sei.NetworkSpec{
		Name:           s.chainID,
		Namespace:      s.namespace,
		Image:          s.seidImage,
		Validators:     s.validators,
		Labels:         map[string]string{runLabelKey: s.runID},
		Genesis:        votingPeriodGenesis,
		DeletionPolicy: sei.DeletionDelete,
	})
	if err != nil {
		return ch, fmt.Errorf("create network %q: %w", s.chainID, err)
	}
	ch.network = net
	if err := net.WaitReady(ctx); err != nil {
		return ch, fmt.Errorf("network %q ready: %w", s.chainID, err)
	}
	t.Logf("network %s: ready (%d validators, pre-upgrade image)", s.chainID, s.validators)
	return ch, nil
}

// bumpNetworkImage re-applies the SeiNetwork with the post-upgrade image. The SDK
// applies server-side, so re-applying the same spec with a new image patches
// spec.image and the controller rolls every validator onto the new binary; the
// unchanged fields (validators, genesis, deletion policy) re-apply idempotently.
func bumpNetworkImage(ctx context.Context, t *testing.T, c *sei.Client, s spec, image string) {
	t.Helper()
	_, err := c.CreateNetwork(ctx, sei.NetworkSpec{
		Name:           s.chainID,
		Namespace:      s.namespace,
		Image:          image,
		Validators:     s.validators,
		Labels:         map[string]string{runLabelKey: s.runID},
		Genesis:        votingPeriodGenesis,
		DeletionPolicy: sei.DeletionDelete,
	})
	if err != nil {
		t.Fatalf("bump network image to %q: %v", image, err)
	}
}

// voteAllValidators runs a GovVote task against each validator in parallel and
// fails the suite if any vote does not complete. A vote clears 2/3 once enough
// validators have voted; the suite votes all of them.
func voteAllValidators(
	ctx context.Context, t *testing.T, c *sei.Client, chainID, ns string, validators int, proposalID uint64,
) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make([]error, validators)
	for i := range validators {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			node := validatorName(chainID, i)
			task, err := c.RunTask(ctx, sei.TaskSpec{
				Name:      taskName(chainID, fmt.Sprintf("vote-%d", i)),
				Namespace: ns,
				Node:      node,
				Kind:      sei.TaskGovVote,
				Timeout:   8 * time.Minute,
				GovVote: &sei.GovVote{
					ChainID:    chainID,
					ProposalID: proposalID,
					Option:     sei.VoteYes,
					Fees:       govFees,
					Gas:        voteGas,
				},
			})
			if err != nil {
				errs[i] = fmt.Errorf("%s: run vote: %w", node, err)
				return
			}
			t.Cleanup(func() {
				delCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				_ = task.Delete(delCtx)
			})
			if _, err := task.WaitComplete(ctx); err != nil {
				errs[i] = fmt.Errorf("%s: vote: %w", node, err)
			}
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("vote: %v", err)
		}
	}
}

// runTaskComplete runs one task to completion, registering its deletion for
// cleanup, and fails the suite if it does not complete.
func runTaskComplete(ctx context.Context, t *testing.T, c *sei.Client, ts sei.TaskSpec) {
	t.Helper()
	task, err := c.RunTask(ctx, ts)
	if err != nil {
		t.Fatalf("run task %s (%s): %v", ts.Name, ts.Kind, err)
	}
	t.Cleanup(func() {
		delCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = task.Delete(delCtx)
	})
	if _, err := task.WaitComplete(ctx); err != nil {
		t.Fatalf("task %s (%s): %v", ts.Name, ts.Kind, err)
	}
}

// validatorName is the SeiNetwork child-naming contract (controller labels.go):
// validator SeiNodes are <network>-<ordinal>.
func validatorName(chainID string, ordinal int) string {
	return fmt.Sprintf("%s-%d", chainID, ordinal)
}

// taskName builds a stable, DNS-safe SeiNodeTask name for a run + step.
func taskName(chainID, step string) string {
	return fmt.Sprintf("%s-%s", chainID, step)
}

// --- chain-as-medium gov queries (harness-level: Cosmos gov REST, not SDK) ---

// govProposal models just enough of a gov v1beta1 proposal to resolve an upgrade
// proposal by its plan name and read its status.
type govProposal struct {
	ProposalID string `json:"proposal_id"`
	Status     string `json:"status"`
	Content    struct {
		Plan struct {
			Name string `json:"name"`
		} `json:"plan"`
	} `json:"content"`
}

// resolveProposalID polls the gov REST endpoint for the voting-period proposal
// whose upgrade plan name matches upgradeName and returns its ID. The proposal
// appears a few blocks after submission, so this polls until it shows up or ctx
// fires.
func resolveProposalID(ctx context.Context, hc *http.Client, rest, upgradeName string) (uint64, error) {
	// proposal_status=2 is PROPOSAL_STATUS_VOTING_PERIOD.
	endpoint := rest + "/cosmos/gov/v1beta1/proposals?proposal_status=2"
	var found uint64
	err := pollREST(ctx, "resolve proposal id", func(ctx context.Context) (bool, error) {
		var body struct {
			Proposals []govProposal `json:"proposals"`
		}
		if !getJSONInto(ctx, hc, endpoint, &body) {
			return false, nil
		}
		for _, p := range body.Proposals {
			if p.Content.Plan.Name != upgradeName {
				continue
			}
			id, err := strconv.ParseUint(p.ProposalID, 10, 64)
			if err != nil {
				return false, fmt.Errorf("unparseable proposal id %q: %w", p.ProposalID, err)
			}
			found = id
			return true, nil
		}
		return false, nil
	})
	return found, err
}

// waitProposalPassed polls a single proposal until it reaches PASSED, failing
// fast if it lands in a terminal non-passed state (REJECTED/FAILED).
func waitProposalPassed(ctx context.Context, hc *http.Client, rest string, id uint64) error {
	endpoint := fmt.Sprintf("%s/cosmos/gov/v1beta1/proposals/%d", rest, id)
	return pollREST(ctx, fmt.Sprintf("proposal %d passed", id), func(ctx context.Context) (bool, error) {
		var body struct {
			Proposal govProposal `json:"proposal"`
		}
		if !getJSONInto(ctx, hc, endpoint, &body) {
			return false, nil
		}
		switch body.Proposal.Status {
		case "PROPOSAL_STATUS_PASSED":
			return true, nil
		case "PROPOSAL_STATUS_REJECTED", "PROPOSAL_STATUS_FAILED":
			return false, fmt.Errorf("proposal %d reached terminal status %s", id, body.Proposal.Status)
		default:
			return false, nil
		}
	})
}

// --- height polling (harness-level TM /status) ---

// pollHeight returns the chain's current latest block height once.
func pollHeight(ctx context.Context, hc *http.Client, tmRPC string) (int64, error) {
	var h int64
	err := pollREST(ctx, "read height", func(ctx context.Context) (bool, error) {
		var body struct {
			Result struct {
				SyncInfo struct {
					LatestBlockHeight string `json:"latest_block_height"`
				} `json:"sync_info"`
			} `json:"result"`
		}
		if !getJSONInto(ctx, hc, tmRPC+"/status", &body) {
			return false, nil
		}
		v, err := strconv.ParseInt(body.Result.SyncInfo.LatestBlockHeight, 10, 64)
		if err != nil {
			return false, nil
		}
		h = v
		return true, nil
	})
	return h, err
}

// pollHeightAtLeast blocks until the chain's latest height reaches target. Unlike
// sei.WaitHeightAdvances (a relative +delta) this waits for an absolute height —
// used to detect the chain arriving at the upgrade halt.
func pollHeightAtLeast(ctx context.Context, hc *http.Client, tmRPC string, target int64) error {
	return pollREST(ctx, fmt.Sprintf("height >= %d", target), func(ctx context.Context) (bool, error) {
		h, err := pollHeight(ctx, hc, tmRPC)
		if err != nil {
			return false, nil
		}
		return h >= target, nil
	})
}

// --- small polling + HTTP helpers ---

// pollREST ticks done() every 3s until it returns true or ctx fires. A done()
// error aborts (a terminal condition); a false keeps polling.
func pollREST(ctx context.Context, what string, done func(context.Context) (bool, error)) error {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		ok, err := done(ctx)
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: not satisfied before deadline: %w", what, ctx.Err())
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
