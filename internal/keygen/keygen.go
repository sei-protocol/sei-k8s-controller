// Package keygen derives a Sei chain account: a BIP-39 mnemonic + the cosmos
// secp256k1 address at the standard coin-type-118 path, bech32-encoded with the
// "sei" prefix. The full pipeline (entropy → mnemonic → seed → BIP-32 master →
// BIP-44 child → secp256k1 → ripemd160 → bech32) matches `seid keys add`
// byte-for-byte, so a mnemonic generated here imports verbatim into a seid
// keyring.
//
// This is the general, k8s-free derivation primitive. Callers that need to stamp
// the result into a Secret layer it on top (the integration harness writes a
// per-run Secret the release-test pod reads via secretKeyRef).
package keygen

import (
	"crypto/sha256"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/cosmos/btcutil/bech32"
	bip39 "github.com/cosmos/go-bip39"
	"golang.org/x/crypto/ripemd160" //nolint:staticcheck // Cosmos address derivation is bound to RIPEMD-160 by protocol.
)

// cosmosHDPath is the cosmos BIP-44 path, coin type 118. Matches `seid keys add`.
const cosmosHDPath = "m/44'/118'/0'/0/0"

const bech32AccountPrefix = "sei"

// SecretMnemonicKey is the conventional Secret data key the mnemonic is stored
// under; downstream pods reference it via secretKeyRef.
const SecretMnemonicKey = "mnemonic"

// Identity is a derived account: the mnemonic that produced it and its bech32
// address. The mnemonic is the secret material; treat it accordingly.
type Identity struct {
	Mnemonic string
	Address  string
}

// Derive generates a 24-word BIP-39 mnemonic and derives the cosmos secp256k1
// address at m/44'/118'/0'/0/0.
func Derive() (Identity, error) {
	// 24 words → 256 bits entropy; matches seid default.
	entropy, err := bip39.NewEntropy(256)
	if err != nil {
		return Identity{}, fmt.Errorf("entropy: %w", err)
	}
	mnemonic, err := bip39.NewMnemonic(entropy)
	if err != nil {
		return Identity{}, fmt.Errorf("mnemonic: %w", err)
	}

	// BIP-39 PBKDF2 → 64-byte seed; empty passphrase matches seid default.
	seed, err := bip39.NewSeedWithErrorChecking(mnemonic, "")
	if err != nil {
		return Identity{}, fmt.Errorf("seed: %w", err)
	}
	master, chainCode := computeMasterFromSeed(seed)
	privKey, err := derivePrivateKeyForPath(master, chainCode, cosmosHDPath)
	if err != nil {
		return Identity{}, fmt.Errorf("derive %s: %w", cosmosHDPath, err)
	}
	_, pub := btcec.PrivKeyFromBytes(privKey)
	pubCompressed := pub.SerializeCompressed()

	// Cosmos address = ripemd160(sha256(pubkey_compressed)), bech32-encoded.
	sha := sha256.Sum256(pubCompressed)
	hasher := ripemd160.New()
	if _, err := hasher.Write(sha[:]); err != nil {
		return Identity{}, fmt.Errorf("ripemd160: %w", err)
	}
	addrBytes := hasher.Sum(nil)

	converted, err := bech32.ConvertBits(addrBytes, 8, 5, true)
	if err != nil {
		return Identity{}, fmt.Errorf("bech32 convert: %w", err)
	}
	address, err := bech32.Encode(bech32AccountPrefix, converted)
	if err != nil {
		return Identity{}, fmt.Errorf("bech32 encode: %w", err)
	}
	return Identity{Mnemonic: mnemonic, Address: address}, nil
}
