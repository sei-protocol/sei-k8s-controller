package sei

// Object-label / peer-wiring producer contract — the canonical single source of
// truth. seictl's copy is unexported (internal/), so the SDK authors these once
// here. Changing a value is a fleet-wide breaking change: chaos selectors,
// follower-discovery queries (node list -l sei.io/seinetwork=<net>,sei.io/role=node),
// and seictl all match the exact literals.
const (
	// LabelRole keys the role an object plays in a network.
	LabelRole = "sei.io/role"

	// RoleNode is the LabelRole value the SDK stamps on every provisioned RPC
	// SeiNode (mirrors provisionnode roleValueNode).
	RoleNode = "node"

	// LabelSeiNetwork keys the owning SeiNetwork name. It is both the node object
	// label and the peer-selector value nodes match on.
	LabelSeiNetwork = "sei.io/seinetwork"
)

// FieldOwner is the SSA field manager the SDK applies under — a distinct writer
// from seictl ("seictl"). Stable: renaming it orphans field ownership on objects
// the SDK already created.
const FieldOwner = "sei-sdk"
