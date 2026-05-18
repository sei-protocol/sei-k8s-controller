// Package sidecartransport builds the http.RoundTripper the controller
// uses to call a SeiNode sidecar through its kube-rbac-proxy front.
//
// The transport reads the controller's projected ServiceAccount token
// and injects Authorization: Bearer <token> on every request so the
// proxy can run TokenReview + SubjectAccessReview against the K8s API.
// Token refresh is handled by k8s.io/client-go/transport.
package sidecartransport

import (
	"net/http"

	"k8s.io/client-go/transport"
)

// DefaultServiceAccountTokenPath is the projected SA token kubelet
// mounts on every pod by default.
const DefaultServiceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// Config builds the bearer-token round-tripper.
type Config struct {
	// TokenPath overrides the SA token location (tests inject a temp
	// file). Defaults to DefaultServiceAccountTokenPath.
	TokenPath string

	// Base is the underlying RoundTripper; defaults to http.DefaultTransport.
	Base http.RoundTripper
}

// New wraps cfg.Base with a transport that adds Authorization: Bearer
// <token> on each request and resets the token cache on 401.
func New(cfg Config) http.RoundTripper {
	if cfg.TokenPath == "" {
		cfg.TokenPath = DefaultServiceAccountTokenPath
	}
	if cfg.Base == nil {
		// Clone so process-global DefaultTransport mutations don't leak in.
		cfg.Base = http.DefaultTransport.(*http.Transport).Clone()
	}
	ts := transport.NewCachedFileTokenSource(cfg.TokenPath)
	return transport.ResettableTokenSourceWrapTransport(ts)(cfg.Base)
}
