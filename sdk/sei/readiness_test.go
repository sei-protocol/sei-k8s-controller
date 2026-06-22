package sei

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func init() { ProbeInterval = 5 * time.Millisecond } // keep readiness tests fast

func TestWaitCaughtUp_GatesOnHeightAndCatchingUp(t *testing.T) {
	// Server: catching_up=true for the first 2 polls, then caught up at height 5.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			_, _ = fmt.Fprint(w, `{"result":{"sync_info":{"latest_block_height":"1","catching_up":true}}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"result":{"sync_info":{"latest_block_height":"5","catching_up":false}}}`)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := WaitCaughtUp(ctx, srv.Client(), srv.URL); err != nil {
		t.Fatalf("WaitCaughtUp: %v", err)
	}
	if got := calls.Load(); got < 3 {
		t.Fatalf("returned before catching_up cleared: %d polls", got)
	}
}

func TestWaitCaughtUp_AcceptsUnwrappedEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"sync_info":{"latest_block_height":"7","catching_up":false}}`) // bare Sei shape
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := WaitCaughtUp(ctx, srv.Client(), srv.URL); err != nil {
		t.Fatalf("unwrapped envelope: %v", err)
	}
}

func TestWaitCaughtUp_TimesOutWhileCatchingUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"result":{"sync_info":{"latest_block_height":"9","catching_up":true}}}`)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	err := WaitCaughtUp(ctx, srv.Client(), srv.URL)
	if err == nil {
		t.Fatal("expected timeout while catching_up=true")
	}
	if !IsTimeout(err) {
		t.Fatalf("expected IsTimeout, got %v", err)
	}
}

func TestWaitEVMServing_GatesOnNonEmptyResult(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 2 {
			w.WriteHeader(http.StatusServiceUnavailable) // listener not bound yet
			return
		}
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":"0x10"}`)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := WaitEVMServing(ctx, srv.Client(), srv.URL); err != nil {
		t.Fatalf("WaitEVMServing: %v", err)
	}
}

func TestWaitEVMServing_KeepsPollingOnRPCError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"error":{"message":"not ready"}}`)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if err := WaitEVMServing(ctx, srv.Client(), srv.URL); err == nil {
		t.Fatal("expected timeout: an error-result must not count as serving")
	}
}
