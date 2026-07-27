package v1alpha1

// SeedSpec configures a CometBFT seed node: a peer-discovery server running the
// P2P transport and PEX reactor only. It hands a dialing node a spread of peer
// addresses, then drops the connection.
//
// A seed joins no consensus, syncs no blocks, and serves no queries — seid binds
// no RPC, gRPC, REST or EVM listener in this mode. Hence no snapshot source
// (nothing to restore), no snapshotGeneration (no history to snapshot), and no
// signing key or operator keyring (it signs nothing). Its data volume holds the
// peer store plus the block and state DBs seid opens and never writes.
//
// Peers come from SeiNodeSpec.Peers as in every mode; a seed with none is a
// valid bootstrap root that others dial without dialing out itself.
//
// A seed reachable from outside the cluster needs three things this spec does
// not provide: SeiNodeSpec.ExternalAddress set to the published `host:port`
// (otherwise seid advertises its in-cluster listen address over PEX and every
// node that learns the seed by gossip gets an unroutable peer), a load balancer
// and DNS record for TCP 26656 that outlive pod and node churn, and an ingress
// allowance for that port. The readiness probe cannot detect any of them
// missing — see readinessProbeForNode.
//
// A seed serves no RPC, so it has no /status and no `catching_up`: sync
// freshness is not a property a seed has. Monitor it on P2P metrics
// (tendermint_p2p_*) at the metrics port instead.
type SeedSpec struct {
	// NodeKey supplies the P2P identity (node_key.json) this seed presents.
	//
	// Required, where the validator's NodeKey is optional, because a seed's
	// NodeID is published: operators dial `NodeID@host:port` and the
	// secret-connection handshake verifies the pinned value, so a changed NodeID
	// silently breaks every client carrying the old one. A Secret-sourced key
	// survives pod recreation and PVC loss; one left to `seid init` regenerates
	// onto the data volume. Carrying the identity to another cluster means
	// replicating the Secret there — the controller reads it, it does not move
	// it. Treat the NodeID as a one-way door.
	//
	// The Secret name is immutable, so rotating the identity means deleting and
	// recreating the SeiNode — and announcing the new NodeID. There is no fast
	// path for a leaked key: the old NodeID stays dialable until every client
	// ships a new default, so a thief keeps impersonating a bootstrap anchor.
	//
	// Give each seed a distinct Secret. Nothing rejects two seeds sharing one,
	// and they would present the same NodeID — collapsing the redundant anchors
	// into a single entry in every dialer's peer store.
	NodeKey NodeKeySource `json:"nodeKey"`
}
