package tasks

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	ethcrypto "github.com/ethereum/go-ethereum/crypto"

	"github.com/sei-protocol/sei-chain/sei-cosmos/crypto/keyring"
	sdk "github.com/sei-protocol/sei-chain/sei-cosmos/types"
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
	autobahnEVMWSPort    = 8546

	ed25519PubKeyTypeName  = "tendermint/PubKeyEd25519"
	ed25519PrivKeyTypeName = "tendermint/PrivKeyEd25519"
)

// autobahnIdentity is the Autobahn slice of a validator's identity.json: the
// public inputs gen-autobahn-config reads from a node directory, in the text
// encodings the provider writes to validator_pubkey.txt, node_pubkey.txt,
// autobahn_address.txt, evmrpc_url.txt and evm_voter.txt. EVMVoter is the
// EVM address of the validator's operator key; the assembler writes it only
// when the ceremony enables EVM governance.
type autobahnIdentity struct {
	ValidatorPubKey string `json:"validator_pubkey"`
	NodePubKey      string `json:"node_pubkey"`
	AutobahnAddress string `json:"autobahn_address"`
	EVMRPCURL       string `json:"evmrpc_url"`
	EVMVoter        string `json:"evm_voter,omitempty"`
}

// autobahnValidator matches config.AutobahnValidator's JSON.
type autobahnValidator struct {
	ValidatorKey string `json:"validator_key"`
	NodeKey      string `json:"node_key"`
	Address      string `json:"address"`
	EVMRPC       string `json:"evmrpc"`
	EVMVoter     string `json:"evm_voter,omitempty"`
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
	Validators         []autobahnValidator    `json:"validators"`
	MaxTxsPerBlock     uint64                 `json:"max_txs_per_block"`
	MaxTxsPerSecond    *uint64                `json:"max_txs_per_second"`
	AllowEmptyBlocks   bool                   `json:"allow_empty_blocks"`
	EnableEvmProxy     *bool                  `json:"enable_evm_proxy,omitempty"`
	BlockInterval      string                 `json:"block_interval"`
	ViewTimeout        string                 `json:"view_timeout"`
	PersistentStateDir string                 `json:"persistent_state_dir"`
	DialInterval       string                 `json:"dial_interval"`
	BlockDB            autobahnBlockDBConfig  `json:"block_db"`
	EVMGovernance      *autobahnEVMGovernance `json:"evm_governance,omitempty"`
}

// autobahnEVMGovernance matches config.EVMGovernance's JSON.
type autobahnEVMGovernance struct {
	Params             autobahnEVMGovernanceParams `json:"params"`
	StartingProposalID uint64                      `json:"starting_proposal_id,omitempty"`
	AppliedUpgrades    []string                    `json:"applied_upgrades,omitempty"`
}

// autobahnEVMGovernanceParams matches config.EVMGovernanceParams's JSON:
// periods are duration strings, deposits wei, ratios 1e18-scaled.
type autobahnEVMGovernanceParams struct {
	VotingPeriod          string   `json:"voting_period"`
	ExpeditedVotingPeriod string   `json:"expedited_voting_period"`
	MinDeposit            *big.Int `json:"min_deposit"`
	MaxDepositPeriod      string   `json:"max_deposit_period"`
	MinExpeditedDeposit   *big.Int `json:"min_expedited_deposit"`
	Quorum                uint64   `json:"quorum"`
	Threshold             uint64   `json:"threshold"`
	VetoThreshold         uint64   `json:"veto_threshold"`
	ExpeditedQuorum       uint64   `json:"expedited_quorum"`
	ExpeditedThreshold    uint64   `json:"expedited_threshold"`
}

// autobahnAddressFor returns the P2P host:port the ceremony advertises for a
// validator, the same in-cluster DNS name peers.json uses.
func autobahnAddressFor(nodeName, namespace string) string {
	return fmt.Sprintf("%s-0.%s.%s.svc.cluster.local:%d", nodeName, nodeName, namespace, autobahnP2PPort)
}

// autobahnEVMRPCURLFor returns the EVM-RPC URL the ceremony advertises for a
// validator's mempool shard: its EVM WebSocket listener, which validators dial
// to proxy transactions to the shard's owner.
func autobahnEVMRPCURLFor(nodeName, namespace string) string {
	return fmt.Sprintf("ws://%s-0.%s.%s.svc.cluster.local:%d", nodeName, nodeName, namespace, autobahnEVMWSPort)
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

// readEVMVoter returns the EVM address of the operator key generate-gentx
// added to the test keyring: the address gentx associates with the
// validator's account, and the one evm_governance names as its voter.
func readEVMVoter(homeDir string) (string, error) {
	kb, err := keyring.New(sdk.KeyringServiceName(), keyring.BackendTest, homeDir, nil)
	if err != nil {
		return "", fmt.Errorf("opening keyring: %w", err)
	}
	info, err := kb.Key(validatorKeyName)
	if err != nil {
		return "", fmt.Errorf("reading key %q: %w", validatorKeyName, err)
	}
	pub, err := ethcrypto.DecompressPubkey(info.GetPubKey().Bytes())
	if err != nil {
		return "", fmt.Errorf("key %q is not a secp256k1 key: %w", validatorKeyName, err)
	}
	return ethcrypto.PubkeyToAddress(*pub).Hex(), nil
}

// checkEVMVoter is the provider's evm_voter check: a hex address, not zero.
func checkEVMVoter(s string) error {
	if s == "" {
		return fmt.Errorf("missing")
	}
	if !common.IsHexAddress(s) {
		return fmt.Errorf("%q is not an address", s)
	}
	if common.HexToAddress(s) == (common.Address{}) {
		return fmt.Errorf("is the zero address")
	}
	return nil
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
	if err := checkRPCURL(id.EVMRPCURL); err != nil {
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

// checkRPCURL is the provider's utils.CheckRPCURL: a ws or wss URL with a host
// and no userinfo.
func checkRPCURL(s string) error {
	if s == "" {
		return fmt.Errorf("missing")
	}
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return fmt.Errorf("scheme %q, want ws or wss", u.Scheme)
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
	BlockInterval    string                          `json:"blockInterval,omitempty"`
	AllowEmptyBlocks *bool                           `json:"allowEmptyBlocks,omitempty"`
	MaxTxsPerBlock   *int64                          `json:"maxTxsPerBlock,omitempty"`
	EnableEvmProxy   *bool                           `json:"enableEvmProxy,omitempty"`
	EvmGovernance    *AutobahnEvmGovernanceOverrides `json:"evmGovernance,omitempty"`
}

// AutobahnEvmGovernanceOverrides enables evm_governance as
// gen-autobahn-config's --evm-governance-voting-period and
// --evm-governance-starting-proposal-id do. The command also writes the
// running binary's release list as applied_upgrades; the sidecar cannot read
// that list, so the operator supplies it as AppliedUpgrades, and empty starts
// the chain past no upgrade.
type AutobahnEvmGovernanceOverrides struct {
	VotingPeriod       string   `json:"votingPeriod"`
	StartingProposalID uint64   `json:"startingProposalId,omitempty"`
	AppliedUpgrades    []string `json:"appliedUpgrades,omitempty"`
}

// Bounds gen-autobahn-config and gov's params validation put on the voting
// period: the command's 2s floor, and params.MaxVotingPeriod.
const (
	autobahnMinGovVotingPeriod = 2 * time.Second
	autobahnMaxGovVotingPeriod = 30 * 24 * time.Hour
	// autobahnMaxUpgradeName is the upgrade precompile's MaxNameLength.
	autobahnMaxUpgradeName = 128
)

// weiPerUsei mirrors the gov precompile's deposit unit.
var weiPerUsei = big.NewInt(1_000_000_000_000)

// buildAutobahnEVMGovernance mirrors buildGenEVMGovernance: x/gov's default
// params (gov.DefaultParams) with VotingPeriod as the voting and deposit
// period and half of it as the expedited voting period.
func buildAutobahnEVMGovernance(o *AutobahnEvmGovernanceOverrides) (*autobahnEVMGovernance, error) {
	d, err := time.ParseDuration(o.VotingPeriod)
	if err != nil {
		return nil, fmt.Errorf("autobahn: evm_governance voting period %q: %w", o.VotingPeriod, err)
	}
	if d < autobahnMinGovVotingPeriod || d > autobahnMaxGovVotingPeriod {
		return nil, fmt.Errorf("autobahn: evm_governance voting period %s must be in [%s, %s]", d, autobahnMinGovVotingPeriod, autobahnMaxGovVotingPeriod)
	}
	// upgrade.CheckGenesisNames: non-empty, at most MaxNameLength, no repeats.
	seen := make(map[string]bool, len(o.AppliedUpgrades))
	for i, name := range o.AppliedUpgrades {
		switch {
		case name == "":
			return nil, fmt.Errorf("autobahn: evm_governance applied upgrade %d is empty", i)
		case len(name) > autobahnMaxUpgradeName:
			return nil, fmt.Errorf("autobahn: evm_governance applied upgrade %d is longer than %d bytes", i, autobahnMaxUpgradeName)
		case seen[name]:
			return nil, fmt.Errorf("autobahn: evm_governance applied upgrade %q repeats", name)
		}
		seen[name] = true
	}
	return &autobahnEVMGovernance{
		Params: autobahnEVMGovernanceParams{
			VotingPeriod:          d.String(),
			ExpeditedVotingPeriod: (d / 2).String(),
			MinDeposit:            new(big.Int).Mul(big.NewInt(10_000_000), weiPerUsei),
			MaxDepositPeriod:      d.String(),
			MinExpeditedDeposit:   new(big.Int).Mul(big.NewInt(20_000_000), weiPerUsei),
			Quorum:                334_000_000_000_000_000,
			Threshold:             500_000_000_000_000_000,
			VetoThreshold:         334_000_000_000_000_000,
			ExpeditedQuorum:       667_000_000_000_000_000,
			ExpeditedThreshold:    667_000_000_000_000_000,
		},
		StartingProposalID: o.StartingProposalID,
		AppliedUpgrades:    o.AppliedUpgrades,
	}, nil
}

// checkAutobahnVoters is the provider's validateEVMGovernance voter rule: with
// governance, every validator has a distinct valid evm_voter; without it, none
// has one.
func checkAutobahnVoters(validators []autobahnValidator, governance bool) error {
	seen := make(map[common.Address]bool, len(validators))
	for _, v := range validators {
		if !governance {
			if v.EVMVoter != "" {
				return fmt.Errorf("autobahn: validator %s sets evm_voter without evm_governance", v.ValidatorKey)
			}
			continue
		}
		if err := checkEVMVoter(v.EVMVoter); err != nil {
			return fmt.Errorf("autobahn: validator %s evm_voter: %w", v.ValidatorKey, err)
		}
		addr := common.HexToAddress(v.EVMVoter)
		if seen[addr] {
			return fmt.Errorf("autobahn: validator %s evm_voter %s is shared with another validator", v.ValidatorKey, addr.Hex())
		}
		seen[addr] = true
	}
	return nil
}

// buildAutobahnConfig renders autobahn.json for the given validators as
// `seid tendermint gen-autobahn-config <dirs> --output autobahn.json` does with
// its default flags: 2000 txs/block, empty blocks off, 400ms blocks, 1500ms
// view timeout, 10s dial interval, state persisted under data/autobahn with a
// 30s BlockDB retention, enable_evm_proxy absent so the provider's Or(true)
// applies, and no evm_governance. Overrides replace the defaults they name,
// checked against the provider's AutobahnFileConfig.Validate bounds; an
// EvmGovernance override requires an evm_voter on every validator, and its
// absence forbids one. Validators keep the order given, which is the
// ceremony's ordinal order.
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
		if overrides.EnableEvmProxy != nil {
			cfg.EnableEvmProxy = overrides.EnableEvmProxy
		}
		if overrides.EvmGovernance != nil {
			governance, err := buildAutobahnEVMGovernance(overrides.EvmGovernance)
			if err != nil {
				return nil, err
			}
			cfg.EVMGovernance = governance
		}
	}
	if err := checkAutobahnVoters(validators, cfg.EVMGovernance != nil); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("autobahn: marshaling config: %w", err)
	}
	return data, nil
}
