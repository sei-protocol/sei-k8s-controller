//go:build integration

package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
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
		{"digest-pinned stale image is refused", map[string]string{
			"SEID_IMAGE": stale + "@sha256:" + strings.Repeat("a", 64),
		}, true},
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

// TestResolveSeidImages pins how an image input follows a Flux ImagePolicy: a
// literal value wins, and a policy that cannot vouch for its status is refused.
// Runs without a cluster.
func TestResolveSeidImages(t *testing.T) {
	const (
		repo   = "189176372795.dkr.ecr.us-east-2.amazonaws.com/sei/sei-chain"
		tag    = "nightly-20261026-1186580"
		digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
		ref    = "flux-system/sei-chain-nightly"
	)
	ready := func(status, message string) []any {
		return []any{map[string]any{"type": fluxReadyCondition, "status": status, "message": message}}
	}
	policy := func(mutate func(status map[string]any)) *unstructured.Unstructured {
		status := map[string]any{
			"observedGeneration": int64(2),
			"conditions":         ready("True", ""),
			"latestRef":          map[string]any{"name": repo, "tag": tag, "digest": digest},
		}
		if mutate != nil {
			mutate(status)
		}
		u := &unstructured.Unstructured{Object: map[string]any{"status": status}}
		u.SetAPIVersion("image.toolkit.fluxcd.io/v1")
		u.SetKind("ImagePolicy")
		u.SetNamespace("flux-system")
		u.SetName("sei-chain-nightly")
		u.SetGeneration(2)
		return u
	}

	for _, tc := range []struct {
		name    string
		image   string
		ref     string
		policy  *unstructured.Unstructured
		want    string
		wantErr string
	}{
		{name: "resolves name:tag@digest", ref: ref,
			policy: policy(nil), want: repo + ":" + tag + "@" + digest},
		{name: "omits an unreflected digest", ref: ref,
			policy: policy(func(s map[string]any) { s["latestRef"].(map[string]any)["digest"] = "" }),
			want:   repo + ":" + tag},
		{name: "literal image wins", image: repo + ":pinned", ref: ref,
			want: repo + ":pinned"},
		{name: "no policy leaves the input unset", want: ""},
		{name: "malformed ref", ref: "sei-chain-nightly", wantErr: "want <namespace>/<name>"},
		{name: "missing policy", ref: ref, wantErr: "not found"},
		{name: "not Ready", ref: ref,
			policy: policy(func(s map[string]any) {
				s["conditions"] = ready("False", "no tags")
			}),
			wantErr: "not Ready: no tags"},
		{name: "no Ready condition", ref: ref,
			policy:  policy(func(s map[string]any) { delete(s, "conditions") }),
			wantErr: "no Ready condition"},
		{name: "status for an older spec", ref: ref,
			policy:  policy(func(s map[string]any) { s["observedGeneration"] = int64(1) }),
			wantErr: "generation 1, spec is at 2"},
		{name: "empty latestRef", ref: ref,
			policy:  policy(func(s map[string]any) { delete(s, "latestRef") }),
			wantErr: "status.latestRef is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range seidImageEnvVars {
				t.Setenv(k, "")
				t.Setenv(k+imagePolicySuffix, "")
			}
			t.Setenv("SEID_IMAGE", tc.image)
			t.Setenv("SEID_IMAGE"+imagePolicySuffix, tc.ref)
			var objs []runtime.Object
			if tc.policy != nil {
				objs = append(objs, tc.policy)
			}
			calls := 0
			newClient := func() (dynamic.Interface, error) {
				calls++
				return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
					map[schema.GroupVersionResource]string{imagePolicyGVR: "ImagePolicyList"}, objs...), nil
			}

			err := resolveSeidImages(context.Background(), newClient)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := os.Getenv("SEID_IMAGE"); got != tc.want {
				t.Fatalf("SEID_IMAGE = %q, want %q", got, tc.want)
			}
			if tc.policy == nil && calls != 0 {
				t.Fatalf("built a client with nothing to resolve")
			}
		})
	}
}
