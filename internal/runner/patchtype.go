package runner

import "k8s.io/apimachinery/pkg/types"

// applyPatchTypeMarker returns the patch type used for server-side apply.
// Split out so apply.go's imports stay tight.
func applyPatchTypeMarker() types.PatchType { return types.ApplyPatchType }
