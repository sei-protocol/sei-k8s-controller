package sei

// Object-label / peer-wiring producer contract — the canonical single source
// of truth (WS-E LLD §5.6, one-way door D5). The controller's seitask and
// seictl copies are unexported (internal/), so the SDK authors these once here;
// those copies converge onto these constants post-#175. Changing a value is a
// fleet-wide breaking change: chaos selectors, follower-discovery queries
// (node list -l sei.io/seinetwork=<net>,sei.io/role=node), and seictl all match
// on the exact literals.
const (
	// LabelRole keys the role an object plays in a network.
	LabelRole = "sei.io/role"

	// RoleNode is the LabelRole value the SDK stamps on every provisioned
	// follower SeiNode (mirrors provisionnode roleValueNode).
	RoleNode = "node"

	// LabelSeiNetwork keys the owning SeiNetwork name. It is both the follower
	// object label and the peer-selector value followers match on.
	LabelSeiNetwork = "sei.io/seinetwork"
)

// FieldOwner is the SSA field manager the SDK applies under — a distinct writer
// from seictl ("seictl") and seitask ("seitask-provision-node"). Stable from
// day one (one-way door D6): renaming it orphans field ownership on objects the
// SDK already created.
const FieldOwner = "sei-sdk"
