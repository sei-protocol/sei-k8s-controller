package sidecartransport

import "crypto/tls"

// insecureTLSClientConfig returns the tls.Config used by the controller
// against the in-pod kube-rbac-proxy. See the package SECURITY POSTURE
// comment for why InsecureSkipVerify is acceptable in this position.
func insecureTLSClientConfig() *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint:gosec // see SECURITY POSTURE in package doc
	}
}
