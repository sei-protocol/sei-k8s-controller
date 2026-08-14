package main

import (
	"strings"
	"testing"

	"github.com/sei-protocol/sei-k8s-controller/sidecar/server"
)

// The refusal is the only thing standing between a dropped environment variable
// and an unauthenticated signing API, so pin every combination rather than just
// the failing one — a future edit that widens the guard's escape hatch should
// break a test, not a cluster.
func TestCheckKeyringNeedsAuthn(t *testing.T) {
	cases := []struct {
		name           string
		keyringBackend string
		authnMode      string
		wantErr        bool
	}{
		{
			name:           "keyring with no authn is refused",
			keyringBackend: server.BackendFile,
			authnMode:      server.AuthnModeUnauthenticated,
			wantErr:        true,
		},
		{
			name: "the test backend is refused too — it is an unencrypted " +
				"operator keyring, so an open listener is worse, not better",
			keyringBackend: server.BackendTest,
			authnMode:      server.AuthnModeUnauthenticated,
			wantErr:        true,
		},
		{
			name:           "keyring behind trusted-header authn is allowed",
			keyringBackend: server.BackendFile,
			authnMode:      server.AuthnModeTrustedHeader,
			wantErr:        false,
		},
		{
			name: "no keyring with no authn is allowed — nothing to sign with, " +
				"which is the non-validator node case",
			keyringBackend: "",
			authnMode:      server.AuthnModeUnauthenticated,
			wantErr:        false,
		},
		{
			name:           "no keyring behind authn is allowed",
			keyringBackend: "",
			authnMode:      server.AuthnModeTrustedHeader,
			wantErr:        false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkKeyringNeedsAuthn(tc.keyringBackend, tc.authnMode)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected a refusal, got nil")
				}
				// The message is what an operator debugging a CrashLoopBackOff
				// reads, so keep it pointing at both halves of the problem.
				for _, want := range []string{"SEI_KEYRING_BACKEND", "SEI_SIDECAR_AUTHN_MODE"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error should name %s; got %q", want, err.Error())
					}
				}
				return
			}
			if err != nil {
				t.Errorf("expected nil, got %v", err)
			}
		})
	}
}

// AuthnModeUnauthenticated is the empty string, so an unset SEI_SIDECAR_AUTHN_MODE
// and an explicit "unauthenticated" reach the guard identically. Pinned because
// the guard's correctness depends on it: if the constant ever gained a non-empty
// value, an unset variable would stop matching and the refusal would go quiet.
func TestUnauthenticatedIsTheZeroValue(t *testing.T) {
	if server.AuthnModeUnauthenticated != "" {
		t.Fatalf("AuthnModeUnauthenticated = %q, want the empty string; "+
			"checkKeyringNeedsAuthn relies on an unset env var resolving to it",
			server.AuthnModeUnauthenticated)
	}
}

// Required only tests that a value was supplied. Both an empty value and a
// relative one resolve every path against the working directory instead of the
// node's data volume, and the sidecar serves normally either way — it creates
// config/, data/ and sidecar.db wherever it happens to be running.
func TestValidateHome(t *testing.T) {
	cases := []struct {
		name    string
		home    string
		wantErr string
	}{
		{name: "empty", home: "", wantErr: "set but empty"},
		{name: "whitespace only", home: "   ", wantErr: "set but empty"},
		{name: "tab only", home: "\t", wantErr: "set but empty"},
		// A relative path carries the same consequence as an empty one.
		{name: "bare relative", home: "sei", wantErr: "absolute path"},
		{name: "dot relative", home: "./sei", wantErr: "absolute path"},
		{name: "parent relative", home: "../sei", wantErr: "absolute path"},
		{name: "absolute", home: "/home/nonroot/.sei"},
		{name: "absolute unnormalized", home: "/home/nonroot/.sei///"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHome(tc.home)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("expected nil, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected a refusal containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error: got %q, want substring %q", err.Error(), tc.wantErr)
			}
			if !strings.Contains(err.Error(), "SEI_HOME") {
				t.Errorf("error should name SEI_HOME; got %q", err.Error())
			}
		})
	}
}
