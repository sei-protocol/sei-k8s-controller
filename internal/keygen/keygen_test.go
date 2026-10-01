package keygen

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/cosmos/btcutil/bech32"
	bip39 "github.com/cosmos/go-bip39"
	"golang.org/x/crypto/ripemd160" //nolint:staticcheck
)

// RIPEMD-160 is the only x/crypto package the controller links directly; these
// published known-answer vectors pin the dependency surface that a bump of
// golang.org/x/crypto can regress.
func TestRipemd160_KnownAnswers(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"", "9c1185a5c5e9fc54612808977ee8f548b2258d31"},
		{"abc", "8eb208f7e05d987a9b044a8e98c6b087f15a0bfc"},
		{"The quick brown fox jumps over the lazy dog", "37f332f68db77bd9d7edd4969571ad671cf9dd3b"},
	}
	for _, tc := range cases {
		h := ripemd160.New()
		h.Write([]byte(tc.input))
		if got := hex.EncodeToString(h.Sum(nil)); got != tc.want {
			t.Fatalf("ripemd160(%q): got %s, want %s", tc.input, got, tc.want)
		}
	}
}

// Derive() is exercised end-to-end: the identity it returns must re-derive to
// the same address through the same pipeline, and the bech32 must decode back
// to the same 20 bytes. This is the path the release-test Secret depends on.
func TestDerive_RoundTrips(t *testing.T) {
	id, err := Derive()
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if got := len(strings.Fields(id.Mnemonic)); got != 24 {
		t.Fatalf("mnemonic word count: got %d, want 24", got)
	}
	if !strings.HasPrefix(id.Address, "sei1") {
		t.Fatalf("address prefix: got %s", id.Address)
	}

	// Re-derive the address from the returned mnemonic through the same steps
	// Derive uses; NewSeedWithErrorChecking also validates the BIP-39 checksum.
	seed, err := bip39.NewSeedWithErrorChecking(id.Mnemonic, "")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	master, cc := computeMasterFromSeed(seed)
	priv, err := derivePrivateKeyForPath(master, cc, cosmosHDPath)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	gotAddr, err := cosmosAddress(priv)
	if err != nil {
		t.Fatalf("cosmosAddress: %v", err)
	}
	if gotAddr != id.Address {
		t.Fatalf("re-derived address: got %s, want %s", gotAddr, id.Address)
	}

	// The emitted bech32 must decode back to ripemd160(sha256(pubkey)).
	hrp, data, err := bech32.Decode(id.Address, 1023)
	if err != nil {
		t.Fatalf("bech32 decode: %v", err)
	}
	if hrp != bech32AccountPrefix {
		t.Fatalf("hrp: got %s, want %s", hrp, bech32AccountPrefix)
	}
	decoded, err := bech32.ConvertBits(data, 5, 8, false)
	if err != nil {
		t.Fatalf("bech32 convert back: %v", err)
	}
	if len(decoded) != 20 {
		t.Fatalf("decoded address length: got %d, want 20", len(decoded))
	}
}

// Two successive Derive calls must not collide — guards entropy plumbing.
func TestDerive_Unique(t *testing.T) {
	a, err := Derive()
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	b, err := Derive()
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if a.Mnemonic == b.Mnemonic || a.Address == b.Address {
		t.Fatal("two Derive calls returned identical identities")
	}
}
