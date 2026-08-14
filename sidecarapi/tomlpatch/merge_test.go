package tomlpatch

import (
	"reflect"
	"testing"
)

// nestedKey is the map key the nested-merge case shares across its three maps.
const nestedKey = "obj"

func TestMerge(t *testing.T) {
	tests := []struct {
		name     string
		original any
		patch    any
		expected any
	}{
		{
			name:     "merge two maps",
			original: map[string]any{"a": 1, "b": 2},
			patch:    map[string]any{"b": 3, "c": 4},
			expected: map[string]any{"a": 1, "b": 3, "c": 4},
		},
		{
			name:     "patch is not a map",
			original: map[string]any{"a": 1},
			patch:    "string value",
			expected: "string value",
		},
		{
			name:     "original is not a map",
			original: "original string",
			patch:    map[string]any{"a": 1},
			expected: map[string]any{"a": 1},
		},
		{
			name:     "null value deletes key",
			original: map[string]any{"a": 1, "b": 2},
			patch:    map[string]any{"b": nil},
			expected: map[string]any{"a": 1},
		},
		{
			// The key is the same in all three maps on purpose — that is what
			// makes this a merge rather than a replacement.
			name:     "nested map merge",
			original: map[string]any{nestedKey: map[string]any{"x": 1, "y": 2}},
			patch:    map[string]any{nestedKey: map[string]any{"y": 3, "z": 4}},
			expected: map[string]any{nestedKey: map[string]any{"x": 1, "y": 3, "z": 4}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := Merge(tt.original, tt.patch)
			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("Merge() = %v, want %v", result, tt.expected)
			}
		})
	}
}
