package tasks

import (
	"strings"
	"testing"

	ethcrypto "github.com/ethereum/go-ethereum/crypto"

	"github.com/sei-protocol/sei-chain/sei-cosmos/crypto/keyring"
	sdk "github.com/sei-protocol/sei-chain/sei-cosmos/types"
)

// The voter readEVMVoter publishes is the EVM address generate-gentx associates
// with the validator's account, so the key that signs the account's txs is the
// one that votes.
func TestReadEVMVoter_MatchesGentxEVMAssociation(t *testing.T) {
	homeDir := t.TempDir()
	cdc, _ := makeCodec()
	bech32, err := NewGentxGenerator(homeDir).addValidatorKey(cdc)
	if err != nil {
		t.Fatal(err)
	}

	voter, err := readEVMVoter(homeDir)
	if err != nil {
		t.Fatalf("readEVMVoter: %v", err)
	}
	if err := checkEVMVoter(voter); err != nil {
		t.Fatalf("voter %q: %v", voter, err)
	}

	kb, err := keyring.New(sdk.KeyringServiceName(), keyring.BackendTest, homeDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	addr, err := sdk.AccAddressFromBech32(bech32)
	if err != nil {
		t.Fatal(err)
	}
	priv, err := getPrivateKeyOfAddr(kb, addr)
	if err != nil {
		t.Fatal(err)
	}
	if want := ethcrypto.PubkeyToAddress(priv.PublicKey).Hex(); voter != want {
		t.Errorf("voter = %s, gentx associates %s", voter, want)
	}
}

func TestReadEVMVoter_NoKey(t *testing.T) {
	if _, err := readEVMVoter(t.TempDir()); err == nil || !strings.Contains(err.Error(), validatorKeyName) {
		t.Fatalf("expected an error naming the %q key, got %v", validatorKeyName, err)
	}
}
