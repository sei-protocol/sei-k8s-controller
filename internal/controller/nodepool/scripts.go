package nodepool

import _ "embed"

//go:embed scripts/generate.sh
var genesisScript string

//go:embed scripts/prep-node.sh
var prepNodeScript string
