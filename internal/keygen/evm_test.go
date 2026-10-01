package keygen

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
)

// Private key 1 has the widely published Ethereum address
// 0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf, which locks the keccak256
// derivation; the cast is that address's 20 bytes under the sei prefix.
func TestEVMIdentityFromKey_PrivateKeyOne(t *testing.T) {
	var one [32]byte
	one[31] = 1
	priv, _ := btcec.PrivKeyFromBytes(one[:])

	id, err := evmIdentityFromKey(priv)
	if err != nil {
		t.Fatalf("evmIdentityFromKey: %v", err)
	}
	if want := "0000000000000000000000000000000000000000000000000000000000000001"; id.PrivateKeyHex != want {
		t.Errorf("PrivateKeyHex: got %s, want %s", id.PrivateKeyHex, want)
	}
	if want := "0x7e5f4552091a69125d5dfcb7b8c2659029395bdf"; id.EVMAddress != want {
		t.Errorf("EVMAddress: got %s, want %s", id.EVMAddress, want)
	}
	if want := "sei10e0525sfrf53yh2aljmm3sn9jq5njk7lfvasxs"; id.Address != want {
		t.Errorf("Address: got %s, want %s", id.Address, want)
	}
}
