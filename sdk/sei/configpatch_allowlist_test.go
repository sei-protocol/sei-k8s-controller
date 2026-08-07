package sei

import "testing"

// configPatchAllowlistCases is the parity contract for the ConfigPatch runtime-safe
// allowlist. The allowlist deliberately lives in THREE hand-maintained copies — this
// package's configPatchKeyAllowed, internal/task.configPatchKeyAllowed, and the CRD's
// CEL family rule on SeiNodeTaskSpec — because this SDK's apimachinery-free boundary
// forbids sharing a single Go predicate across them. This table pins both Go copies
// to the same truth so they cannot drift silently; keep it byte-identical to the
// sibling table in internal/task (configpatch_allowlist_test.go). The CEL leg is
// covered by the envtest admission cases.
var configPatchAllowlistCases = []struct {
	key  string
	want bool
}{
	// In-family (runtime-safe toggles the controller does not re-assert).
	{"chain.occ_enabled", true},
	{"giga_executor.enabled", true},
	{"giga_executor.occ_enabled", true},
	{"mempool.max-txs", true},
	{"mempool.size", true},
	// Out-of-family / near-misses that must be rejected.
	{"chain.occ_enabled_extra", false},      // == match must be exact
	{"chain", false},                        // family root without member
	{"giga_executor", false},                // prefix without the dot
	{"gigaexecutor.enabled", false},         // missing separator
	{"mempoolx.size", false},                // prefix-of-prefix, not the family
	{"network.p2p.persistent_peers", false}, // controller re-asserts this on update plans
	{"consensus.timeout_commit", false},
	{"", false},
}

// TestConfigPatchKeyAllowedParity pins sdk/sei.configPatchKeyAllowed to the shared
// allowlist contract. internal/task has a mirror test against the identical table; a
// divergence between the two Go copies surfaces as one of the two tests failing.
func TestConfigPatchKeyAllowedParity(t *testing.T) {
	for _, tc := range configPatchAllowlistCases {
		if got := configPatchKeyAllowed(tc.key); got != tc.want {
			t.Errorf("configPatchKeyAllowed(%q) = %t, want %t", tc.key, got, tc.want)
		}
	}
}
