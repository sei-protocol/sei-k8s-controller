package keygen

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
)

// BIP-32 / BIP-44 derivation, inlined to keep the seitask binary free of
// sei-cosmos. Only non-stdlib dep is btcec/v2 for secp256k1 point arithmetic;
// algorithm verified against the cosmos-sdk fundraiser vector in hd_test.go.

var secp256k1Order = btcec.S256().N

const hardenedOffset uint32 = 1 << 31 // BIP-32: hardened indices have bit 31 set.

// computeMasterFromSeed: HMAC-SHA512(key="Bitcoin seed", data=seed). Output
// split: left half = master private key, right half = master chain code.
func computeMasterFromSeed(seed []byte) (privKey, chainCode [32]byte) {
	h := hmac.New(sha512.New, []byte("Bitcoin seed"))
	h.Write(seed)
	sum := h.Sum(nil)
	copy(privKey[:], sum[:32])
	copy(chainCode[:], sum[32:])
	return
}

// derivePrivateKeyForPath walks "m/<idx>['/<idx>']..." from the master.
// Trailing apostrophe = hardened. Returns the final 32-byte private key.
func derivePrivateKeyForPath(master, chainCode [32]byte, path string) ([]byte, error) {
	segments, err := parseBIP44Path(path)
	if err != nil {
		return nil, err
	}
	priv := master
	cc := chainCode
	for _, idx := range segments {
		priv, cc, err = ckdPriv(priv, cc, idx)
		if err != nil {
			return nil, fmt.Errorf("derive index %d: %w", idx, err)
		}
	}
	out := make([]byte, 32)
	copy(out, priv[:])
	return out, nil
}

// ckdPriv is BIP-32 CKDpriv: parent (priv, chain) + index → child (priv, chain).
// Hardened indices feed the parent private key; non-hardened feed the parent
// compressed pubkey.
func ckdPriv(parentPriv, parentCC [32]byte, index uint32) ([32]byte, [32]byte, error) {
	var data []byte
	if index >= hardenedOffset {
		// Hardened: 0x00 || parent_priv || index_be32
		data = make([]byte, 1+32+4)
		data[0] = 0x00
		copy(data[1:33], parentPriv[:])
		binary.BigEndian.PutUint32(data[33:], index)
	} else {
		// Non-hardened: parent_pubkey_compressed || index_be32
		priv, _ := btcec.PrivKeyFromBytes(parentPriv[:])
		pubCompressed := priv.PubKey().SerializeCompressed()
		data = make([]byte, 33+4)
		copy(data, pubCompressed)
		binary.BigEndian.PutUint32(data[33:], index)
	}
	h := hmac.New(sha512.New, parentCC[:])
	h.Write(data)
	sum := h.Sum(nil)

	// IL = sum[:32], IR = sum[32:]. The IL>=N and child==0 cases are <2^-127
	// probability; we error rather than retry because mnemonic-bound paths
	// are fixed (BIP-32 §5).
	il := new(big.Int).SetBytes(sum[:32])
	if il.Cmp(secp256k1Order) >= 0 {
		return [32]byte{}, [32]byte{}, errors.New("IL >= curve order")
	}

	parentInt := new(big.Int).SetBytes(parentPriv[:])
	childInt := new(big.Int).Add(il, parentInt)
	childInt.Mod(childInt, secp256k1Order)

	if childInt.Sign() == 0 {
		return [32]byte{}, [32]byte{}, errors.New("child key == 0")
	}

	var childPriv, childCC [32]byte
	childInt.FillBytes(childPriv[:])
	copy(childCC[:], sum[32:])
	return childPriv, childCC, nil
}

// parseBIP44Path: "m/44'/118'/0'/0/0" → [44|H, 118|H, 0|H, 0, 0]. Leading "m/" required.
func parseBIP44Path(path string) ([]uint32, error) {
	rest, ok := strings.CutPrefix(path, "m/")
	if !ok {
		return nil, fmt.Errorf("path must start with m/: %q", path)
	}
	parts := strings.Split(rest, "/")
	out := make([]uint32, 0, len(parts))
	for _, p := range parts {
		hardened := strings.HasSuffix(p, "'")
		if hardened {
			p = strings.TrimSuffix(p, "'")
		}
		n, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("path segment %q: %w", p, err)
		}
		if hardened && n >= 1<<31 {
			return nil, fmt.Errorf("path segment %q overflows hardened range", p)
		}
		idx := uint32(n)
		if hardened {
			idx += hardenedOffset
		}
		out = append(out, idx)
	}
	return out, nil
}
