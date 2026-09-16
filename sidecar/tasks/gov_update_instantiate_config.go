// Package tasks — gov-update-instantiate-config handler.
//
// This handler signs a wasm UpdateInstantiateConfigProposal as the validator's
// operator account. The proposal rewrites the instantiate permission frozen in
// each referenced code's CodeInfo; changing the wasm module's live default
// parameter does not update code that already exists.
//
// # REHYDRATION
//
// MsgSubmitProposal is NOT chain-idempotent. Crash-idempotency is provided by
// the pre-broadcast TxMarker + rehydrate-adopt in SignAndBroadcast: a re-run of
// the same task adopts its in-flight tx rather than signing a duplicate.
package tasks

import (
	"context"
	"errors"
	"fmt"

	sdk "github.com/sei-protocol/sei-chain/sei-cosmos/types"
	govtypes "github.com/sei-protocol/sei-chain/sei-cosmos/x/gov/types"
	wasmtypes "github.com/sei-protocol/sei-chain/sei-wasmd/x/wasm/types"

	"github.com/sei-protocol/seilog"

	"github.com/sei-protocol/sei-k8s-controller/sidecar/engine"
	"github.com/sei-protocol/sei-k8s-controller/sidecarapi/wire"
)

var govUpdateInstantiateConfigLog = seilog.NewLogger("seictl", "task", "gov-update-instantiate-config")

type instantiateConfigUpdate struct {
	CodeID     uint64 `json:"codeId"`
	Permission string `json:"permission"`
}

// GovUpdateInstantiateConfigRequest holds gov-update-instantiate-config params.
type GovUpdateInstantiateConfigRequest struct {
	ChainID string `json:"chainId"`
	KeyName string `json:"keyName"`

	Title       string `json:"title"`
	Description string `json:"description"`

	Updates []instantiateConfigUpdate `json:"updates"`

	InitialDeposit string `json:"initialDeposit"`

	Memo string `json:"memo,omitempty"`
	Fees string `json:"fees"`
	Gas  uint64 `json:"gas"`
}

// GovInstantiateConfigUpdater captures cfg by value at construction;
// engine.ExecutionConfig is read-only after startup.
type GovInstantiateConfigUpdater struct {
	cfg engine.ExecutionConfig
}

func NewGovInstantiateConfigUpdater(cfg engine.ExecutionConfig) *GovInstantiateConfigUpdater {
	return &GovInstantiateConfigUpdater{cfg: cfg}
}

func (g *GovInstantiateConfigUpdater) Handler() engine.TaskHandler {
	return engine.TypedHandlerWithResult(func(ctx context.Context, params GovUpdateInstantiateConfigRequest) (*wire.GovTxResult, error) {
		msg, err := buildGovUpdateInstantiateConfigMsg(g.cfg, params)
		if err != nil {
			return nil, err
		}
		result, err := SignAndBroadcast(ctx, g.cfg, SignAndBroadcastInput{
			ChainID: params.ChainID,
			KeyName: params.KeyName,
			Msg:     msg,
			Fees:    params.Fees,
			Gas:     params.Gas,
			Memo:    params.Memo,
			TaskID:  engine.TaskIDFromContext(ctx),
		})
		if err != nil {
			return nil, err
		}
		out, classifyErr := classifyGovResult(engine.TaskGovInstantiateConfig, result)
		classifyErr = requireProposalID(out, classifyErr)
		govUpdateInstantiateConfigLog.Info("proposal broadcast",
			"taskId", engine.TaskIDFromContext(ctx),
			"chainId", params.ChainID,
			"updates", len(params.Updates),
			"txHash", out.TxHash,
			"height", out.Height,
			"proposalId", out.ProposalID,
			"inclusionStatus", out.InclusionStatus)
		return out, classifyErr
	})
}

func buildGovUpdateInstantiateConfigMsg(
	cfg engine.ExecutionConfig,
	params GovUpdateInstantiateConfigRequest,
) (*govtypes.MsgSubmitProposal, error) {
	if cfg.Keyring == nil {
		return nil, Terminal(errors.New("keyring not configured: set SEI_KEYRING_BACKEND/SEI_KEYRING_PASSPHRASE on the sidecar"))
	}
	if params.KeyName == "" {
		return nil, Terminal(errors.New("keyName required"))
	}
	if params.Title == "" {
		return nil, Terminal(errors.New("title required"))
	}
	if params.Description == "" {
		return nil, Terminal(errors.New("description required"))
	}
	if len(params.Updates) == 0 {
		return nil, Terminal(errors.New("at least one update required"))
	}

	updates := make([]wasmtypes.AccessConfigUpdate, 0, len(params.Updates))
	seen := make(map[uint64]struct{}, len(params.Updates))
	for i, update := range params.Updates {
		if update.CodeID == 0 {
			return nil, Terminal(fmt.Errorf("updates[%d].codeId required (must be > 0)", i))
		}
		if _, ok := seen[update.CodeID]; ok {
			return nil, Terminal(fmt.Errorf("duplicate codeId %d", update.CodeID))
		}
		seen[update.CodeID] = struct{}{}

		permission, err := parseInstantiatePermission(update.Permission)
		if err != nil {
			return nil, Terminal(fmt.Errorf("updates[%d].permission: %w", i, err))
		}
		updates = append(updates, wasmtypes.AccessConfigUpdate{
			CodeID:                update.CodeID,
			InstantiatePermission: permission,
		})
	}

	info, err := cfg.Keyring.Key(params.KeyName)
	if err != nil {
		return nil, Terminal(fmt.Errorf("keyring entry %q: %w", params.KeyName, err))
	}
	deposit, err := sdk.ParseCoinsNormalized(params.InitialDeposit)
	if err != nil {
		return nil, Terminal(fmt.Errorf("parse initialDeposit %q: %w", params.InitialDeposit, err))
	}
	if len(deposit) == 0 {
		return nil, Terminal(fmt.Errorf("initialDeposit %q resolves to zero coins", params.InitialDeposit))
	}
	if !deposit.IsAllPositive() {
		return nil, Terminal(fmt.Errorf("initialDeposit %q contains non-positive amounts", params.InitialDeposit))
	}
	for _, coin := range deposit {
		if coin.Denom != feeDenom {
			return nil, Terminal(fmt.Errorf(
				"initialDeposit %q: denom %q not permitted (only %q)",
				params.InitialDeposit, coin.Denom, feeDenom))
		}
	}

	content := &wasmtypes.UpdateInstantiateConfigProposal{
		Title:               params.Title,
		Description:         params.Description,
		AccessConfigUpdates: updates,
	}
	if err := content.ValidateBasic(); err != nil {
		return nil, Terminal(fmt.Errorf("validate UpdateInstantiateConfigProposal: %w", err))
	}
	msg, err := govtypes.NewMsgSubmitProposal(content, deposit, info.GetAddress())
	if err != nil {
		return nil, Terminal(fmt.Errorf("build MsgSubmitProposal: %w", err))
	}
	return msg, nil
}

func parseInstantiatePermission(raw string) (wasmtypes.AccessConfig, error) {
	switch raw {
	case "nobody":
		return wasmtypes.AllowNobody, nil
	case "everybody":
		return wasmtypes.AllowEverybody, nil
	case "":
		return wasmtypes.AccessConfig{}, errors.New("permission required")
	default:
		addr, err := sdk.AccAddressFromBech32(raw)
		if err != nil {
			return wasmtypes.AccessConfig{}, fmt.Errorf(
				"must be nobody, everybody, or a sei account address: %w", err)
		}
		return wasmtypes.AccessTypeOnlyAddress.With(addr), nil
	}
}
