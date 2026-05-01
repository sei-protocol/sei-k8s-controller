package planner

import (
	"testing"
)

func TestMergeOverrides_ForcesLoggingLevelInfo(t *testing.T) {
	cases := []struct {
		name string
		user map[string]string
	}{
		{"user_debug", map[string]string{overrideKeyLoggingLevel: "debug"}},
		{"user_warn", map[string]string{overrideKeyLoggingLevel: "warn"}},
		{"user_info", map[string]string{overrideKeyLoggingLevel: "info"}},
		{"user_empty", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged := mergeOverrides(nil, tc.user)
			if got := merged[overrideKeyLoggingLevel]; got != enforcedLoggingLevel {
				t.Fatalf("logging.level = %q, want %q", got, enforcedLoggingLevel)
			}
		})
	}
}

func TestMergeOverrides_PreservesNonForcedUserKeys(t *testing.T) {
	merged := mergeOverrides(
		map[string]string{"network.p2p.external_address": "ctrl.address:26656"},
		map[string]string{
			"network.p2p.external_address": "user.address:26656",
			"storage.snapshot_interval":    "1000",
			overrideKeyLoggingLevel:        "debug",
		},
	)
	if got := merged["network.p2p.external_address"]; got != "user.address:26656" {
		t.Errorf("user override should win for non-forced key: got %q", got)
	}
	if got := merged["storage.snapshot_interval"]; got != "1000" {
		t.Errorf("user key should pass through: got %q", got)
	}
	if got := merged[overrideKeyLoggingLevel]; got != enforcedLoggingLevel {
		t.Errorf("forced key should override user: got %q, want %q", got, enforcedLoggingLevel)
	}
}

func TestMergeOverrides_NestedCallsConvergeToInfo(t *testing.T) {
	inner := mergeOverrides(
		map[string]string{"a": "1"},
		map[string]string{"b": "2"},
	)
	outer := mergeOverrides(inner, map[string]string{overrideKeyLoggingLevel: "debug"})
	if got := outer[overrideKeyLoggingLevel]; got != enforcedLoggingLevel {
		t.Fatalf("nested merge with user debug: logging.level = %q, want %q", got, enforcedLoggingLevel)
	}
}
