// Package tomlpatch provides merge-patch utilities for TOML and JSON documents.
//
// It lives in the contract module because both the sidecar (config-patch,
// config-apply, state-sync) and the seictl CLI (config, genesis, patch) need
// it, and neither may import the other: the CLI must stay free of the chain
// graph the sidecar carries. A copy in each would put the same drift risk
// across a module boundary that this migration exists to remove.
package tomlpatch

import "maps"

// Merge performs a recursive merge-patch of patch into original.
// nil values in the patch delete the corresponding key from original.
// Non-map patches replace the original entirely.
func Merge(original, patch any) any {
	patchMap, patchIsMap := patch.(map[string]any)
	if !patchIsMap {
		return patch
	}
	originalMap, originalIsMap := original.(map[string]any)
	if !originalIsMap {
		originalMap = make(map[string]any)
	}
	result := make(map[string]any)
	maps.Copy(result, originalMap)
	for key, patchAt := range patchMap {
		if patchAt == nil {
			delete(result, key)
		} else if originalAt, exists := result[key]; exists {
			result[key] = Merge(originalAt, patchAt)
		} else {
			result[key] = patchAt
		}
	}
	return result
}
