package tasks

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// This file mirrors `seid tendermint gen-autobahn-config` (sei-chain
// sei-tendermint/cmd/tendermint/commands/gen_autobahn_config.go): the four
// per-validator inputs it reads, the checks it applies to each, and the JSON it
// writes with the command's default flags. The sidecar image carries no seid
// and its pinned sei-chain predates Autobahn, so the provider's shape is
// reproduced here field for field; a provider change to that command is a
// change to this file.

const (
	autobahnArtifactName = "autobahn.json"
	autobahnP2PPort      = 26656
	autobahnEVMRPCPort   = 8545

	ed25519PubKeyTypeName  = "tendermint/PubKeyEd25519"
	ed25519PrivKeyTypeName = "tendermint/PrivKeyEd25519"
)

// autobahnIdentity is the Autobahn slice of a validator's identity.json: the
// public inputs gen-autobahn-config reads from a node directory, in the text
// encodings the provider writes to validator_pubkey.txt, node_pubkey.txt,
// autobahn_address.txt and evmrpc_url.txt.
type autobahnIdentity struct {
	ValidatorPubKey string `json:"validator_pubkey"`
	NodePubKey      string `json:"node_pubkey"`
	AutobahnAddress string `json:"autobahn_address"`
	EVMRPCURL       string `json:"evmrpc_url"`
}

// autobahnValidator matches config.AutobahnValidator's JSON.
type autobahnValidator struct {
	ValidatorKey string `json:"validator_key"`
	NodeKey      string `json:"node_key"`
	Address      string `json:"address"`
	EVMRPC       string `json:"evmrpc"`
}

// autobahnBlockDBConfig matches config.AutobahnBlockDBConfig's JSON.
type autobahnBlockDBConfig struct {
	Retention *string `json:"retention"`
	GCPeriod  *string `json:"gc_period"`
}

// autobahnFileConfig matches config.AutobahnFileConfig's JSON for the fields
// gen-autobahn-config populates. Optional fields the command leaves absent and
// that the provider marks omitzero are omitted here too.
type autobahnFileConfig struct {
	Validators         []autobahnValidator   `json:"validators"`
	MaxTxsPerBlock     uint64                `json:"max_txs_per_block"`
	MaxTxsPerSecond    *uint64               `json:"max_txs_per_second"`
	AllowEmptyBlocks   bool                  `json:"allow_empty_blocks"`
	BlockInterval      string                `json:"block_interval"`
	ViewTimeout        string                `json:"view_timeout"`
	PersistentStateDir string                `json:"persistent_state_dir"`
	DialInterval       string                `json:"dial_interval"`
	BlockDB            autobahnBlockDBConfig `json:"block_db"`
}

// autobahnAddressFor returns the P2P host:port the ceremony advertises for a
// validator, the same in-cluster DNS name peers.json uses.
func autobahnAddressFor(nodeName, namespace string) string {
	return fmt.Sprintf("%s-0.%s.%s.svc.cluster.local:%d", nodeName, nodeName, namespace, autobahnP2PPort)
}

// autobahnEVMRPCURLFor returns the EVM-RPC URL the ceremony advertises for a
// validator's mempool shard.
func autobahnEVMRPCURLFor(nodeName, namespace string) string {
	return fmt.Sprintf("http://%s-0.%s.%s.svc.cluster.local:%d", nodeName, nodeName, namespace, autobahnEVMRPCPort)
}

// readAutobahnIdentity derives the validator's Autobahn public inputs from the
// key files generate-identity wrote under <home>/config.
func readAutobahnIdentity(homeDir, nodeName, namespace string) (*autobahnIdentity, error) {
	configDir := filepath.Join(homeDir, "config")
	valPub, err := readValidatorPubKey(filepath.Join(configDir, "priv_validator_key.json"))
	if err != nil {
		return nil, err
	}
	nodePub, err := readNodePubKey(filepath.Join(configDir, "node_key.json"))
	if err != nil {
		return nil, err
	}
	return &autobahnIdentity{
		ValidatorPubKey: "validator:" + ed25519PubKeyString(valPub),
		NodePubKey:      "node:" + ed25519PubKeyString(nodePub),
		AutobahnAddress: autobahnAddressFor(nodeName, namespace),
		EVMRPCURL:       autobahnEVMRPCURLFor(nodeName, namespace),
	}, nil
}

// ed25519PubKeyString is crypto/ed25519.PublicKey.String in the provider.
func ed25519PubKeyString(pub ed25519.PublicKey) string {
	return "ed25519:public:" + hex.EncodeToString(pub)
}

type typedKeyJSON struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

func readValidatorPubKey(path string) (ed25519.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", filepath.Base(path), err)
	}
	var key struct {
		PubKey typedKeyJSON `json:"pub_key"`
	}
	if err := json.Unmarshal(data, &key); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", filepath.Base(path), err)
	}
	if key.PubKey.Type != ed25519PubKeyTypeName {
		return nil, fmt.Errorf("%s: pub_key type %q, want %s", filepath.Base(path), key.PubKey.Type, ed25519PubKeyTypeName)
	}
	raw, err := base64.StdEncoding.DecodeString(key.PubKey.Value)
	if err != nil {
		return nil, fmt.Errorf("%s: decoding pub_key: %w", filepath.Base(path), err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%s: pub_key is %d bytes, want %d", filepath.Base(path), len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

func readNodePubKey(path string) (ed25519.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", filepath.Base(path), err)
	}
	var key struct {
		PrivKey typedKeyJSON `json:"priv_key"`
	}
	if err := json.Unmarshal(data, &key); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", filepath.Base(path), err)
	}
	if key.PrivKey.Type != ed25519PrivKeyTypeName {
		return nil, fmt.Errorf("%s: priv_key type %q, want %s", filepath.Base(path), key.PrivKey.Type, ed25519PrivKeyTypeName)
	}
	raw, err := base64.StdEncoding.DecodeString(key.PrivKey.Value)
	if err != nil {
		return nil, fmt.Errorf("%s: decoding priv_key: %w", filepath.Base(path), err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%s: priv_key is %d bytes, want %d", filepath.Base(path), len(raw), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(raw).Public().(ed25519.PublicKey), nil
}

// validateAutobahnIdentity applies the checks gen-autobahn-config applies when
// it parses each node directory, naming the validator and the field on failure.
func validateAutobahnIdentity(nodeName string, id *autobahnIdentity) error {
	if id == nil {
		return fmt.Errorf("validator %s: identity.json has no autobahn section; its upload-genesis-artifacts ran without engine Autobahn", nodeName)
	}
	if err := checkPrefixedPubKey(id.ValidatorPubKey, "validator:"); err != nil {
		return fmt.Errorf("validator %s: validator_pubkey: %w", nodeName, err)
	}
	if err := checkPrefixedPubKey(id.NodePubKey, "node:"); err != nil {
		return fmt.Errorf("validator %s: node_pubkey: %w", nodeName, err)
	}
	if err := checkHostPort(id.AutobahnAddress); err != nil {
		return fmt.Errorf("validator %s: autobahn_address: %w", nodeName, err)
	}
	if err := checkHTTPURL(id.EVMRPCURL); err != nil {
		return fmt.Errorf("validator %s: evmrpc_url: %w", nodeName, err)
	}
	return nil
}

func checkPrefixedPubKey(s, prefix string) error {
	if s == "" {
		return fmt.Errorf("missing")
	}
	rest := strings.TrimPrefix(s, prefix)
	if rest == s {
		return fmt.Errorf("%q lacks the %q prefix", s, prefix)
	}
	hexPart := strings.TrimPrefix(rest, "ed25519:public:")
	if hexPart == rest {
		return fmt.Errorf("%q lacks the ed25519:public: prefix", s)
	}
	raw, err := hex.DecodeString(hexPart)
	if err != nil {
		return fmt.Errorf("%q: %w", s, err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return fmt.Errorf("%q is %d bytes, want %d", s, len(raw), ed25519.PublicKeySize)
	}
	return nil
}

func checkHostPort(s string) error {
	if s == "" {
		return fmt.Errorf("missing")
	}
	_, port, err := net.SplitHostPort(s)
	if err != nil {
		return err
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("port %q: %w", port, err)
	}
	return nil
}

func checkHTTPURL(s string) error {
	if s == "" {
		return fmt.Errorf("missing")
	}
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q, want http or https", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("missing host")
	}
	if u.User != nil {
		return fmt.Errorf("userinfo not allowed")
	}
	return nil
}

// autobahnProtocolMaxTxsPerBlock mirrors sei-tendermint autobahn/types.MaxTxsPerBlock,
// the ceiling the producer clamps max_txs_per_block to.
const autobahnProtocolMaxTxsPerBlock = 2_000

// AutobahnConfigOverrides replaces gen-autobahn-config defaults in the rendered
// autobahn.json. Unset fields keep the default.
type AutobahnConfigOverrides struct {
	BlockInterval    string `json:"blockInterval,omitempty"`
	AllowEmptyBlocks *bool  `json:"allowEmptyBlocks,omitempty"`
	MaxTxsPerBlock   *int64 `json:"maxTxsPerBlock,omitempty"`
}

// buildAutobahnConfig renders autobahn.json for the given validators as
// `seid tendermint gen-autobahn-config <dirs> --output autobahn.json` does with
// its default flags: 2000 txs/block, empty blocks off, 400ms blocks, 1500ms
// view timeout, 10s dial interval, state persisted under data/autobahn with a
// 30s BlockDB retention. Overrides replace the defaults they name, checked
// against the provider's AutobahnFileConfig.Validate bounds. Validators keep
// the order given, which is the ceremony's ordinal order.
func buildAutobahnConfig(validators []autobahnValidator, overrides *AutobahnConfigOverrides) ([]byte, error) {
	if len(validators) == 0 {
		return nil, fmt.Errorf("autobahn: no validators")
	}
	retention := (30 * time.Second).String()
	cfg := autobahnFileConfig{
		Validators:         validators,
		MaxTxsPerBlock:     autobahnProtocolMaxTxsPerBlock,
		AllowEmptyBlocks:   false,
		BlockInterval:      (400 * time.Millisecond).String(),
		ViewTimeout:        (1500 * time.Millisecond).String(),
		PersistentStateDir: "data/autobahn",
		DialInterval:       (10 * time.Second).String(),
		BlockDB:            autobahnBlockDBConfig{Retention: &retention},
	}
	if overrides != nil {
		if overrides.BlockInterval != "" {
			d, err := time.ParseDuration(overrides.BlockInterval)
			if err != nil {
				return nil, fmt.Errorf("autobahn: block_interval %q: %w", overrides.BlockInterval, err)
			}
			if d <= 0 {
				return nil, fmt.Errorf("autobahn: block_interval must be > 0, got %q", overrides.BlockInterval)
			}
			cfg.BlockInterval = d.String()
		}
		if overrides.AllowEmptyBlocks != nil {
			cfg.AllowEmptyBlocks = *overrides.AllowEmptyBlocks
		}
		if overrides.MaxTxsPerBlock != nil {
			if *overrides.MaxTxsPerBlock < 1 || *overrides.MaxTxsPerBlock > autobahnProtocolMaxTxsPerBlock {
				return nil, fmt.Errorf("autobahn: max_txs_per_block must be in [1, %d], got %d", autobahnProtocolMaxTxsPerBlock, *overrides.MaxTxsPerBlock)
			}
			cfg.MaxTxsPerBlock = uint64(*overrides.MaxTxsPerBlock)
		}
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("autobahn: marshaling config: %w", err)
	}
	return data, nil
}
