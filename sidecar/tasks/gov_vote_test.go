package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	sdk "github.com/sei-protocol/sei-chain/sei-cosmos/types"
	govtypes "github.com/sei-protocol/sei-chain/sei-cosmos/x/gov/types"

	"github.com/sei-protocol/sei-k8s-controller/sidecar/engine"
	"github.com/sei-protocol/sei-k8s-controller/sidecarapi/wire"
)

func TestBuildVoteMsg(t *testing.T) {
	kr, addr := testKeyring(t)
	cfg := engine.ExecutionConfig{Keyring: kr}

	t.Run("happy path", func(t *testing.T) {
		msg, err := buildVoteMsg(cfg, GovVoteRequest{
			KeyName:    "node_admin",
			ProposalID: 42,
			Option:     "yes",
		})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if msg.Voter != addr.String() {
			t.Errorf("voter = %q, want %q", msg.Voter, addr.String())
		}
		if msg.ProposalId != 42 {
			t.Errorf("proposalId = %d, want 42", msg.ProposalId)
		}
		if msg.Option != govtypes.OptionYes {
			t.Errorf("option = %v, want OptionYes", msg.Option)
		}
		// Guard: signAndBroadcast runs ValidateBasic immediately; lock
		// that it accepts the message we produce here.
		if err := msg.ValidateBasic(); err != nil {
			t.Errorf("ValidateBasic on returned msg: %v", err)
		}
	})

	t.Run("zero proposalId is Terminal", func(t *testing.T) {
		_, err := buildVoteMsg(cfg, GovVoteRequest{
			KeyName:    "node_admin",
			ProposalID: 0,
			Option:     "yes",
		})
		if !IsTerminal(err) {
			t.Fatalf("want Terminal, got %v", err)
		}
	})

	t.Run("invalid option is Terminal", func(t *testing.T) {
		_, err := buildVoteMsg(cfg, GovVoteRequest{
			KeyName:    "node_admin",
			ProposalID: 7,
			Option:     "bogus",
		})
		if !IsTerminal(err) {
			t.Fatalf("want Terminal, got %v", err)
		}
	})

	t.Run("nil keyring is Terminal", func(t *testing.T) {
		_, err := buildVoteMsg(engine.ExecutionConfig{}, GovVoteRequest{
			KeyName:    "node_admin",
			ProposalID: 7,
			Option:     "yes",
		})
		if !IsTerminal(err) {
			t.Fatalf("want Terminal, got %v", err)
		}
	})

	t.Run("empty keyName is Terminal", func(t *testing.T) {
		_, err := buildVoteMsg(cfg, GovVoteRequest{
			KeyName:    "",
			ProposalID: 7,
			Option:     "yes",
		})
		if !IsTerminal(err) {
			t.Fatalf("want Terminal, got %v", err)
		}
	})

	t.Run("missing key in keyring is Terminal", func(t *testing.T) {
		_, err := buildVoteMsg(cfg, GovVoteRequest{
			KeyName:    "does-not-exist",
			ProposalID: 7,
			Option:     "yes",
		})
		if !IsTerminal(err) {
			t.Fatalf("want Terminal, got %v", err)
		}
		// Make sure the underlying keyring error is preserved.
		var terr *TerminalError
		if !errors.As(err, &terr) || terr.Unwrap() == nil {
			t.Fatalf("expected wrapped keyring error: %v", err)
		}
	})
}

// govVoteHarness wires a GovVoter with a fake broadcast and a fake vote read
// that returns votes in order, then repeats the last one.
type govVoteHarness struct {
	g     *GovVoter
	reads int
}

func newGovVoteHarness(t *testing.T, result *SignAndBroadcastResult, votes ...*govtypes.Vote) *govVoteHarness {
	t.Helper()
	kr, _ := testKeyring(t)
	h := &govVoteHarness{}
	h.g = &GovVoter{
		cfg: engine.ExecutionConfig{Keyring: kr},
		broadcast: func(context.Context, engine.ExecutionConfig, SignAndBroadcastInput) (*SignAndBroadcastResult, error) {
			return result, nil
		},
		readVote: func(context.Context, engine.ExecutionConfig, string, uint64, sdk.AccAddress) (*govtypes.Vote, error) {
			h.reads++
			if len(votes) == 0 {
				return nil, errors.New("voter not found for proposal")
			}
			return votes[min(h.reads-1, len(votes)-1)], nil
		},
		confirmWait:  50 * time.Millisecond,
		confirmEvery: time.Millisecond,
	}
	return h
}

func runGovVote(t *testing.T, g *GovVoter, option string) (*wire.GovTxResult, error) {
	t.Helper()
	ctx := engine.WithTaskID(context.Background(), "gov-vote-test")
	raw, err := g.Handler()(ctx, map[string]any{
		"chainId": "sei-test", "keyName": "node_admin", "proposalId": 7, "option": option, "fees": "4000usei", "gas": 200000,
	})
	if len(raw) == 0 || string(raw) == "null" {
		return nil, err
	}
	var out wire.GovTxResult
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("decode result: %v", uerr)
	}
	return &out, err
}

func recordedVote(option govtypes.VoteOption) *govtypes.Vote {
	return &govtypes.Vote{ProposalId: 7, Options: govtypes.NewNonSplitVoteOption(option)}
}

// PLT-1401: validators often run with the tx index off, so the node cannot
// look up the vote tx. The task then confirms the vote from the gov module's
// recorded choice: Complete once the vote reads the requested option.
func TestGovVoteUnverifiableTxConfirmedByVoteQuery(t *testing.T) {
	h := newGovVoteHarness(t, &SignAndBroadcastResult{TxHash: "ABCD", Unverifiable: true},
		nil, recordedVote(govtypes.OptionYes))
	out, err := runGovVote(t, h.g, "yes")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if out == nil || out.TxHash != "ABCD" || out.InclusionStatus != wire.InclusionUnverifiable {
		t.Errorf("result = %+v; want the tx hash, still marked unverifiable", out)
	}
	if h.reads < 2 {
		t.Errorf("read the vote %d times, want at least 2 (not yet recorded, then recorded)", h.reads)
	}
}

// A recorded vote with a different option is not this vote: the task keeps
// the unverifiable failure, and the operator checks the tx.
func TestGovVoteUnverifiableTxDifferentOptionStaysUnverifiable(t *testing.T) {
	h := newGovVoteHarness(t, &SignAndBroadcastResult{TxHash: "ABCD", Unverifiable: true}, recordedVote(govtypes.OptionNo))
	out, err := runGovVote(t, h.g, "yes")
	if !IsTerminal(err) || !strings.Contains(err.Error(), "inclusion unverifiable") {
		t.Fatalf("want terminal inclusion-unverifiable error, got %v", err)
	}
	if out == nil || out.TxHash != "ABCD" {
		t.Errorf("result = %+v", out)
	}
}

// A vote the node can look up never reaches the vote query.
func TestGovVoteCommittedTxSkipsVoteQuery(t *testing.T) {
	now := time.Now()
	h := newGovVoteHarness(t, &SignAndBroadcastResult{TxHash: "ABCD", Height: 9, IncludedAt: &now})
	if _, err := runGovVote(t, h.g, "yes"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if h.reads != 0 {
		t.Errorf("read the vote %d times, want 0", h.reads)
	}
}
