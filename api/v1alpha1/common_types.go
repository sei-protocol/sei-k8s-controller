package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// PeerSource is a union type — exactly one field must be set.
// +kubebuilder:validation:XValidation:rule="(has(self.ec2Tags) ? 1 : 0) + (has(self.static) ? 1 : 0) + (has(self.label) ? 1 : 0) == 1",message="exactly one of ec2Tags, static, or label must be set"
type PeerSource struct {
	// EC2Tags discovers peers by querying EC2 for running instances
	// matching the specified tags.
	// +optional
	EC2Tags *EC2TagsPeerSource `json:"ec2Tags,omitempty"`

	// Static provides a fixed list of peer addresses.
	// +optional
	Static *StaticPeerSource `json:"static,omitempty"`

	// Label discovers peers by selecting SeiNode resources via
	// Kubernetes labels. The controller resolves matching nodes to
	// stable headless Service DNS names and tracks them in
	// status.resolvedPeers. The sidecar queries each for its
	// Tendermint node ID at task execution time.
	// +optional
	Label *LabelPeerSource `json:"label,omitempty"`
}

// LabelPeerSource discovers peers by selecting SeiNode resources via
// Kubernetes labels. The controller resolves matching nodes to their
// headless Service DNS names ({name}-0.{name}.{namespace}.svc.cluster.local)
// and writes them to status.resolvedPeers on every reconcile.
//
// Controller-managed labels available on every SeiNode owned by a SeiNetwork:
//   - sei.io/chain — from .spec.genesis.chainId
//   - sei.io/seinetwork — owning SeiNetwork name (canonical key; controller
//     CR-selection matches on this)
//   - sei.io/seinetwork-ordinal — replica index (canonical key)
//   - sei.io/nodedeployment — owning SeiNetwork name (frozen selector key,
//     still stamped for selector continuity with external consumers)
//   - sei.io/nodedeployment-ordinal — replica index (frozen selector key)
//   - sei.io/role — always "validator" (a SeiNetwork is a validator pool)
//
// These reserved keys are always controller-stamped. (There is no
// sei.io/revision label — it was removed with the rollout state machine.)
type LabelPeerSource struct {
	// Selector is a set of key-value label pairs. SeiNode resources
	// matching ALL labels are included as peers.
	// +kubebuilder:validation:MinProperties=1
	Selector map[string]string `json:"selector"`

	// Namespace restricts discovery to a specific namespace.
	// When empty, defaults to the namespace of the discovering node.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// EC2TagsPeerSource discovers peers via EC2 tag filters in a specific region.
// node_id comes from the sei.io/node-id tag, not verified against the live peer;
// a forged tag is rejected at the CometBFT handshake (degrades connectivity,
// can't impersonate). Verify live if EC2Tags gets a real consumer.
type EC2TagsPeerSource struct {
	// Region is the AWS region to query for EC2 instances.
	// +kubebuilder:validation:MinLength=1
	Region string `json:"region"`

	// Tags are the EC2 instance tags to filter on.
	// +kubebuilder:validation:MinProperties=1
	Tags map[string]string `json:"tags"`
}

// StaticPeerSource provides a fixed list of peer addresses.
type StaticPeerSource struct {
	// Addresses is a list of peer addresses in "nodeId@host:port" format.
	// +kubebuilder:validation:MinItems=1
	Addresses []string `json:"addresses"`
}

// SnapshotSource identifies where to obtain a chain snapshot and how to
// configure the node after restore. Exactly one source variant must be set.
// +kubebuilder:validation:XValidation:rule="(has(self.s3) ? 1 : 0) + (has(self.stateSync) ? 1 : 0) == 1",message="exactly one of s3 or stateSync must be set"
type SnapshotSource struct {
	// S3 downloads a snapshot archive from an S3 bucket.
	// +optional
	S3 *S3SnapshotSource `json:"s3,omitempty"`

	// StateSync fetches snapshot chunks from peers via Tendermint state sync.
	// +optional
	StateSync *StateSyncSource `json:"stateSync,omitempty"`

	// TrustPeriod is the window during which the snapshot's block validators
	// are considered trustworthy. Must be long enough to cover the age of
	// the snapshot (e.g. "9999h0m0s" for old S3 snapshots).
	// +optional
	TrustPeriod string `json:"trustPeriod,omitempty"`

	// RpcServers declares the CometBFT rpc-servers used as light-client
	// witnesses (trust-point acquisition and verification), replacing the
	// controller-level canonical-syncer registry: when set, the registry is
	// not consulted for this node. It applies to both source variants — s3
	// restore verifies its trust point against the same witnesses state sync
	// does. Witnesses are not snapshot providers: snapshot chunks are
	// delivered over p2p by snapshot-serving peers, and at least one peer must
	// already have produced a snapshot for the bootstrap to succeed.
	//
	// Endpoints are bare host:port (no scheme), matching the registry shape;
	// unlike registry entries they are shape-validated at admission (IPv6
	// literals are not supported). Shape is the only validation: each endpoint
	// must serve this chain's CometBFT RPC — a wrong-chain or unreachable
	// endpoint passes admission and fails at runtime in the sidecar's
	// state-sync configure step (a wrong-chain witness yields a wrong-chain
	// trust point). Editing this field does not retarget an in-flight plan; it
	// takes effect on the next plan build.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MinItems=2
	// +kubebuilder:validation:items:Pattern=`^[^\s:/,]+:[0-9]{1,5}$`
	RpcServers []string `json:"rpcServers,omitempty"`

	// BackfillBlocks is the number of historical blocks to fetch from peers
	// after snapshot restore.
	// +optional
	BackfillBlocks int64 `json:"backfillBlocks,omitempty"`

	// BootstrapImage is a seid container image used for the bootstrap Job.
	// When set, the controller runs a one-shot Job with this image and the
	// seictl sidecar to prepare the node's data PVC before the main StatefulSet
	// starts. The Job restores a snapshot, applies config, and syncs to the
	// target height.
	// +optional
	BootstrapImage string `json:"bootstrapImage,omitempty"`
}

// S3SnapshotSource configures snapshot download from the platform
// snapshot bucket. The sidecar resolves objects under
// {SEI_SNAPSHOT_BUCKET}/{chainID}/state-sync/ and selects the latest
// snapshot via latest.txt.
type S3SnapshotSource struct {
	// TargetHeight is the block height the node should sync to after restoring.
	// +kubebuilder:validation:Minimum=1
	TargetHeight int64 `json:"targetHeight"`
}

// StateSyncSource enables Tendermint state sync, which fetches snapshot
// chunks from peers on the p2p network. The controller manages trust
// height and RPC server configuration automatically.
type StateSyncSource struct{}

// SnapshotGenerationConfig configures snapshot generation. One or more snapshot
// modes may be enabled by setting the corresponding sub-struct. A mode sub-struct
// being absent means that snapshot type is not produced. An empty
// SnapshotGenerationConfig (no sub-struct set) is rejected by the planner as a
// likely user typo.
type SnapshotGenerationConfig struct {
	// Tendermint configures Tendermint state-sync snapshot generation.
	// +optional
	Tendermint *TendermintSnapshotGenerationConfig `json:"tendermint,omitempty"`
}

// TendermintSnapshotGenerationConfig configures a node to produce Tendermint
// state-sync snapshots. The controller sets archival pruning and a
// system-default snapshot-interval in app.toml. Snapshots are written to the
// node's data volume.
type TendermintSnapshotGenerationConfig struct {
	// KeepRecent is the number of recent snapshots to retain on disk.
	// When Publish is set, must be at least 2 so the upload algorithm can
	// select the second-to-latest completed snapshot. Otherwise must be at
	// least 1.
	// +kubebuilder:validation:Minimum=1
	KeepRecent int32 `json:"keepRecent"`

	// Publish, when set, causes the sidecar to upload completed snapshots
	// to {SEI_SNAPSHOT_BUCKET}/{chainID}/. Absence means snapshots are kept
	// on disk only and are not uploaded.
	// +optional
	Publish *TendermintSnapshotPublishConfig `json:"publish,omitempty"`
}

// TendermintSnapshotPublishConfig configures how completed Tendermint
// snapshots are uploaded. Currently an empty struct — its presence on
// TendermintSnapshotGenerationConfig enables upload to the platform
// snapshot bucket. Fields may be added here in the future (e.g., bucket
// override, prefix) without a breaking change.
type TendermintSnapshotPublishConfig struct{}

// FreezeSpec holds a node at a block height. The node executes through
// Height-1, then stops while it continues to serve query RPC. seid refuses to
// freeze a validator, so only the non-consensus modes carry this sub-spec.
//
// Which history the node serves follows from its mode. Pruning happens at
// commit, so a stopped node stops pruning: a frozen full node serves only the
// window it had retained, while a frozen archive node serves every block below
// the freeze height.
type FreezeSpec struct {
	// Height is the block height at which the node stops executing; the node
	// serves blocks through Height-1. Immutable: the node has already stopped,
	// so lowering the height cannot un-execute and raising it would resume a
	// node that is read-only by contract. Replace the node to change it.
	//
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="height is immutable"
	Height int64 `json:"height"`
}

// ResultExportConfig configures the node to export block-execution results.
// One or more use-case sub-structs may be enabled. An empty ResultExportConfig
// (no sub-struct set) is rejected by the planner as a likely user typo.
type ResultExportConfig struct {
	// ShadowResult configures the node to generate and export shadow-result
	// pages, comparing local block execution results against a canonical
	// chain via app-hash divergence detection.
	// +optional
	ShadowResult *ShadowResultConfig `json:"shadowResult,omitempty"`
}

// ShadowResultConfig configures shadow-result generation. The sidecar queries
// the local RPC endpoint for block_results, uploads them in compressed NDJSON
// pages to the platform result-export bucket, and compares each page's app-hash
// against the canonical chain. The export task completes when divergence is
// detected.
type ShadowResultConfig struct {
	// CanonicalRPC is the HTTP RPC endpoint of the canonical chain node
	// to compare block-execution results against.
	// +kubebuilder:validation:MinLength=1
	CanonicalRPC string `json:"canonicalRpc"`
}

// SidecarConfig configures the sei-sidecar container.
type SidecarConfig struct {
	// Image overrides the sidecar container image for this node, in place of the
	// platform config's images.sidecar. Prefer leaving it unset so one value
	// governs the whole cell.
	//
	// The controller renders no command for the sidecar container, so the image's
	// entrypoint is the command. An image whose entrypoint does not match what
	// the controller expects fails quietly rather than loudly: the container
	// exits 0 and restarts while seid waits on /v0/healthz behind a startup
	// probe that tolerates roughly five days. Pin only to debug, and only to an
	// image built alongside the running controller.
	// +optional
	Image string `json:"image,omitempty"`

	// Port is the HTTP port the sidecar listens on.
	// +optional
	// +kubebuilder:default=7777
	Port int32 `json:"port,omitempty"`

	// Resources defines CPU/memory requests and limits for the sidecar container.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// ConfigValue supplies a typed value at a dotted TOML path in a config file.
type ConfigValue struct {
	// FileName names the config file, for example config.toml or app.toml.
	// Only config.toml and app.toml have a controller-generated base. Removing
	// an entry for any other file leaves its key in place on disk.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_-]+\.toml$`
	FileName string `json:"fileName"`

	// Key is a dotted TOML path, for example evm.enable.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$`
	Key string `json:"key"`

	// Value preserves the JSON type for the TOML merge. Scalars, arrays, and
	// tables are accepted. A value is required; explicit null is rejected.
	// A nested null is admitted by the schema but rejected at plan-build because
	// it has no TOML representation. Integers outside int64 range fall back to
	// float64 and may lose precision; numbers outside float64 range are rejected
	// at plan-build.
	Value apiextensionsv1.JSON `json:"value"`
}

// NodeIsolation controls whether a Sei pod may share a worker node.
// +kubebuilder:validation:Enum=Shared;Dedicated
type NodeIsolation string

const (
	NodeIsolationShared    NodeIsolation = "Shared"
	NodeIsolationDedicated NodeIsolation = "Dedicated"
)

// SchedulingConfig configures scheduling for a SeiNode or SeiNetwork.
type SchedulingConfig struct {
	// NodeIsolation requests shared or single-tenant worker-node placement.
	// There is deliberately no schema default: an unset field must remain unset
	// on reads so existing SeiNodes retain their legacy annotation fallback.
	// The controller resolves an unset value to Shared after that fallback.
	// +optional
	NodeIsolation NodeIsolation `json:"nodeIsolation,omitempty"`
}

// ConsensusEngine selects the consensus engine seid runs.
// +kubebuilder:validation:Enum=Tendermint;Autobahn
type ConsensusEngine string

const (
	ConsensusEngineTendermint ConsensusEngine = "Tendermint"
	ConsensusEngineAutobahn   ConsensusEngine = "Autobahn"
)

// ConsensusSpec selects the consensus engine and the application it drives.
// It is create-only on both Kinds: the engine is baked into the ceremony's
// artifact and the node's home directory.
// +kubebuilder:validation:XValidation:rule="!(has(self.evmOnly) && self.evmOnly) || (has(self.engine) && self.engine == 'Autobahn')",message="evmOnly requires engine Autobahn: the EVM-only executor runs only under Autobahn consensus"
type ConsensusSpec struct {
	// Engine is the consensus engine. Tendermint when omitted. Autobahn makes the
	// genesis ceremony generate autobahn.json from every validator's identity and
	// points config.toml's autobahn-config-file at it on validators and followers.
	// +optional
	Engine ConsensusEngine `json:"engine,omitempty"`

	// EvmOnly replaces the ABCI application with the EVM-only executor (chain ID
	// 713715). It requires engine Autobahn and is rejected on a seed. The node
	// serves only the EVM JSON-RPC listener on 8545: CometBFT RPC, REST and gRPC
	// are off, no cosmos-exporter is attached, and Ready proves that listener
	// answers, not sync distance.
	// +optional
	EvmOnly bool `json:"evmOnly,omitempty"`
}

// AutobahnCeremonySpec is the operator-settable slice of the ceremony's
// autobahn.json. Each field replaces one value gen-autobahn-config writes with
// its default flags; an omitted field keeps that default. Every validator and
// follower reads the same artifact, so these are network-wide and create-only.
// +kubebuilder:validation:XValidation:rule="!has(self.blockInterval) || duration(self.blockInterval) > duration('0s')",message="blockInterval must be a positive duration"
type AutobahnCeremonySpec struct {
	// BlockInterval is autobahn.json block_interval, a Go duration string such
	// as 400ms (the gen-autobahn-config default). Must be positive.
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$`
	// +optional
	BlockInterval string `json:"blockInterval,omitempty"`

	// AllowEmptyBlocks is autobahn.json allow_empty_blocks. Default false: the
	// chain sits at its last height until a transaction arrives, so an idle
	// Autobahn network at height 0 is healthy, not stuck.
	// +optional
	AllowEmptyBlocks *bool `json:"allowEmptyBlocks,omitempty"`

	// MaxTxsPerBlock is autobahn.json max_txs_per_block. Default 2000, which is
	// also the protocol ceiling (autobahn/types.MaxTxsPerBlock): the producer
	// clamps any larger value to 2000, so only lowering it has an effect.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=2000
	// +optional
	MaxTxsPerBlock *int64 `json:"maxTxsPerBlock,omitempty"`
}

// NetworkConsensusSpec is ConsensusSpec plus the ceremony inputs only a
// SeiNetwork can carry: a SeiNode consumes the ceremony's autobahn.json, it
// never generates one.
// +kubebuilder:validation:XValidation:rule="!has(self.autobahn) || (has(self.engine) && self.engine == 'Autobahn')",message="autobahn settings require engine Autobahn: they are written into the ceremony's autobahn.json, which only an Autobahn ceremony produces"
type NetworkConsensusSpec struct {
	ConsensusSpec `json:",inline"`

	// Autobahn tunes the autobahn.json the genesis ceremony writes. Omitted
	// fields keep gen-autobahn-config's defaults. Create-only.
	// +optional
	Autobahn *AutobahnCeremonySpec `json:"autobahn,omitempty"`
}

// Node returns the per-node slice of the spec, the shape every validator child
// carries; nil for a nil spec.
func (n *NetworkConsensusSpec) Node() *ConsensusSpec {
	if n == nil {
		return nil
	}
	return n.ConsensusSpec.DeepCopy()
}

// IsAutobahn reports whether the effective engine is Autobahn; false for nil.
func (n *NetworkConsensusSpec) IsAutobahn() bool {
	return n != nil && n.ConsensusSpec.IsAutobahn()
}

// EffectiveEngine returns the engine the network runs; Tendermint for nil.
func (n *NetworkConsensusSpec) EffectiveEngine() ConsensusEngine {
	if n == nil {
		return ConsensusEngineTendermint
	}
	return n.ConsensusSpec.EffectiveEngine()
}

// IsEvmOnly reports the effective evmOnly value; false for nil.
func (n *NetworkConsensusSpec) IsEvmOnly() bool {
	return n != nil && n.ConsensusSpec.IsEvmOnly()
}

// AutobahnSettings returns the ceremony overrides; nil when none are set.
func (n *NetworkConsensusSpec) AutobahnSettings() *AutobahnCeremonySpec {
	if n == nil {
		return nil
	}
	return n.Autobahn
}

// CommitsOnDemand reports whether the network commits blocks only when
// transactions arrive, so an unchanging height is idleness rather than a
// stall: Autobahn with allow_empty_blocks off (its default). Tendermint and
// Autobahn with empty blocks enabled commit continuously.
func (n *NetworkConsensusSpec) CommitsOnDemand() bool {
	if !n.IsAutobahn() {
		return false
	}
	return n.Autobahn == nil || n.Autobahn.AllowEmptyBlocks == nil || !*n.Autobahn.AllowEmptyBlocks
}

// EffectiveEngine returns the engine a nil or empty spec resolves to.
func (c *ConsensusSpec) EffectiveEngine() ConsensusEngine {
	if c == nil || c.Engine == "" {
		return ConsensusEngineTendermint
	}
	return c.Engine
}

// IsAutobahn reports whether the effective engine is Autobahn.
func (c *ConsensusSpec) IsAutobahn() bool {
	return c.EffectiveEngine() == ConsensusEngineAutobahn
}

// IsEvmOnly reports the effective evmOnly value; false for a nil spec.
func (c *ConsensusSpec) IsEvmOnly() bool {
	return c != nil && c.EvmOnly
}
