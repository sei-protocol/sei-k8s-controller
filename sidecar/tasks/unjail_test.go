package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sei-protocol/sei-chain/sei-cosmos/crypto/keys/ed25519"
	sdk "github.com/sei-protocol/sei-chain/sei-cosmos/types"
	slashingtypes "github.com/sei-protocol/sei-chain/sei-cosmos/x/slashing/types"
	stakingtypes "github.com/sei-protocol/sei-chain/sei-cosmos/x/staking/types"
	"github.com/sei-protocol/sei-chain/sei-tendermint/rpc/coretypes"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sei-protocol/sei-k8s-controller/sidecar/engine"
	"github.com/sei-protocol/sei-k8s-controller/sidecarapi/wire"
)

const unjailTaskID = "11111111-2222-3333-4444-555555555555"

var testBlockTime = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

// releasable is a jail state the keeper would unjail: self-bond at the
// minimum, jailed, and past its jail period.
func releasable() jailState {
	return jailState{
		BlockTime:         testBlockTime,
		HasSelfDelegation: true,
		SelfBond:          sdk.NewInt(1_000_000),
		MinSelfBond:       sdk.NewInt(1_000_000),
		Jailed:            true,
		JailedUntil:       testBlockTime.Add(-time.Minute),
	}
}

// with returns releasable() changed by edit.
func with(edit func(*jailState)) jailState {
	st := releasable()
	edit(&st)
	return st
}

// unjailHarness wires an Unjailer with fake chain reads and a fake broadcast,
// and records what each saw.
type unjailHarness struct {
	u          *Unjailer
	ckpt       *fakeCheckpointer
	reads      int
	broadcasts []SignAndBroadcastInput
}

func newUnjailHarness(t *testing.T, st jailState, readErr error, result *SignAndBroadcastResult) (*unjailHarness, sdk.AccAddress) {
	t.Helper()
	kr, addr := testKeyring(t)
	h := &unjailHarness{ckpt: newFakeCheckpointer(nil)}
	h.u = &Unjailer{
		cfg: engine.ExecutionConfig{Keyring: kr, Checkpointer: h.ckpt},
		readJail: func(context.Context, engine.ExecutionConfig, string, sdk.ValAddress) (jailState, error) {
			h.reads++
			return st, readErr
		},
		broadcast: func(_ context.Context, _ engine.ExecutionConfig, in SignAndBroadcastInput) (*SignAndBroadcastResult, error) {
			h.broadcasts = append(h.broadcasts, in)
			return result, nil
		},
	}
	return h, addr
}

func runUnjail(t *testing.T, u *Unjailer, keyName string) (*wire.GovTxResult, error) {
	t.Helper()
	ctx := engine.WithTaskID(context.Background(), unjailTaskID)
	raw, err := u.Handler()(ctx, map[string]any{
		"chainId": "sei-test", "keyName": keyName, "fees": "4000usei", "gas": 200000,
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

func committed(code uint32) *SignAndBroadcastResult {
	now := time.Now()
	return &SignAndBroadcastResult{TxHash: "ABCD", Height: 99, Code: code, IncludedAt: &now}
}

func TestUnjailRefusesBeforeBroadcast(t *testing.T) {
	cases := []struct {
		name     string
		st       jailState
		readErr  error
		keyName  string
		terminal bool
	}{
		{name: "no validator for the operator account", readErr: errNoValidator, keyName: "node_admin", terminal: true},
		{name: "no self-delegation", st: with(func(s *jailState) { s.HasSelfDelegation = false }), keyName: "node_admin", terminal: true},
		{name: "self-delegation below min", st: with(func(s *jailState) { s.SelfBond = sdk.NewInt(999_999) }), keyName: "node_admin", terminal: true},
		{name: "validator not jailed", st: with(func(s *jailState) { s.Jailed = false }), keyName: "node_admin", terminal: true},
		{name: "validator tombstoned", st: with(func(s *jailState) { s.Tombstoned = true }), keyName: "node_admin", terminal: true},
		{name: "jail period not over", st: with(func(s *jailState) { s.JailedUntil = testBlockTime.Add(time.Hour) }), keyName: "node_admin", terminal: true},
		{name: "local node catching up", st: jailState{CatchingUp: true, BlockTime: testBlockTime}, keyName: "node_admin"},
		{name: "chain read fails", readErr: errors.New("connection refused"), keyName: "node_admin"},
		{name: "key missing from keyring", st: releasable(), keyName: "does-not-exist", terminal: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newUnjailHarness(t, tc.st, tc.readErr, committed(0))
			_, err := runUnjail(t, h.u, tc.keyName)
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			if IsTerminal(err) != tc.terminal {
				t.Errorf("IsTerminal = %v, want %v (err: %v)", IsTerminal(err), tc.terminal, err)
			}
			if len(h.broadcasts) != 0 {
				t.Errorf("broadcast %d times, want 0", len(h.broadcasts))
			}
		})
	}
}

func TestUnjailNilKeyringIsTerminal(t *testing.T) {
	u := &Unjailer{
		readJail: func(context.Context, engine.ExecutionConfig, string, sdk.ValAddress) (jailState, error) {
			t.Fatal("must not read chain state without a keyring")
			return jailState{}, nil
		},
	}
	if _, err := runUnjail(t, u, "node_admin"); !IsTerminal(err) {
		t.Fatalf("want Terminal, got %v", err)
	}
}

func TestUnjailBroadcastsMsgUnjailForTheOperatorsValidator(t *testing.T) {
	h, addr := newUnjailHarness(t, releasable(), nil, committed(0))
	out, err := runUnjail(t, h.u, "node_admin")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(h.broadcasts) != 1 {
		t.Fatalf("broadcast %d times, want 1", len(h.broadcasts))
	}
	in := h.broadcasts[0]
	msg, ok := in.Msg.(*slashingtypes.MsgUnjail)
	if !ok {
		t.Fatalf("msg = %T, want *MsgUnjail", in.Msg)
	}
	if want := sdk.ValAddress(addr).String(); msg.ValidatorAddr != want {
		t.Errorf("validatorAddr = %q, want %q", msg.ValidatorAddr, want)
	}
	if in.TaskID != unjailTaskID || in.KeyName != "node_admin" || in.Fees != "4000usei" || in.Gas != 200000 {
		t.Errorf("broadcast input = %+v", in)
	}
	if out.TxHash != "ABCD" || out.Height != 99 || out.InclusionStatus != wire.InclusionCommittedOK {
		t.Errorf("result = %+v", out)
	}
}

func TestUnjailCommittedFailureIsTerminalAndKeepsTxHash(t *testing.T) {
	h, _ := newUnjailHarness(t, releasable(), nil, committed(5))
	out, err := runUnjail(t, h.u, "node_admin")
	if !IsTerminal(err) {
		t.Fatalf("want Terminal, got %v", err)
	}
	if out == nil || out.TxHash != "ABCD" || out.InclusionStatus != wire.InclusionCommittedFailed {
		t.Errorf("result = %+v", out)
	}
}

// PLT-1392 harbor e2e: validators often run with the tx index off, so the
// node cannot look up the unjail tx. The task then confirms the release from
// the jail state: Complete once the validator reads released.
func TestUnjailUnverifiableTxConfirmedByJailState(t *testing.T) {
	kr, _ := testKeyring(t)
	states := []jailState{releasable(), releasable(), with(func(s *jailState) { s.Jailed = false })}
	reads := 0
	u := &Unjailer{
		cfg: engine.ExecutionConfig{Keyring: kr, Checkpointer: newFakeCheckpointer(nil)},
		readJail: func(context.Context, engine.ExecutionConfig, string, sdk.ValAddress) (jailState, error) {
			st := states[min(reads, len(states)-1)]
			reads++
			return st, nil
		},
		broadcast: func(context.Context, engine.ExecutionConfig, SignAndBroadcastInput) (*SignAndBroadcastResult, error) {
			return &SignAndBroadcastResult{TxHash: "ABCD", Unverifiable: true}, nil
		},
		confirmWait:  time.Second,
		confirmEvery: time.Millisecond,
	}
	out, err := runUnjail(t, u, "node_admin")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if out == nil || out.TxHash != "ABCD" || out.InclusionStatus != wire.InclusionUnverifiable {
		t.Errorf("result = %+v; want the tx hash, still marked unverifiable", out)
	}
	if reads != 3 {
		t.Errorf("read jail state %d times, want 3 (pre-check, then two polls)", reads)
	}
}

// A validator that still reads jailed when the wait ends keeps the
// unverifiable failure: the operator must check the tx through an indexed RPC.
func TestUnjailUnverifiableTxStillJailedStaysUnverifiable(t *testing.T) {
	h, _ := newUnjailHarness(t, releasable(), nil, &SignAndBroadcastResult{TxHash: "ABCD", Unverifiable: true})
	h.u.confirmWait = 20 * time.Millisecond
	h.u.confirmEvery = 5 * time.Millisecond
	out, err := runUnjail(t, h.u, "node_admin")
	if !IsTerminal(err) || !strings.Contains(err.Error(), "inclusion unverifiable") {
		t.Fatalf("want terminal inclusion-unverifiable error, got %v", err)
	}
	if out == nil || out.TxHash != "ABCD" {
		t.Errorf("result = %+v", out)
	}
	if h.reads < 2 {
		t.Errorf("read jail state %d times, want the pre-check plus at least one poll", h.reads)
	}
}

// A rehydrated run must adopt the first run's tx, not re-check the jail: the
// first unjail may already have released the validator.
func TestUnjailWithTxMarkerSkipsJailCheck(t *testing.T) {
	h, _ := newUnjailHarness(t, with(func(s *jailState) { s.Jailed = false }), nil, committed(0))
	if err := h.ckpt.SaveTxMarker(&engine.TxMarker{TaskID: unjailTaskID, TxHash: "ABCD"}); err != nil {
		t.Fatal(err)
	}
	if _, err := runUnjail(t, h.u, "node_admin"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if h.reads != 0 {
		t.Errorf("read jail state %d times, want 0", h.reads)
	}
	if len(h.broadcasts) != 1 {
		t.Errorf("broadcast %d times, want 1", len(h.broadcasts))
	}
}

// A node that is catching up must report "catching up", not a terminal "no
// validator": it answers from a height where the validator may not exist yet.
func TestUnjailCatchingUpWinsOverValidatorNotFound(t *testing.T) {
	h, _ := newUnjailHarness(t, jailState{}, nil, committed(0))
	staking := &fakeStakingQuery{validatorErr: status.Error(codes.NotFound, "validator not found")}
	h.u.readJail = func(ctx context.Context, _ engine.ExecutionConfig, _ string, valAddr sdk.ValAddress) (jailState, error) {
		return readJailState(ctx, statusAt(testBlockTime, true), staking, &fakeSlashingQuery{}, newSignTxInterfaceRegistry(), valAddr)
	}
	_, err := runUnjail(t, h.u, "node_admin")
	if err == nil || IsTerminal(err) {
		t.Fatalf("want a non-terminal catching-up error, got %v", err)
	}
	if !strings.Contains(err.Error(), "catching up") {
		t.Errorf("err = %v, want the catching-up refusal", err)
	}
	if staking.validatorCalls != 0 || len(h.broadcasts) != 0 {
		t.Errorf("validator queries = %d, broadcasts = %d; want 0 and 0", staking.validatorCalls, len(h.broadcasts))
	}
}

// A jailed validator with no signing info was never bonded. The keeper lets it
// unjail at any time, so the task broadcasts.
func TestUnjailJailedWithoutSigningInfoBroadcasts(t *testing.T) {
	h, addr := newUnjailHarness(t, jailState{}, nil, committed(0))
	valAddr := sdk.ValAddress(addr)
	v := newTestValidator(t, valAddr, true, sdk.NewInt(1_000_000), sdk.NewDec(1_000_000), sdk.NewInt(1_000_000))
	staking := &fakeStakingQuery{validator: v, delegation: selfDelegation(valAddr, sdk.NewDec(1_000_000))}
	slashing := &fakeSlashingQuery{err: status.Error(codes.NotFound, "SigningInfo not found for validator")}
	h.u.readJail = func(ctx context.Context, _ engine.ExecutionConfig, _ string, va sdk.ValAddress) (jailState, error) {
		return readJailState(ctx, statusAt(testBlockTime, false), staking, slashing, newSignTxInterfaceRegistry(), va)
	}
	if _, err := runUnjail(t, h.u, "node_admin"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(h.broadcasts) != 1 {
		t.Errorf("broadcast %d times, want 1", len(h.broadcasts))
	}
}

// --- readJailState ---

type fakeStakingQuery struct {
	stakingtypes.QueryClient
	validator      *stakingtypes.Validator
	validatorErr   error
	validatorCalls int
	delegation     *stakingtypes.DelegationResponse
	delegationErr  error
	gotDelegator   string
}

func (f *fakeStakingQuery) Validator(context.Context, *stakingtypes.QueryValidatorRequest, ...grpc.CallOption) (*stakingtypes.QueryValidatorResponse, error) {
	f.validatorCalls++
	if f.validatorErr != nil {
		return nil, f.validatorErr
	}
	return &stakingtypes.QueryValidatorResponse{Validator: *f.validator}, nil
}

func (f *fakeStakingQuery) Delegation(_ context.Context, req *stakingtypes.QueryDelegationRequest, _ ...grpc.CallOption) (*stakingtypes.QueryDelegationResponse, error) {
	f.gotDelegator = req.DelegatorAddr
	if f.delegationErr != nil {
		return nil, f.delegationErr
	}
	return &stakingtypes.QueryDelegationResponse{DelegationResponse: f.delegation}, nil
}

type fakeSlashingQuery struct {
	slashingtypes.QueryClient
	info    slashingtypes.ValidatorSigningInfo
	err     error
	calls   int
	gotCons string
}

func (f *fakeSlashingQuery) SigningInfo(_ context.Context, req *slashingtypes.QuerySigningInfoRequest, _ ...grpc.CallOption) (*slashingtypes.QuerySigningInfoResponse, error) {
	f.calls++
	f.gotCons = req.ConsAddress
	if f.err != nil {
		return nil, f.err
	}
	return &slashingtypes.QuerySigningInfoResponse{ValSigningInfo: f.info}, nil
}

func statusAt(blockTime time.Time, catchingUp bool) func(context.Context) (*coretypes.ResultStatus, error) {
	return func(context.Context) (*coretypes.ResultStatus, error) {
		return &coretypes.ResultStatus{SyncInfo: coretypes.SyncInfo{LatestBlockTime: blockTime, CatchingUp: catchingUp}}, nil
	}
}

var testConsKey = ed25519.GenPrivKey().PubKey()

// newTestValidator builds a validator whose consensus key unpacks, with the
// given tokens, delegator shares, and min self-delegation.
func newTestValidator(t *testing.T, valAddr sdk.ValAddress, jailed bool, tokens sdk.Int, shares sdk.Dec, minSelf sdk.Int) *stakingtypes.Validator {
	t.Helper()
	v, err := stakingtypes.NewValidator(valAddr, testConsKey, stakingtypes.Description{Moniker: "v"})
	if err != nil {
		t.Fatal(err)
	}
	v.Jailed = jailed
	v.Tokens = tokens
	v.DelegatorShares = shares
	v.MinSelfDelegation = minSelf
	return &v
}

func selfDelegation(valAddr sdk.ValAddress, shares sdk.Dec) *stakingtypes.DelegationResponse {
	return &stakingtypes.DelegationResponse{Delegation: stakingtypes.Delegation{
		DelegatorAddress: sdk.AccAddress(valAddr).String(),
		ValidatorAddress: valAddr.String(),
		Shares:           shares,
	}}
}

func TestReadJailState(t *testing.T) {
	_, addr := testKeyring(t)
	valAddr := sdk.ValAddress(addr)
	until := testBlockTime.Add(10 * time.Minute)
	// Exchange rate 0.5: 2,000 shares back 1,000 tokens.
	newV := func(jailed bool) *stakingtypes.Validator {
		return newTestValidator(t, valAddr, jailed, sdk.NewInt(1_000), sdk.NewDec(2_000), sdk.NewInt(700))
	}
	read := func(catchingUp bool, staking *fakeStakingQuery, slashing *fakeSlashingQuery) (jailState, error) {
		return readJailState(context.Background(), statusAt(testBlockTime, catchingUp), staking, slashing, newSignTxInterfaceRegistry(), valAddr)
	}

	// PLT-1392 harbor e2e: a validator from a real query reply carries its
	// consensus key packed, because sei-cosmos's QueryValidatorResponse does
	// not implement UnpackInterfaces. The read must unpack it itself.
	t.Run("jailed validator decoded from the wire still resolves its consensus address", func(t *testing.T) {
		bz, err := newV(true).Marshal()
		if err != nil {
			t.Fatal(err)
		}
		var fromWire stakingtypes.Validator
		if err := fromWire.Unmarshal(bz); err != nil {
			t.Fatal(err)
		}
		if _, err := fromWire.GetConsAddr(); err == nil {
			t.Fatal("precondition: a validator decoded from the wire should carry its consensus key packed")
		}
		staking := &fakeStakingQuery{validator: &fromWire, delegation: selfDelegation(valAddr, sdk.NewDec(1_500))}
		slash := &fakeSlashingQuery{info: slashingtypes.ValidatorSigningInfo{JailedUntil: until}}
		st, err := read(false, staking, slash)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !st.Jailed || !st.JailedUntil.Equal(until) {
			t.Errorf("state = %+v", st)
		}
		if wantCons := sdk.ConsAddress(testConsKey.Address()).String(); slash.gotCons != wantCons {
			t.Errorf("signing-info cons address = %q, want %q", slash.gotCons, wantCons)
		}
	})

	t.Run("jailed reads self-bond at the exchange rate and signing info by consensus address", func(t *testing.T) {
		staking := &fakeStakingQuery{validator: newV(true), delegation: selfDelegation(valAddr, sdk.NewDec(1_500))}
		slash := &fakeSlashingQuery{info: slashingtypes.ValidatorSigningInfo{JailedUntil: until, Tombstoned: true}}
		st, err := read(false, staking, slash)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !st.Jailed || !st.Tombstoned || !st.JailedUntil.Equal(until) || !st.BlockTime.Equal(testBlockTime) || st.CatchingUp {
			t.Errorf("state = %+v", st)
		}
		if !st.HasSelfDelegation || !st.SelfBond.Equal(sdk.NewInt(750)) || !st.MinSelfBond.Equal(sdk.NewInt(700)) {
			t.Errorf("self-bond = %v %s, min = %s; want true 750, 700", st.HasSelfDelegation, st.SelfBond, st.MinSelfBond)
		}
		if want := sdk.AccAddress(valAddr).String(); staking.gotDelegator != want {
			t.Errorf("delegation queried for %q, want the operator account %q", staking.gotDelegator, want)
		}
		if wantCons := sdk.ConsAddress(testConsKey.Address()).String(); slash.gotCons != wantCons {
			t.Errorf("signing-info cons address = %q, want %q", slash.gotCons, wantCons)
		}
	})

	t.Run("self-bond truncates like the keeper", func(t *testing.T) {
		// 3 shares at rate 1000/2000 = 1.5 tokens, truncated to 1.
		if got := selfBondTokens(*newV(true), sdk.NewDec(3)); !got.Equal(sdk.NewInt(1)) {
			t.Errorf("selfBondTokens = %s, want 1", got)
		}
	})

	t.Run("no self-delegation leaves HasSelfDelegation false", func(t *testing.T) {
		staking := &fakeStakingQuery{validator: newV(true), delegationErr: status.Error(codes.NotFound, "delegation not found")}
		st, err := read(false, staking, &fakeSlashingQuery{})
		if err != nil || st.HasSelfDelegation {
			t.Errorf("state = %+v, err = %v; want no self-delegation and no error", st, err)
		}
	})

	t.Run("self-delegation query error passes through", func(t *testing.T) {
		staking := &fakeStakingQuery{validator: newV(true), delegationErr: errors.New("connection refused")}
		if _, err := read(false, staking, &fakeSlashingQuery{}); err == nil {
			t.Error("want an error, got nil")
		}
	})

	t.Run("jailed without signing info leaves jail fields zero", func(t *testing.T) {
		staking := &fakeStakingQuery{validator: newV(true), delegation: selfDelegation(valAddr, sdk.NewDec(1_500))}
		slash := &fakeSlashingQuery{err: status.Error(codes.NotFound, "SigningInfo not found for validator")}
		st, err := read(false, staking, slash)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !st.Jailed || st.Tombstoned || !st.JailedUntil.IsZero() {
			t.Errorf("state = %+v; want jailed, not tombstoned, zero JailedUntil", st)
		}
	})

	t.Run("signing info query error passes through", func(t *testing.T) {
		staking := &fakeStakingQuery{validator: newV(true), delegation: selfDelegation(valAddr, sdk.NewDec(1_500))}
		if _, err := read(false, staking, &fakeSlashingQuery{err: errors.New("connection refused")}); err == nil {
			t.Error("want an error, got nil")
		}
	})

	t.Run("not jailed skips signing info", func(t *testing.T) {
		staking := &fakeStakingQuery{validator: newV(false), delegation: selfDelegation(valAddr, sdk.NewDec(1_500))}
		slash := &fakeSlashingQuery{}
		st, err := read(false, staking, slash)
		if err != nil || st.Jailed || slash.calls != 0 {
			t.Errorf("state = %+v, err = %v, signing-info calls = %d", st, err, slash.calls)
		}
	})

	t.Run("catching up reads nothing past status", func(t *testing.T) {
		staking := &fakeStakingQuery{validatorErr: status.Error(codes.NotFound, "gone")}
		st, err := read(true, staking, &fakeSlashingQuery{})
		if err != nil || !st.CatchingUp || staking.validatorCalls != 0 {
			t.Errorf("state = %+v, err = %v, validator calls = %d", st, err, staking.validatorCalls)
		}
	})

	t.Run("NotFound code maps to errNoValidator", func(t *testing.T) {
		_, err := read(false, &fakeStakingQuery{validatorErr: status.Error(codes.NotFound, "gone")}, &fakeSlashingQuery{})
		if !errors.Is(err, errNoValidator) {
			t.Errorf("err = %v, want errNoValidator", err)
		}
	})

	t.Run("not-found message maps to errNoValidator", func(t *testing.T) {
		_, err := read(false, &fakeStakingQuery{validatorErr: fmt.Errorf("rpc error: validator %s not found", valAddr)}, &fakeSlashingQuery{})
		if !errors.Is(err, errNoValidator) {
			t.Errorf("err = %v, want errNoValidator", err)
		}
	})

	t.Run("other validator query error passes through", func(t *testing.T) {
		_, err := read(false, &fakeStakingQuery{validatorErr: errors.New("connection refused")}, &fakeSlashingQuery{})
		if err == nil || errors.Is(err, errNoValidator) {
			t.Errorf("err = %v, want a pass-through error", err)
		}
	})
}
