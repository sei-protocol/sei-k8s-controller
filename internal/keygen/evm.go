package keygen

import (
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"golang.org/x/crypto/sha3"
)

// EVMIdentity is a secp256k1 key addressed the way Sei's EVM sees it.
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
	if _, err := h.Write(uncompressed[1:]); err != nil {
		return EVMIdentity{}, fmt.Errorf("keccak256: %w", err)
	}
	addr := h.Sum(nil)[12:]

	cast, err := bech32Address(addr)
	if err != nil {
		return EVMIdentity{}, err
	}
	return EVMIdentity{
		PrivateKeyHex: hex.EncodeToString(priv.Serialize()),
		EVMAddress:    "0x" + hex.EncodeToString(addr),
		Address:       cast,
	}, nil
}
