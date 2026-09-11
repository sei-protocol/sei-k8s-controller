package tasks

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeAutobahnKeyFiles lays down the two key files generate-identity writes
// and returns the public keys gen-autobahn-config would derive from them.
func writeAutobahnKeyFiles(t *testing.T, homeDir string) (valPub, nodePub ed25519.PublicKey) {
	t.Helper()
	configDir := filepath.Join(homeDir, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	valPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	nodePub, nodePriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pvk := map[string]any{
		"address": "AA",
		"pub_key": map[string]string{"type": ed25519PubKeyTypeName, "value": base64.StdEncoding.EncodeToString(valPub)},
	}
	nk := map[string]any{
		"priv_key": map[string]string{"type": ed25519PrivKeyTypeName, "value": base64.StdEncoding.EncodeToString(nodePriv)},
	}
	for name, doc := range map[string]any{"priv_validator_key.json": pvk, "node_key.json": nk} {
		data, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(configDir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return valPub, nodePub
}

func TestReadAutobahnIdentity_ProviderTextEncodings(t *testing.T) {
	homeDir := t.TempDir()
	valPub, nodePub := writeAutobahnKeyFiles(t, homeDir)

	id, err := readAutobahnIdentity(homeDir, "val-1", "bench")
	if err != nil {
		t.Fatal(err)
	}
	if want := "validator:ed25519:public:" + hex.EncodeToString(valPub); id.ValidatorPubKey != want {
		t.Errorf("validator_pubkey = %q, want %q", id.ValidatorPubKey, want)
	}
	if want := "node:ed25519:public:" + hex.EncodeToString(nodePub); id.NodePubKey != want {
		t.Errorf("node_pubkey = %q, want %q", id.NodePubKey, want)
	}
	if id.AutobahnAddress != "val-1-0.val-1.bench.svc.cluster.local:26656" {
		t.Errorf("autobahn_address = %q", id.AutobahnAddress)
	}
	if id.EVMRPCURL != "http://val-1-0.val-1.bench.svc.cluster.local:8545" {
		t.Errorf("evmrpc_url = %q", id.EVMRPCURL)
	}
	if err := validateAutobahnIdentity("val-1", id); err != nil {
		t.Errorf("a freshly derived identity must validate: %v", err)
	}
}

func TestReadAutobahnIdentity_RejectsForeignKeyTypes(t *testing.T) {
	homeDir := t.TempDir()
	writeAutobahnKeyFiles(t, homeDir)
	pvk := filepath.Join(homeDir, "config", "priv_validator_key.json")
	if err := os.WriteFile(pvk, []byte(`{"pub_key":{"type":"tendermint/PubKeySecp256k1","value":"AA=="}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readAutobahnIdentity(homeDir, "val-1", "bench"); err == nil || !strings.Contains(err.Error(), ed25519PubKeyTypeName) {
		t.Fatalf("expected a key-type error naming %s, got %v", ed25519PubKeyTypeName, err)
	}
}

func TestValidateAutobahnIdentity(t *testing.T) {
	good := autobahnIdentity{
		ValidatorPubKey: "validator:ed25519:public:" + strings.Repeat("ab", ed25519.PublicKeySize),
		NodePubKey:      "node:ed25519:public:" + strings.Repeat("cd", ed25519.PublicKeySize),
		AutobahnAddress: "val-0.val.ns.svc.cluster.local:26656",
		EVMRPCURL:       "http://val-0.val.ns.svc.cluster.local:8545",
	}
	tests := []struct {
		name   string
		mutate func(*autobahnIdentity)
		want   string
	}{
		{"valid", func(*autobahnIdentity) {}, ""},
		{"missing section", nil, "no autobahn section"},
		{"validator prefix", func(id *autobahnIdentity) { id.ValidatorPubKey = "node:ed25519:public:" + strings.Repeat("ab", 32) }, "validator_pubkey"},
		{"validator hex length", func(id *autobahnIdentity) { id.ValidatorPubKey = "validator:ed25519:public:abcd" }, "validator_pubkey"},
		{"node prefix", func(id *autobahnIdentity) { id.NodePubKey = "ed25519:public:" + strings.Repeat("cd", 32) }, "node_pubkey"},
		{"address without port", func(id *autobahnIdentity) { id.AutobahnAddress = "val-0.val.ns.svc.cluster.local" }, "autobahn_address"},
		{"evmrpc not a URL", func(id *autobahnIdentity) { id.EVMRPCURL = "val-0:8545" }, "evmrpc_url"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var id *autobahnIdentity
			if tc.mutate != nil {
				copied := good
				tc.mutate(&copied)
				id = &copied
			}
			err := validateAutobahnIdentity("val-0", id)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error mentioning %q, got %v", tc.want, err)
			}
		})
	}
}

// The artifact must decode as the provider's AutobahnFileConfig with the
// values gen-autobahn-config writes under its default flags.
func TestBuildAutobahnConfig_MatchesGenAutobahnConfigDefaults(t *testing.T) {
	validators := []autobahnValidator{
		{ValidatorKey: "validator:ed25519:public:aa", NodeKey: "node:ed25519:public:bb", Address: "a:26656", EVMRPC: "http://a:8545"},
		{ValidatorKey: "validator:ed25519:public:cc", NodeKey: "node:ed25519:public:dd", Address: "b:26656", EVMRPC: "http://b:8545"},
	}
	data, err := buildAutobahnConfig(validators, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"max_txs_per_block":    "2000",
		"max_txs_per_second":   "null",
		"allow_empty_blocks":   "false",
		"block_interval":       `"400ms"`,
		"view_timeout":         `"1.5s"`,
		"persistent_state_dir": `"data/autobahn"`,
		"dial_interval":        `"10s"`,
		"block_db":             `{"retention":"30s","gc_period":null}`,
	}
	for key, value := range want {
		raw, ok := got[key]
		if !ok {
			t.Errorf("missing %s", key)
			continue
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			t.Fatal(err)
		}
		if compact.String() != value {
			t.Errorf("%s = %s, want %s", key, compact.String(), value)
		}
	}
	for _, absent := range []string{"max_inbound_fullnode_peers", "enable_evm_proxy"} {
		if _, ok := got[absent]; ok {
			t.Errorf("%s must be omitted so the provider default applies", absent)
		}
	}
	var vals []autobahnValidator
	if err := json.Unmarshal(got["validators"], &vals); err != nil {
		t.Fatal(err)
	}
	if len(vals) != 2 || vals[0] != validators[0] || vals[1] != validators[1] {
		t.Errorf("validators round-trip mismatch: %+v", vals)
	}

	if _, err := buildAutobahnConfig(nil, nil); err == nil {
		t.Error("an empty validator set must be rejected")
	}

	empty, err := buildAutobahnConfig(validators, &AutobahnConfigOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(empty, data) {
		t.Error("an empty overrides struct must render the same artifact as nil")
	}
}

func TestBuildAutobahnConfig_Overrides(t *testing.T) {
	validators := []autobahnValidator{
		{ValidatorKey: "validator:ed25519:public:aa", NodeKey: "node:ed25519:public:bb", Address: "a:26656", EVMRPC: "http://a:8545"},
	}
	allow := true
	maxTxs := int64(1500)
	data, err := buildAutobahnConfig(validators, &AutobahnConfigOverrides{
		BlockInterval:    "1s",
		AllowEmptyBlocks: &allow,
		MaxTxsPerBlock:   &maxTxs,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"max_txs_per_block":  "1500",
		"allow_empty_blocks": "true",
		"block_interval":     `"1s"`,
		"view_timeout":       `"1.5s"`,
		"dial_interval":      `"10s"`,
	}
	for key, value := range want {
		if string(got[key]) != value {
			t.Errorf("%s = %s, want %s", key, got[key], value)
		}
	}

	disallow := false
	zero := int64(0)
	overCap := int64(2001)
	for name, o := range map[string]*AutobahnConfigOverrides{
		"above protocol cap":   {MaxTxsPerBlock: &overCap},
		"unparseable duration": {BlockInterval: "fast"},
		"bare number":          {BlockInterval: "400"},
		"zero duration":        {BlockInterval: "0s"},
		"negative duration":    {BlockInterval: "-1s"},
		"zero max txs":         {MaxTxsPerBlock: &zero},
	} {
		if _, err := buildAutobahnConfig(validators, o); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := buildAutobahnConfig(validators, &AutobahnConfigOverrides{AllowEmptyBlocks: &disallow}); err != nil {
		t.Errorf("explicit false must be accepted: %v", err)
	}
}
