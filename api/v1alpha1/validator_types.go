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
