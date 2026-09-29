package keygen

import (
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/cosmos/btcutil/bech32"
	"golang.org/x/crypto/sha3"
)

// EVMIdentity is a random secp256k1 key addressed the way Sei's EVM sees it.
// PrivateKeyHex is the secret material; treat it accordingly.
type EVMIdentity struct {
	// PrivateKeyHex is the 32-byte key, hex without a 0x prefix — the form
	// sei-load's funding.rootKeyFile reads.
	PrivateKeyHex string
	// EVMAddress is the 0x address: the last 20 bytes of keccak256 over the
	// uncompressed public key.
	EVMAddress string
	// Address is the bech32 cast of EVMAddress's 20 bytes. A balance funded
	// here in genesis becomes spendable from EVMAddress once the key's first
	// EVM tx associates the two, which is not true of the coin-type-118
	// address Derive returns for the same key.
	Address string
}

// DeriveEVM generates a fresh secp256k1 key and its EVM and cast addresses.
func DeriveEVM() (EVMIdentity, error) {
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		return EVMIdentity{}, fmt.Errorf("private key: %w", err)
	}
	return evmIdentityFromKey(priv)
}

func evmIdentityFromKey(priv *btcec.PrivateKey) (EVMIdentity, error) {
	uncompressed := priv.PubKey().SerializeUncompressed()
	h := sha3.NewLegacyKeccak256()
	h.Write(uncompressed[1:])
	addr := h.Sum(nil)[12:]

	converted, err := bech32.ConvertBits(addr, 8, 5, true)
	if err != nil {
		return EVMIdentity{}, fmt.Errorf("bech32 convert: %w", err)
	}
	cast, err := bech32.Encode(bech32AccountPrefix, converted)
	if err != nil {
		return EVMIdentity{}, fmt.Errorf("bech32 encode: %w", err)
	}
	return EVMIdentity{
		PrivateKeyHex: hex.EncodeToString(priv.Serialize()),
		EVMAddress:    "0x" + hex.EncodeToString(addr),
		Address:       cast,
	}, nil
}
