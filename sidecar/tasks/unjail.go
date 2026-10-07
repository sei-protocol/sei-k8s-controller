// Package tasks — unjail handler.
//
// This handler signs MsgUnjail as the validator's operator account, through
// the same sign-tx path as gov-vote. API authentication is controlled by
// SEI_SIDECAR_AUTHN_MODE; see sidecar/server/auth.go.

package tasks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	codectypes "github.com/sei-protocol/sei-chain/sei-cosmos/codec/types"
	sdk "github.com/sei-protocol/sei-chain/sei-cosmos/types"
	slashingtypes "github.com/sei-protocol/sei-chain/sei-cosmos/x/slashing/types"
	stakingtypes "github.com/sei-protocol/sei-chain/sei-cosmos/x/staking/types"
	"github.com/sei-protocol/sei-chain/sei-tendermint/rpc/coretypes"

	"github.com/sei-protocol/seilog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sei-protocol/sei-k8s-controller/sidecar/engine"
	"github.com/sei-protocol/sei-k8s-controller/sidecarapi/wire"
)

var unjailLog = seilog.NewLogger("seictl", "task", "unjail")

// UnjailRequest holds unjail params. The validator is the one whose operator
// account KeyName names; the request carries no validator address of its own.
type UnjailRequest struct {
	ChainID string `json:"chainId"`
	KeyName string `json:"keyName"`
	Memo    string `json:"memo,omitempty"`
	Fees    string `json:"fees"`
	Gas     uint64 `json:"gas"`
}

// jailState is the chain state the pre-broadcast check reads. BlockTime is the
// local node's latest block time: the chain compares JailedUntil against block
// time, not wall-clock time. A catching-up node's state holds only CatchingUp
// and BlockTime; the rest is not read.
type jailState struct {
	CatchingUp bool
	BlockTime  time.Time

	// HasSelfDelegation, SelfBond, and MinSelfBond feed the keeper's
	// self-delegation checks. SelfBond is the operator's self-delegated tokens,
	// converted from shares and truncated as the keeper does.
	HasSelfDelegation bool
	SelfBond          sdk.Int
	MinSelfBond       sdk.Int

	Jailed bool
	// Tombstoned and JailedUntil come from the validator's signing info. A
	// jailed validator with no signing info leaves both at zero.
	Tombstoned  bool
	JailedUntil time.Time
}

// errNoValidator reports that the operator account has no validator record.
var errNoValidator = errors.New("no validator for this operator account")

// Unjailer captures cfg by value at construction; engine.Config is documented
// read-only after startup, so the copy is safe.
type Unjailer struct {
	cfg engine.ExecutionConfig

	// readJail and broadcast are test seams. They default to the chain query
	// and SignAndBroadcast.
	readJail  func(ctx context.Context, cfg engine.ExecutionConfig, chainID string, valAddr sdk.ValAddress) (jailState, error)
	broadcast func(ctx context.Context, cfg engine.ExecutionConfig, in SignAndBroadcastInput) (*SignAndBroadcastResult, error)

	// confirmWait bounds how long an unjail whose tx the node cannot look up
	// waits for the validator to read released; confirmEvery is the poll
	// interval. Zero values check once.
	confirmWait  time.Duration
	confirmEvery time.Duration
}

func NewUnjailer(cfg engine.ExecutionConfig) *Unjailer {
	return &Unjailer{
		cfg:          cfg,
		readJail:     chainJailState,
		broadcast:    SignAndBroadcast,
		confirmWait:  30 * time.Second,
		confirmEvery: time.Second,
	}
}

// Handler checks the validator's jail state, then delegates to
// SignAndBroadcast.
//
// MsgUnjail is not chain-idempotent: a second unjail of a released validator
// commits with a non-zero code and spends the fee. The pre-broadcast check
// therefore refuses a validator that is not jailed. A rehydrated run that finds
// a tx marker skips the check and adopts the marker instead: the first run's
// unjail may already have released the validator, and the check would then
// misreport a landed unjail as "not jailed".
func (u *Unjailer) Handler() engine.TaskHandler {
	return engine.TypedHandlerWithResult(func(ctx context.Context, params UnjailRequest) (*wire.GovTxResult, error) {
		taskID := engine.TaskIDFromContext(ctx)
		valAddr, err := operatorValAddr(u.cfg, params.KeyName)
		if err != nil {
			return nil, err
		}
		adopting, err := hasTxMarker(u.cfg, taskID)
		if err != nil {
			return nil, err
		}
		if !adopting {
			if err := u.checkJailed(ctx, params.ChainID, valAddr); err != nil {
				return nil, err
			}
		}
		result, err := u.broadcast(ctx, u.cfg, SignAndBroadcastInput{
			ChainID: params.ChainID,
			KeyName: params.KeyName,
			Msg:     slashingtypes.NewMsgUnjail(valAddr),
			Fees:    params.Fees,
			Gas:     params.Gas,
			Memo:    params.Memo,
			TaskID:  taskID,
		})
		if err != nil {
			return nil, err
		}
		out, cerr := classifyGovResult(engine.TaskUnjail, result)
		releasedByState := result.Unverifiable && u.releasedAfter(ctx, params.ChainID, valAddr)
		if releasedByState {
			cerr = nil
		}
		unjailLog.Info("unjail broadcast",
			"taskId", taskID,
			"chainId", params.ChainID,
			"validator", valAddr.String(),
			"txHash", out.TxHash,
			"height", out.Height,
			"inclusionStatus", out.InclusionStatus,
			"releasedByState", releasedByState)
		return out, cerr
	})
}

// releasedAfter settles an unjail whose tx the node cannot look up because its
// tx index is off, as on most validators. The unjail's effect shows in state
// instead: a validator that reads not jailed was released. An unjail from
// elsewhere that lands first reads the same, and the validator is released
// either way. It polls until confirmWait passes, and checks at least once.
func (u *Unjailer) releasedAfter(ctx context.Context, chainID string, valAddr sdk.ValAddress) bool {
	deadline := time.Now().Add(u.confirmWait)
	for {
		st, err := u.readJail(ctx, u.cfg, chainID, valAddr)
		if err == nil && !st.CatchingUp && !st.Jailed {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(u.confirmEvery):
		}
	}
}

// checkJailed refuses an unjail that the chain would reject after taking the
// fee. CheckTx does not run the message handler, so these cases otherwise
// surface only as a committed-but-failed tx. After the catching-up check, the
// cases follow the order of the slashing keeper's Unjail
// (sei-cosmos x/slashing/keeper/unjail.go): no validator, no self-delegation,
// self-delegation below MinSelfDelegation, not jailed, tombstoned, jail period
// not over.
func (u *Unjailer) checkJailed(ctx context.Context, chainID string, valAddr sdk.ValAddress) error {
	st, err := u.readJail(ctx, u.cfg, chainID, valAddr)
	if errors.Is(err, errNoValidator) {
		return Terminal(fmt.Errorf("operator account has no validator %s: unjail applies only to a validator", valAddr))
	}
	if err != nil {
		return fmt.Errorf("read jail state of %s: %w", valAddr, err)
	}
	switch {
	case st.CatchingUp:
		return fmt.Errorf("local seid is catching up, so its jail state for %s may be stale; retry when it is caught up", valAddr)
	case !st.HasSelfDelegation:
		return Terminal(fmt.Errorf("validator %s has no self-delegation; the chain rejects its unjail (ErrMissingSelfDelegation)", valAddr))
	case st.SelfBond.LT(st.MinSelfBond):
		return Terminal(fmt.Errorf("validator %s self-delegation %s is below its min self-delegation %s; the chain rejects its unjail (ErrSelfDelegationTooLowToUnjail)",
			valAddr, st.SelfBond, st.MinSelfBond))
	case !st.Jailed:
		return Terminal(fmt.Errorf("validator %s is not jailed; refusing to spend a fee on an unjail", valAddr))
	case st.Tombstoned:
		return Terminal(fmt.Errorf("validator %s is tombstoned; an unjail cannot release it", valAddr))
	case st.BlockTime.Before(st.JailedUntil):
		return Terminal(fmt.Errorf("validator %s stays jailed until %s; the latest block time is %s",
			valAddr, st.JailedUntil.UTC().Format(time.RFC3339), st.BlockTime.UTC().Format(time.RFC3339)))
	}
	return nil
}

// operatorValAddr resolves the validator operator address from the keyring
// entry. The operator address and the account address share their bytes.
func operatorValAddr(cfg engine.ExecutionConfig, keyName string) (sdk.ValAddress, error) {
	if cfg.Keyring == nil {
		return nil, Terminal(errors.New("keyring not configured: set SEI_KEYRING_BACKEND/SEI_KEYRING_PASSPHRASE on the sidecar"))
	}
	if keyName == "" {
		return nil, Terminal(errors.New("keyName required"))
	}
	info, err := cfg.Keyring.Key(keyName)
	if err != nil {
		return nil, Terminal(fmt.Errorf("keyring entry %q: %w", keyName, err))
	}
	return sdk.ValAddress(info.GetAddress()), nil
}

// hasTxMarker reports whether a prior run of this task persisted a tx marker.
func hasTxMarker(cfg engine.ExecutionConfig, taskID string) (bool, error) {
	if cfg.Checkpointer == nil || taskID == "" {
		return false, nil
	}
	marker, err := cfg.Checkpointer.GetTxMarker(taskID)
	if err != nil {
		return false, fmt.Errorf("read tx marker: %w", err)
	}
	return marker != nil, nil
}

// chainJailState reads the validator and its signing info from the local seid
// over gRPC-over-ABCI, and the latest block time from /status. The SDK query
// path does not honor ctx, so the reads run off-goroutine and ctx cancellation
// returns at once, as in sdkTxClient.AccountNumberSequence.
func chainJailState(ctx context.Context, cfg engine.ExecutionConfig, chainID string, valAddr sdk.ValAddress) (jailState, error) {
	clientCtx, err := newSignTxClientContext(cfg, SignAndBroadcastInput{ChainID: chainID}, sdk.AccAddress(valAddr))
	if err != nil {
		return jailState{}, err
	}

	type res struct {
		st  jailState
		err error
	}
	ch := make(chan res, 1)
	go func() {
		st, err := readJailState(ctx, clientCtx.Client.Status, stakingtypes.NewQueryClient(clientCtx), slashingtypes.NewQueryClient(clientCtx), clientCtx.InterfaceRegistry, valAddr)
		ch <- res{st, err}
	}()
	select {
	case <-ctx.Done():
		return jailState{}, ctx.Err()
	case r := <-ch:
		return r.st, r.err
	}
}

// readJailState reads the jail state through narrow seams, so a test can fake
// each read. unpacker decodes the validator's consensus key: sei-cosmos's
// QueryValidatorResponse does not implement UnpackInterfaces, so the query
// client returns the key still packed.
func readJailState(
	ctx context.Context,
	statusOf func(context.Context) (*coretypes.ResultStatus, error),
	staking stakingtypes.QueryClient,
	slashing slashingtypes.QueryClient,
	unpacker codectypes.AnyUnpacker,
	valAddr sdk.ValAddress,
) (jailState, error) {
	s, err := statusOf(ctx)
	if err != nil {
		return jailState{}, fmt.Errorf("query local seid /status: %w", err)
	}
	st := jailState{BlockTime: s.SyncInfo.LatestBlockTime, CatchingUp: s.SyncInfo.CatchingUp}
	// A syncing node answers from an old height, where the validator may not
	// exist yet. Read nothing more: checkJailed refuses on CatchingUp first.
	if st.CatchingUp {
		return st, nil
	}

	vres, err := staking.Validator(ctx, &stakingtypes.QueryValidatorRequest{ValidatorAddr: valAddr.String()})
	if err != nil {
		if isQueryNotFound(err, valAddr.String()+" not found") {
			return jailState{}, errNoValidator
		}
		return jailState{}, fmt.Errorf("query validator: %w", err)
	}
	v := vres.Validator
	st.Jailed = v.Jailed
	st.MinSelfBond = v.MinSelfDelegation

	dres, err := staking.Delegation(ctx, &stakingtypes.QueryDelegationRequest{
		DelegatorAddr: sdk.AccAddress(valAddr).String(),
		ValidatorAddr: valAddr.String(),
	})
	switch {
	case err == nil && dres.DelegationResponse != nil:
		st.HasSelfDelegation = true
		st.SelfBond = selfBondTokens(v, dres.DelegationResponse.Delegation.Shares)
	case err == nil, isQueryNotFound(err, "not found for validator "+valAddr.String()):
		// No self-delegation: HasSelfDelegation stays false.
	default:
		return jailState{}, fmt.Errorf("query self-delegation: %w", err)
	}
	if !st.Jailed {
		return st, nil
	}

	if err := v.UnpackInterfaces(unpacker); err != nil {
		return jailState{}, fmt.Errorf("unpack validator consensus key: %w", err)
	}
	consAddr, err := v.GetConsAddr()
	if err != nil {
		return jailState{}, fmt.Errorf("validator consensus address: %w", err)
	}
	sres, err := slashing.SigningInfo(ctx, &slashingtypes.QuerySigningInfoRequest{ConsAddress: consAddr.String()})
	if err != nil {
		// The keeper lets a jailed validator with no signing info unjail at
		// any time: it was never bonded, so it was jailed for falling below
		// its min self-delegation. Tombstoned and JailedUntil stay zero.
		if isQueryNotFound(err, "SigningInfo not found") {
			return st, nil
		}
		return jailState{}, fmt.Errorf("query signing info: %w", err)
	}
	st.Tombstoned = sres.ValSigningInfo.Tombstoned
	st.JailedUntil = sres.ValSigningInfo.JailedUntil
	return st, nil
}

// selfBondTokens converts delegation shares to tokens the way the keeper does:
// TokensFromShares at the validator's exchange rate, truncated to an integer.
func selfBondTokens(v stakingtypes.Validator, shares sdk.Dec) sdk.Int {
	if v.DelegatorShares.IsZero() {
		return sdk.ZeroInt()
	}
	return v.TokensFromShares(shares).TruncateInt()
}

// isQueryNotFound matches a query's missing-record answer. The gRPC NotFound
// code survives the ABCI round trip; the message match covers a node whose
// query path drops the code.
func isQueryNotFound(err error, msgFragment string) bool {
	return status.Code(err) == codes.NotFound || strings.Contains(err.Error(), msgFragment)
}
