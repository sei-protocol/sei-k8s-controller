// Package sidecartransport builds the http.RoundTripper the controller
// uses to call a SeiNode sidecar when the sidecar is fronted by a
// kube-rbac-proxy (spec.sidecar.tls set).
//
// The transport:
//   - Trusts the proxy's cert-manager-issued self-signed cert.
//   - Reads the controller's projected ServiceAccount token via
//     k8s.io/client-go/transport.NewCachedFileTokenSource, which
//     refreshes from disk every ~minute and invalidates immediately
//     on a 401 response.
//   - Injects Authorization: Bearer <token> on every request.
//
// # SECURITY POSTURE — InsecureSkipVerify
//
// The kube-rbac-proxy cert is cert-manager self-signed; no external
// chain to verify. Authn comes from the bearer token + TokenReview,
// not cert identity. Trade-off: a pod with services/endpoints write
// in the namespace can MITM and exfiltrate the controller SA token,
// replayable against kube-apiserver until kubelet rotates it (~12
// min). Compensating control: that namespace-local write already
// grants equivalent CRD-manipulation power. CA pinning is the
// cleaner path — follow-up to #224.
package sidecartransport

import (
	"net/http"

	"k8s.io/client-go/transport"
)

// DefaultServiceAccountTokenPath is the projected SA token kubelet
// mounts on every pod by default.
const DefaultServiceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// Config builds the trusted-header round-tripper.
type Config struct {
	// TokenPath overrides the SA token location (tests inject a temp
	// file). Defaults to DefaultServiceAccountTokenPath.
	TokenPath string

	// Base is the underlying RoundTripper; defaults to a
	// defaultTLSTransport(). Settable so callers can swap in a
	// fake (tests) or a CA-pinned transport (future hardening).
	Base http.RoundTripper
}

// New wraps cfg.Base with a transport that adds Authorization: Bearer
// <token> on each request and resets the token cache on 401.
func New(cfg Config) http.RoundTripper {
	if cfg.TokenPath == "" {
		cfg.TokenPath = DefaultServiceAccountTokenPath
	}
	if cfg.Base == nil {
		cfg.Base = defaultTLSTransport()
	}
	ts := transport.NewCachedFileTokenSource(cfg.TokenPath)
	return transport.ResettableTokenSourceWrapTransport(ts)(cfg.Base)
}

func defaultTLSTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = insecureTLSClientConfig()
	return t
}
