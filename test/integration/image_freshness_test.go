//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"
)

// TestStaleSeidImages pins the gate's two edges: which tag shapes it reads, and
// which it must leave alone. Runs without a cluster.
func TestStaleSeidImages(t *testing.T) {
	const (
		fresh = "189176372795.dkr.ecr.us-east-2.amazonaws.com/sei/sei-chain:nightly-20260805-0d9c675"
		stale = "189176372795.dkr.ecr.us-east-2.amazonaws.com/sei/sei-chain:nightly-20260731-2d2628f"
		// The upgrade suites pin commits, carry no date, and must never trip this.
		pinned = "189176372795.dkr.ecr.us-east-2.amazonaws.com/sei/sei-chain:fbc0d9342ca28887958013170e4020d93cacdbfa"
	)
	now := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name      string
		env       map[string]string
		wantStale bool
	}{
		{"same-day image passes", map[string]string{"SEID_IMAGE": fresh}, false},
		{"five-day-old image is refused", map[string]string{"SEID_IMAGE": stale}, true},
		{"pinned commit tag is not date-checked", map[string]string{"SEID_IMAGE": pinned}, false},
		{"unset image is not checked", map[string]string{}, false},
		{"mock flavour is checked too", map[string]string{
			"SEID_IMAGE":      fresh,
			"SEID_IMAGE_MOCK": strings.Replace(stale, "nightly-", "mock-nightly-", 1),
		}, true},
		{"chaos flavour is checked too", map[string]string{
			"SEID_IMAGE": fresh,
			"SEID_IMAGE_CHAOS": strings.Replace(stale, "nightly-",
				"mock_chain_validation-mock_balances-nightly-", 1),
		}, true},
		{"budget is overridable", map[string]string{
			"SEID_IMAGE":               stale,
			"SEID_IMAGE_MAX_AGE_HOURS": "240",
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SEI_NODE_CLUSTER", "harbor")
			for _, k := range append(seidImageEnvVars, "SEID_IMAGE_MAX_AGE_HOURS") {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got := staleSeidImages(now) != ""
			if got != tc.wantStale {
				t.Fatalf("staleSeidImages stale=%v, want %v", got, tc.wantStale)
			}
		})
	}

	// Without a cluster every suite skips, so the gate must not fire locally.
	t.Run("local run is exempt", func(t *testing.T) {
		t.Setenv("SEI_NODE_CLUSTER", "")
		t.Setenv("SEID_IMAGE", stale)
		if msg := staleSeidImages(now); msg != "" {
			t.Fatalf("expected no gate without a cluster, got: %s", msg)
		}
	})
}
