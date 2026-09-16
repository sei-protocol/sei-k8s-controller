package tasks

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/sei-protocol/sei-chain/sei-cosmos/types"
	wasmtypes "github.com/sei-protocol/sei-chain/sei-wasmd/x/wasm/types"

	"github.com/sei-protocol/sei-k8s-controller/sidecar/engine"
)

func validGovUpdateInstantiateConfigRequest() GovUpdateInstantiateConfigRequest {
	return GovUpdateInstantiateConfigRequest{
		ChainID:     "arctic-1",
		KeyName:     "node_admin",
		Title:       "Disable CosmWasm Contract Instantiation",
		Description: "Set every existing code's instantiate permission to Nobody.",
		Updates: []instantiateConfigUpdate{
			{CodeID: 1, Permission: "nobody"},
			{CodeID: 2, Permission: "everybody"},
		},
		InitialDeposit: "10000000usei",
		Fees:           "30000usei",
		Gas:            1_200_000,
	}
}

func TestBuildGovUpdateInstantiateConfigMsg(t *testing.T) {
	keyring, addr := testKeyring(t)
	cfg := engine.ExecutionConfig{Keyring: keyring}

	t.Run("happy path", func(t *testing.T) {
		msg, err := buildGovUpdateInstantiateConfigMsg(
			cfg, validGovUpdateInstantiateConfigRequest())
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if msg.Proposer != addr.String() {
			t.Errorf("proposer = %q, want %q", msg.Proposer, addr.String())
		}
		content, ok := msg.GetContent().(*wasmtypes.UpdateInstantiateConfigProposal)
		if !ok {
			t.Fatalf("content type = %T, want *UpdateInstantiateConfigProposal", msg.GetContent())
		}
		if len(content.AccessConfigUpdates) != 2 {
			t.Fatalf("updates = %d, want 2", len(content.AccessConfigUpdates))
		}
		if got := content.AccessConfigUpdates[0].InstantiatePermission; got != wasmtypes.AllowNobody {
			t.Errorf("updates[0].permission = %#v, want AllowNobody", got)
		}
		if got := content.AccessConfigUpdates[1].InstantiatePermission; got != wasmtypes.AllowEverybody {
			t.Errorf("updates[1].permission = %#v, want AllowEverybody", got)
		}
		if err := msg.ValidateBasic(); err != nil {
			t.Errorf("ValidateBasic on returned msg: %v", err)
		}
	})

	t.Run("address maps to OnlyAddress", func(t *testing.T) {
		req := validGovUpdateInstantiateConfigRequest()
		req.Updates = []instantiateConfigUpdate{{CodeID: 1, Permission: addr.String()}}
		msg, err := buildGovUpdateInstantiateConfigMsg(cfg, req)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		content := msg.GetContent().(*wasmtypes.UpdateInstantiateConfigProposal)
		got := content.AccessConfigUpdates[0].InstantiatePermission
		if got.Permission != wasmtypes.AccessTypeOnlyAddress || got.Address != addr.String() {
			t.Errorf("permission = %#v, want OnlyAddress(%s)", got, addr)
		}
	})

	t.Run("non-usei deposit rejected", func(t *testing.T) {
		req := validGovUpdateInstantiateConfigRequest()
		req.InitialDeposit = "10000000uatom"
		if _, err := buildGovUpdateInstantiateConfigMsg(cfg, req); err == nil {
			t.Fatal("expected error for non-usei deposit")
		} else if !strings.Contains(err.Error(), "not permitted") {
			t.Errorf("err = %v, want denom-not-permitted", err)
		}
	})

	t.Run("validation failures are terminal", func(t *testing.T) {
		cases := []struct {
			name string
			mut  func(*GovUpdateInstantiateConfigRequest)
		}{
			{"missing keyName", func(req *GovUpdateInstantiateConfigRequest) { req.KeyName = "" }},
			{"missing title", func(req *GovUpdateInstantiateConfigRequest) { req.Title = "" }},
			{"missing description", func(req *GovUpdateInstantiateConfigRequest) { req.Description = "" }},
			{"empty updates", func(req *GovUpdateInstantiateConfigRequest) { req.Updates = nil }},
			{"zero codeId", func(req *GovUpdateInstantiateConfigRequest) { req.Updates[0].CodeID = 0 }},
			{"duplicate codeId", func(req *GovUpdateInstantiateConfigRequest) {
				req.Updates[1].CodeID = req.Updates[0].CodeID
			}},
			{"empty permission", func(req *GovUpdateInstantiateConfigRequest) {
				req.Updates[0].Permission = ""
			}},
			{"invalid permission", func(req *GovUpdateInstantiateConfigRequest) {
				req.Updates[0].Permission = "somebody"
			}},
			{"non-usei deposit", func(req *GovUpdateInstantiateConfigRequest) {
				req.InitialDeposit = "1uatom"
			}},
			{"zero deposit", func(req *GovUpdateInstantiateConfigRequest) {
				req.InitialDeposit = "0usei"
			}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				req := validGovUpdateInstantiateConfigRequest()
				tc.mut(&req)
				_, err := buildGovUpdateInstantiateConfigMsg(cfg, req)
				if err == nil {
					t.Fatal("expected error")
				}
				if !IsTerminal(err) {
					t.Errorf("err = %v, want Terminal", err)
				}
			})
		}
	})
}

// This threads the proposal through the sign path. Reaching BroadcastSync
// proves makeSignTxCodec registers UpdateInstantiateConfigProposal as gov
// Content; without that registration the Any cannot be encoded.
func TestGovUpdateInstantiateConfigHandlerHappyPath(t *testing.T) {
	cfg, _ := newGuardCfg(t, "arctic-1")
	txClient := &fakeTxClient{
		accountNumber: 17,
		sequence:      42,
		broadcastResp: &sdk.TxResponse{Code: 0, TxHash: "h", Height: 0},
		queryDefault:  &sdk.TxResponse{Code: 0, Height: 7},
	}

	req := validGovUpdateInstantiateConfigRequest()
	msg, err := buildGovUpdateInstantiateConfigMsg(cfg, req)
	if err != nil {
		t.Fatalf("buildGovUpdateInstantiateConfigMsg: %v", err)
	}
	info, err := cfg.Keyring.Key("node_admin")
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}

	result, err := signAndBroadcast(context.Background(), cfg, txClient, SignAndBroadcastInput{
		ChainID: "arctic-1",
		KeyName: "node_admin",
		Msg:     msg,
		Fees:    req.Fees,
		Gas:     req.Gas,
		TaskID:  "00000000-0000-0000-0000-0000000000ab",
	}, info.GetAddress())
	if err != nil {
		t.Fatalf("signAndBroadcast: %v", err)
	}
	if result.TxHash != "h" {
		t.Errorf("TxHash = %q, want h", result.TxHash)
	}
	if txClient.broadcasts != 1 {
		t.Errorf("broadcasts = %d, want 1", txClient.broadcasts)
	}
}
