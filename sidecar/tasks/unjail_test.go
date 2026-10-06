package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

var (
	testBlockTime = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	releasable    = jailState{Jailed: true, JailedUntil: testBlockTime.Add(-time.Minute), BlockTime: testBlockTime}
)

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
		{name: "validator not jailed", st: jailState{BlockTime: testBlockTime}, keyName: "node_admin", terminal: true},
		{name: "validator tombstoned", st: jailState{Jailed: true, Tombstoned: true, BlockTime: testBlockTime}, keyName: "node_admin", terminal: true},
		{name: "jail period not over", st: jailState{Jailed: true, JailedUntil: testBlockTime.Add(time.Hour), BlockTime: testBlockTime}, keyName: "node_admin", terminal: true},
		{name: "local node catching up", st: jailState{Jailed: true, CatchingUp: true, BlockTime: testBlockTime}, keyName: "node_admin"},
		{name: "chain read fails", readErr: errors.New("connection refused"), keyName: "node_admin"},
		{name: "key missing from keyring", st: releasable, keyName: "does-not-exist", terminal: true},
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
	h, addr := newUnjailHarness(t, releasable, nil, committed(0))
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
	h, _ := newUnjailHarness(t, releasable, nil, committed(5))
	out, err := runUnjail(t, h.u, "node_admin")
	if !IsTerminal(err) {
		t.Fatalf("want Terminal, got %v", err)
	}
	if out == nil || out.TxHash != "ABCD" || out.InclusionStatus != wire.InclusionCommittedFailed {
		t.Errorf("result = %+v", out)
	}
}

// A rehydrated run must adopt the first run's tx, not re-check the jail: the
// first unjail may already have released the validator.
func TestUnjailWithTxMarkerSkipsJailCheck(t *testing.T) {
	h, _ := newUnjailHarness(t, jailState{BlockTime: testBlockTime}, nil, committed(0))
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

// --- readJailState ---

type fakeStakingQuery struct {
	stakingtypes.QueryClient
	validator *stakingtypes.Validator
	err       error
}

func (f *fakeStakingQuery) Validator(context.Context, *stakingtypes.QueryValidatorRequest, ...grpc.CallOption) (*stakingtypes.QueryValidatorResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &stakingtypes.QueryValidatorResponse{Validator: *f.validator}, nil
}

type fakeSlashingQuery struct {
	slashingtypes.QueryClient
	info    slashingtypes.ValidatorSigningInfo
	calls   int
	gotCons string
}

func (f *fakeSlashingQuery) SigningInfo(_ context.Context, req *slashingtypes.QuerySigningInfoRequest, _ ...grpc.CallOption) (*slashingtypes.QuerySigningInfoResponse, error) {
	f.calls++
	f.gotCons = req.ConsAddress
	return &slashingtypes.QuerySigningInfoResponse{ValSigningInfo: f.info}, nil
}

func statusAt(blockTime time.Time, catchingUp bool) func(context.Context) (*coretypes.ResultStatus, error) {
	return func(context.Context) (*coretypes.ResultStatus, error) {
		return &coretypes.ResultStatus{SyncInfo: coretypes.SyncInfo{LatestBlockTime: blockTime, CatchingUp: catchingUp}}, nil
	}
}

func TestReadJailState(t *testing.T) {
	_, addr := testKeyring(t)
	valAddr := sdk.ValAddress(addr)
	consKey := ed25519.GenPrivKey().PubKey()
	newValidator := func(jailed bool) *stakingtypes.Validator {
		v, err := stakingtypes.NewValidator(valAddr, consKey, stakingtypes.Description{Moniker: "v"})
		if err != nil {
			t.Fatal(err)
		}
		v.Jailed = jailed
		return &v
	}
	until := testBlockTime.Add(10 * time.Minute)

	t.Run("jailed reads signing info by consensus address", func(t *testing.T) {
		slash := &fakeSlashingQuery{info: slashingtypes.ValidatorSigningInfo{JailedUntil: until, Tombstoned: true}}
		st, err := readJailState(context.Background(), statusAt(testBlockTime, true),
			&fakeStakingQuery{validator: newValidator(true)}, slash, valAddr)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		want := jailState{Jailed: true, Tombstoned: true, JailedUntil: until, BlockTime: testBlockTime, CatchingUp: true}
		if st != want {
			t.Errorf("state = %+v, want %+v", st, want)
		}
		if wantCons := sdk.ConsAddress(consKey.Address()).String(); slash.gotCons != wantCons {
			t.Errorf("signing-info cons address = %q, want %q", slash.gotCons, wantCons)
		}
	})

	t.Run("not jailed skips signing info", func(t *testing.T) {
		slash := &fakeSlashingQuery{}
		st, err := readJailState(context.Background(), statusAt(testBlockTime, false),
			&fakeStakingQuery{validator: newValidator(false)}, slash, valAddr)
		if err != nil || st.Jailed || slash.calls != 0 {
			t.Errorf("state = %+v, err = %v, signing-info calls = %d", st, err, slash.calls)
		}
	})

	t.Run("NotFound code maps to errNoValidator", func(t *testing.T) {
		_, err := readJailState(context.Background(), statusAt(testBlockTime, false),
			&fakeStakingQuery{err: status.Error(codes.NotFound, "gone")}, &fakeSlashingQuery{}, valAddr)
		if !errors.Is(err, errNoValidator) {
			t.Errorf("err = %v, want errNoValidator", err)
		}
	})

	t.Run("not-found message maps to errNoValidator", func(t *testing.T) {
		_, err := readJailState(context.Background(), statusAt(testBlockTime, false),
			&fakeStakingQuery{err: fmt.Errorf("rpc error: validator %s not found", valAddr)}, &fakeSlashingQuery{}, valAddr)
		if !errors.Is(err, errNoValidator) {
			t.Errorf("err = %v, want errNoValidator", err)
		}
	})

	t.Run("other query error passes through", func(t *testing.T) {
		_, err := readJailState(context.Background(), statusAt(testBlockTime, false),
			&fakeStakingQuery{err: errors.New("connection refused")}, &fakeSlashingQuery{}, valAddr)
		if err == nil || errors.Is(err, errNoValidator) {
			t.Errorf("err = %v, want a pass-through error", err)
		}
	})
}
