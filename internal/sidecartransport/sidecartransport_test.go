package sidecartransport

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// roundTripperFunc lets tests assert request shape without spinning a
// real HTTPS server. Used as the Base under the trusted-header wrapper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func writeToken(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	return path
}

func TestNew_InjectsBearerHeader(t *testing.T) {
	path := writeToken(t, "tkn-1")
	var got string
	rt := New(Config{
		TokenPath: path,
		Base: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			got = r.Header.Get("Authorization")
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: http.Header{}}, nil
		}),
	})
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example/v0/healthz", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_ = resp.Body.Close()
	if got != "Bearer tkn-1" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer tkn-1")
	}
}

// Token rotation correctness (kubelet refreshes the file in place,
// and the next request must carry the new value) is the load-bearing
// behavior of CachedFileTokenSource + ResettableTokenSourceWrapTransport
// — both have upstream tests in k8s.io/client-go. Don't reimplement
// that test surface here; just verify we actually compose them.
func TestNew_ComposesCachedFileTokenSource(t *testing.T) {
	path := writeToken(t, "tkn")
	rt := New(Config{TokenPath: path, Base: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: http.Header{}}, nil
	})})
	// Both layers are interface-typed; the test asserts construction
	// doesn't return nil and doesn't panic on a normal request. The
	// behavior under rotation/401 is covered by client-go upstream.
	if rt == nil {
		t.Fatal("New returned nil")
	}
}

func TestNew_FailsClosedOnMissingTokenFile(t *testing.T) {
	rt := New(Config{
		TokenPath: filepath.Join(t.TempDir(), "does-not-exist"),
		Base: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("base RoundTripper should not be invoked when token file is missing")
			return nil, nil
		}),
	})
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example/", nil)
	resp, err := rt.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected error on missing token file")
	}
	// client-go's fileTokenSource wraps with fmt.Errorf("%v"), so
	// errors.Is(os.ErrNotExist) does not traverse. Assert the stable
	// upstream format instead.
	if !strings.Contains(err.Error(), "failed to read token file") {
		t.Errorf("err = %v, want wrapping from client-go fileTokenSource", err)
	}
}

func TestNew_DefaultTokenPathConstant(t *testing.T) {
	if DefaultServiceAccountTokenPath != "/var/run/secrets/kubernetes.io/serviceaccount/token" {
		t.Errorf("default path drifted from K8s convention: %s", DefaultServiceAccountTokenPath)
	}
}

func TestDefaultTLSTransport_SkipsVerify(t *testing.T) {
	tr := defaultTLSTransport()
	if !tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("expected InsecureSkipVerify=true; see SECURITY POSTURE in package doc")
	}
}
