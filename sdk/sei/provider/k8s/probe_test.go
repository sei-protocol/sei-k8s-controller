package k8s

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

// TestSyncInfo_EnvelopeDecode covers the Sei CometBFT unwrapped-envelope
// handling: the probe must resolve sync_info from both the JSON-RPC envelope and
// the bare shape.
func TestSyncInfo_EnvelopeDecode(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantHeight string
	}{
		{"jsonrpc envelope", `{"result":{"sync_info":{"latest_block_height":"42"}}}`, "42"},
		{"bare envelope", `{"sync_info":{"latest_block_height":"7"}}`, "7"},
		{"empty", `{}`, ""},
		{"envelope present but height empty falls back to bare", `{"result":{"sync_info":{}},"sync_info":{"latest_block_height":"9"}}`, "9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var r tendermintStatusResponse
			if err := json.Unmarshal([]byte(tc.body), &r); err != nil {
				t.Fatal(err)
			}
			if got := r.syncInfo().LatestBlockHeight; got != tc.wantHeight {
				t.Errorf("height = %q, want %q", got, tc.wantHeight)
			}
		})
	}
}

// statusFixture returns a /status JSON body in either envelope shape.
func statusFixture(height string, enveloped bool) string {
	si := map[string]any{"latest_block_height": height}
	if enveloped {
		b, _ := json.Marshal(map[string]any{resultKey: map[string]any{"sync_info": si}})
		return string(b)
	}
	b, _ := json.Marshal(map[string]any{"sync_info": si})
	return string(b)
}

func TestProbeTendermint_Liveness(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(statusFixture("42", true)))
	}))
	defer srv.Close()
	if err := probeTendermint(context.Background(), srv.Client(), srv.URL, "SeiNetwork ns/net"); err != nil {
		t.Fatalf("probeTendermint should pass once /status answers: %v", err)
	}
}

// TestProbeTendermint_PollsUntilBound proves the probe polls — a listener that is
// down for the first few hits, then binds, passes.
func TestProbeTendermint_PollsUntilBound(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(statusFixture("42", false)))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := probeTendermint(ctx, srv.Client(), srv.URL, "SeiNetwork ns/net"); err != nil {
		t.Fatalf("probe should pass once the listener binds: %v", err)
	}
}

func TestProbeEVM_BoundListener(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, resultKey: "0x10"})
	}))
	defer srv.Close()
	if err := probeEVM(context.Background(), srv.Client(), srv.URL, "SeiNode ns/rpc-0"); err != nil {
		t.Fatalf("probeEVM: %v", err)
	}
}

// TestProbeEVM_DeadlineIsTimeout proves a never-bound listener under an elapsed
// deadline surfaces as sei.IsTimeout (context.DeadlineExceeded).
func TestProbeEVM_DeadlineIsTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable) // never binds
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	err := probeEVM(ctx, srv.Client(), srv.URL, "SeiNode ns/rpc-0")
	if err == nil {
		t.Fatal("expected error when EVM never binds")
	}
	if !sei.IsTimeout(err) {
		t.Fatalf("elapsed deadline should be sei.IsTimeout, got %v", err)
	}
}
