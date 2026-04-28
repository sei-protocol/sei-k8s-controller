package v1alpha1

// ValidatorSpec configures a consensus-participating validator node.
// Validators bootstrap the same way as full nodes but participate in consensus.
type ValidatorSpec struct {
	// Snapshot configures how the node obtains its initial chain state.
	// When absent the node block-syncs from genesis.
	// +optional
	Snapshot *SnapshotSource `json:"snapshot,omitempty"`

	// GenesisCeremony indicates this validator participates in a group genesis
	// ceremony. Set by the SeiNodeDeployment controller — not intended for direct use.
	// +optional
	GenesisCeremony *GenesisCeremonyNodeConfig `json:"genesisCeremony,omitempty"`

	// SigningKey declares the source of this validator's consensus signing
	// key (priv_validator_key.json). When omitted, the node runs as a
	// non-signing observer — suitable for pre-sync (Phase 1 of the
	// validator-migration runbook) or for genesis-ceremony bootstraps that
	// produce keys on-cluster. Mutually exclusive with GenesisCeremony.
	// +optional
	SigningKey *SigningKeySource `json:"signingKey,omitempty"`
}

// SigningKeySource declares where a validator's consensus signing key
// material comes from. Exactly one variant must be set. Variants are
// mutually exclusive — a validator has one consensus identity.
//
// +kubebuilder:validation:XValidation:rule="(has(self.secret) ? 1 : 0) == 1",message="exactly one signing key source must be set"
type SigningKeySource struct {
	// Secret loads the signing key from a Kubernetes Secret in the
	// SeiNode's namespace.
	// +optional
	Secret *SecretSigningKeySource `json:"secret,omitempty"`

	// Future siblings: TMKMS, Remote, Vault. When added, update the
	// XValidation exactly-one rule.
}

// SecretSigningKeySource references a Kubernetes Secret containing the
// validator's consensus signing key. The Secret must contain a data key
// `priv_validator_key.json` holding the Tendermint validator key
// (consensus identity), mounted read-only at
// $SEI_HOME/config/priv_validator_key.json. Rotating this value without
// a paired on-chain MsgEditValidator will cause the validator to miss
// blocks.
//
// priv_validator_state.json (CometBFT's slashing-protection ledger) is
// owned by seid on the data PVC and is created automatically on first
// start. The controller does not inject this file.
type SecretSigningKeySource struct {
	// SecretName is the name of a Secret in the SeiNode's namespace.
	// The controller never creates, mutates, or deletes this Secret —
	// its lifecycle is fully external (kubectl, ESO, CSI Secrets Store, etc.).
	// Immutable: re-pointing the consensus key on a running validator
	// is a slashing risk; force delete-and-recreate to rotate.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="secretName is immutable"
	SecretName string `json:"secretName"`
}

// GenesisCeremonyNodeConfig holds per-node genesis ceremony parameters.
// Populated by the SeiNodeDeployment controller when genesis is configured.
type GenesisCeremonyNodeConfig struct {
	// ChainID of the genesis network.
	// +kubebuilder:validation:MinLength=1
	ChainID string `json:"chainId"`

	// StakingAmount is the self-delegation amount for this validator's gentx.
	// +kubebuilder:validation:MinLength=1
	StakingAmount string `json:"stakingAmount"`

	// AccountBalance is the initial coin balance to fund this validator's
	// genesis account. The node's own address is discovered during identity
	// generation — no cross-node coordination needed.
	// +kubebuilder:validation:MinLength=1
	AccountBalance string `json:"accountBalance"`

	// GenesisParams is a JSON string of genesis parameter overrides merged
	// on top of sei-config's GenesisDefaults(). Applied before gentx generation.
	// +optional
	GenesisParams string `json:"genesisParams,omitempty"`

	// Index is the node's ordinal within the group (0-based).
	// +kubebuilder:validation:Minimum=0
	Index int32 `json:"index"`
}
