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
// handling: the canonical probe must resolve sync_info from both the JSON-RPC
// envelope and the bare shape, and surface catching_up from whichever carried
// the height.
func TestSyncInfo_EnvelopeDecode(t *testing.T) {
	cases := []struct {
		name           string
		body           string
		wantHeight     string
		wantCatchingUp bool
	}{
		{"jsonrpc envelope, caught up", `{"result":{"sync_info":{"latest_block_height":"42","catching_up":false}}}`, "42", false},
		{"jsonrpc envelope, catching up", `{"result":{"sync_info":{"latest_block_height":"42","catching_up":true}}}`, "42", true},
		{"bare envelope, caught up", `{"sync_info":{"latest_block_height":"7","catching_up":false}}`, "7", false},
		{"bare envelope, catching up", `{"sync_info":{"latest_block_height":"7","catching_up":true}}`, "7", true},
		{"empty", `{}`, "", false},
		{"envelope present but height empty falls back to bare", `{"result":{"sync_info":{}},"sync_info":{"latest_block_height":"9","catching_up":false}}`, "9", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var r tendermintStatusResponse
			if err := json.Unmarshal([]byte(tc.body), &r); err != nil {
				t.Fatal(err)
			}
			si := r.syncInfo()
			if si.LatestBlockHeight != tc.wantHeight {
				t.Errorf("height = %q, want %q", si.LatestBlockHeight, tc.wantHeight)
			}
			if si.CatchingUp != tc.wantCatchingUp {
				t.Errorf("catchingUp = %v, want %v", si.CatchingUp, tc.wantCatchingUp)
			}
		})
	}
}

// statusFixture returns a /status JSON body for the given height and catching_up
// flag, in either envelope shape.
func statusFixture(height string, catchingUp, enveloped bool) string {
	si := map[string]any{"latest_block_height": height, "catching_up": catchingUp}
	if enveloped {
		b, _ := json.Marshal(map[string]any{resultKey: map[string]any{"sync_info": si}})
		return string(b)
	}
	b, _ := json.Marshal(map[string]any{"sync_info": si})
	return string(b)
}

func TestWaitForCaughtUp_Gate(t *testing.T) {
	cases := []struct {
		name       string
		height     string
		catchingUp bool
		enveloped  bool
		wantPass   bool
	}{
		{"caught up, height 42, envelope", "42", false, true, true},
		{"caught up, height 42, bare", "42", false, false, true},
		{"catching up blocks even at high height", "999", true, true, false},
		{"height 1 blocks (genesis not advanced)", "1", false, true, false},
		{"height 0 blocks", "0", false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(statusFixture(tc.height, tc.catchingUp, tc.enveloped)))
			}))
			defer srv.Close()

			err := waitForCaughtUp(context.Background(), srv.Client(), srv.URL, 120*time.Millisecond, 20*time.Millisecond)
			if tc.wantPass && err != nil {
				t.Fatalf("gate should pass, got %v", err)
			}
			if !tc.wantPass && err == nil {
				t.Fatalf("gate should NOT pass (height=%s catchingUp=%v)", tc.height, tc.catchingUp)
			}
		})
	}
}

// TestWaitForCaughtUp_ConvergesAfterCatchup proves a node that starts catching
// up and then converges passes the gate (the gate polls, it doesn't latch).
func TestWaitForCaughtUp_ConvergesAfterCatchup(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		catchingUp := hits.Add(1) < 3 // first two polls catching up, then caught up
		_, _ = w.Write([]byte(statusFixture("100", catchingUp, true)))
	}))
	defer srv.Close()

	if err := waitForCaughtUp(context.Background(), srv.Client(), srv.URL, time.Second, 10*time.Millisecond); err != nil {
		t.Fatalf("gate should pass once the node converges: %v", err)
	}
}

func TestWaitForEVMReady_BoundListener(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, resultKey: "0x10"})
	}))
	defer srv.Close()
	if err := waitForEVMReady(context.Background(), srv.Client(), srv.URL, time.Second, 10*time.Millisecond); err != nil {
		t.Fatalf("waitForEVMReady: %v", err)
	}
}

func TestWaitForEVMReady_NeverBound_TimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable) // listener not up
	}))
	defer srv.Close()
	if err := waitForEVMReady(context.Background(), srv.Client(), srv.URL, 120*time.Millisecond, 20*time.Millisecond); err == nil {
		t.Fatal("expected timeout when EVM never binds")
	}
}

// TestProbeReady_EVMGateBlocksOnCaughtUpButUnboundEVM is the readiness-vs-
// discoverability gate: TM is caught up but the EVM listener never binds, so
// probeReady must fail (ClassTimeout) rather than declaring the node ready.
func TestProbeReady_EVMGateBlocksOnCaughtUpButUnboundEVM(t *testing.T) {
	var tmHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet { // TM /status — caught up
			tmHits.Add(1)
			_, _ = w.Write([]byte(statusFixture("50", false, true)))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable) // EVM never binds
	}))
	defer srv.Close()

	err := probeReady(context.Background(), srv.Client(), srv.URL, srv.URL, "SeiNode ns/rpc-0", 150*time.Millisecond, 20*time.Millisecond)
	if err == nil {
		t.Fatal("probeReady should fail when EVM never binds even though TM is caught up")
	}
	if !sei.IsTimeout(err) {
		t.Fatalf("EVM-not-ready should be ClassTimeout, got %v", err)
	}
	if tmHits.Load() == 0 {
		t.Fatal("TM stage was never reached")
	}
}
