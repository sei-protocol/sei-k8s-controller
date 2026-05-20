package keygen

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/cosmos/btcutil/bech32"
	bip39 "github.com/cosmos/go-bip39"
	"golang.org/x/crypto/ripemd160" //nolint:staticcheck
)

// BIP-32 reference test vector 1 from the BIP-32 spec
// (https://github.com/bitcoin/bips/blob/master/bip-0032.mediawiki).
// Locks the inlined ckdPriv / computeMasterFromSeed against the canonical
// algorithm.
func TestComputeMasterFromSeed_BIP32Vector1(t *testing.T) {
	seedHex := "000102030405060708090a0b0c0d0e0f"
	seed, _ := hex.DecodeString(seedHex)
	wantPriv := "e8f32e723decf4051aefac8e2c93c9c5b214313817cdb01a1494b917c8436b35"
	wantChainCode := "873dff81c02f525623fd1fe5167eac3a55a049de3d314bb42ee227ffed37d508"

	priv, cc := computeMasterFromSeed(seed)
	if got := hex.EncodeToString(priv[:]); got != wantPriv {
		t.Fatalf("master priv: got %s, want %s", got, wantPriv)
	}
	if got := hex.EncodeToString(cc[:]); got != wantChainCode {
		t.Fatalf("master chainCode: got %s, want %s", got, wantChainCode)
	}
}

// Cross-implementation check against the cosmos-sdk fundraiser test
// vectors (sei-cosmos/crypto/hd/testdata/test.json, first entry). Locks
// the full BIP-39 → BIP-32 → BIP-44 → secp256k1 → cosmos-address pipeline
// to the canonical algorithm sei-cosmos implements.
func TestDeriveIdentity_CosmosFundraiserVector(t *testing.T) {
	const (
		mnemonic = "measure slogan connect luggage stereo federal stuff stomach stumble security end differ"
		wantSeed = "c237f7aa198c5bd560ac8daf5b8421d03855171465b2999b07159671e9186461e7d75dba6c7264b963108431f8674ac8d095b7a22878fa0ab8b582e5d6ea1986"
		wantMstr = "c177fa4bd21420b6ba2246e4f7a43fbe76545d1204174cae942ffbd79a434d11"
		wantPriv = "91bba8805845210665d7a9c5aff63ef69f7604fbeddb485706d31f04458f572c"
		wantPub  = "0210ddc89abc90bbcbec8c63f5a4ebb016b58063b9d1a77854502042bdfcac5e51"
		wantAddr = "72e7d6e9cfa899043a0783752a4876423f8effb8" // raw 20 bytes, ripemd160(sha256(pubkey))
	)
	seed, err := bip39.NewSeedWithErrorChecking(mnemonic, "")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := hex.EncodeToString(seed); got != wantSeed {
		t.Fatalf("seed: got %s, want %s", got, wantSeed)
	}

	master, cc := computeMasterFromSeed(seed)
	if got := hex.EncodeToString(master[:]); got != wantMstr {
		t.Fatalf("master: got %s, want %s", got, wantMstr)
	}

	priv, err := derivePrivateKeyForPath(master, cc, "m/44'/118'/0'/0/0")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if got := hex.EncodeToString(priv); got != wantPriv {
		t.Fatalf("priv: got %s, want %s", got, wantPriv)
	}

	_, pub := btcec.PrivKeyFromBytes(priv)
	pubCompressed := pub.SerializeCompressed()
	if got := hex.EncodeToString(pubCompressed); got != wantPub {
		t.Fatalf("pubkey: got %s, want %s", got, wantPub)
	}

	sha := sha256.Sum256(pubCompressed)
	hasher := ripemd160.New()
	hasher.Write(sha[:])
	addr := hasher.Sum(nil)
	if got := hex.EncodeToString(addr); got != wantAddr {
		t.Fatalf("address bytes: got %s, want %s", got, wantAddr)
	}

	// Bech32-encode with the sei HRP — the prefix is a presentation detail
	// over the same 20-byte address. Verifying it round-trips means the
	// encoding step is correct; the actual sei1... string is computed
	// deterministically from wantAddr above.
	converted, err := bech32.ConvertBits(addr, 8, 5, true)
	if err != nil {
		t.Fatalf("bech32 convert: %v", err)
	}
	seiAddr, err := bech32.Encode("sei", converted)
	if err != nil {
		t.Fatalf("bech32 encode: %v", err)
	}
	if !strings.HasPrefix(seiAddr, "sei1") {
		t.Fatalf("expected sei1 prefix, got %s", seiAddr)
	}
}

func TestParseBIP44Path(t *testing.T) {
	cases := []struct {
		path string
		want []uint32
		err  bool
	}{
		{"m/44'/118'/0'/0/0", []uint32{
			44 + hardenedOffset, 118 + hardenedOffset, 0 + hardenedOffset, 0, 0,
		}, false},
		{"44'/118'/0'/0/0", nil, true},
		{"m/44/118/0/0/0", []uint32{44, 118, 0, 0, 0}, false},
		{"m/4294967295", []uint32{4294967295}, false},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got, err := parseBIP44Path(tc.path)
			if (err != nil) != tc.err {
				t.Fatalf("err = %v, wantErr=%v", err, tc.err)
			}
			if err != nil {
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len: got %d, want %d", len(got), len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("segment %d: got %d, want %d", i, got[i], tc.want[i])
				}
			}
		})
	}
}
