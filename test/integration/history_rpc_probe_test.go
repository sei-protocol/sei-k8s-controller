//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	seiconfig "github.com/sei-protocol/sei-config"
)

type historyRPCSample struct {
	Event      string    `json:"event"`
	Node       string    `json:"node"`
	Phase      string    `json:"phase"`
	StartedAt  time.Time `json:"startedAt"`
	LatencyMs  float64   `json:"latencyMs"`
	Outcome    string    `json:"outcome"`
	Height     *uint64   `json:"height,omitempty"`
	HTTPStatus int       `json:"httpStatus,omitempty"`
	Error      string    `json:"error,omitempty"`
}

func historyHead(ctx context.Context, client *http.Client, endpoint string) (sample historyRPCSample) {
	start := time.Now()
	sample.StartedAt = start.UTC()
	defer func() { sample.LatencyMs = time.Since(start).Seconds() * 1000 }()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
	if err != nil {
		sample.Outcome, sample.Error = "request-error", err.Error()
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err == nil {
		defer resp.Body.Close()
		sample.HTTPStatus = resp.StatusCode
		if resp.StatusCode != http.StatusOK {
			sample.Outcome = "http-error"
			return
		}
		var reply struct {
			ID      int
			JSONRPC string
			Result  string
			Error   *struct {
				Code    int
				Message string
			}
		}
		err = json.NewDecoder(resp.Body).Decode(&reply)
		if err == nil {
			if reply.Error != nil {
				sample.Outcome, sample.Error = "rpc-error", fmt.Sprintf("%d: %s", reply.Error.Code, reply.Error.Message)
				return
			}
			h, parseErr := strconv.ParseUint(strings.TrimPrefix(reply.Result, "0x"), 16, 64)
			if reply.ID != 1 || reply.JSONRPC != "2.0" || !strings.HasPrefix(reply.Result, "0x") || parseErr != nil {
				sample.Outcome = "invalid-reply"
				return
			}
			sample.Outcome, sample.Height = "ok", &h
			return
		}
	}
	sample.Outcome, sample.Error = "read-error", err.Error()
	if errors.Is(err, context.DeadlineExceeded) {
		sample.Outcome = "timeout"
	}
	return
}

// Each node gets its own loop so a slow RPC cannot delay probes on its peers.
func monitorHistoryRPC(ctx context.Context, t *testing.T, ch *chain, validators int) (setPhase func(string), stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	var phase atomic.Value
	phase.Store("before-load")
	targets := make(map[string]string)
	for i := range validators {
		name := validatorName(ch.network.Name(), i)
		targets[name] = fmt.Sprintf("http://%s.%s.svc:%d", name, ch.network.Namespace(), seiconfig.PortEVMHTTP)
	}
	for i, node := range ch.rpcNodes {
		targets[rpcNodeName(ch.network.Name(), i)] = node.EVMRPC()
	}
	client := &http.Client{Timeout: 2 * time.Second}
	var workers sync.WaitGroup
	for node, endpoint := range targets {
		workers.Add(1)
		go func() {
			defer workers.Done()
			tick := time.NewTicker(5 * time.Second)
			defer tick.Stop()
			for {
				current := phase.Load().(string)
				sample := historyHead(ctx, client, endpoint)
				if ctx.Err() != nil {
					return
				}
				sample.Event, sample.Node, sample.Phase = "rpc-responsiveness", node, current
				line, err := json.Marshal(sample)
				if err != nil {
					t.Errorf("encode RPC probe: %v", err)
					return
				}
				t.Log(string(line))
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
			}
		}()
	}
	return func(next string) { phase.Store(next) }, func() { cancel(); workers.Wait() }
}

func TestHistoryRPCProbeResults(t *testing.T) {
	for _, tc := range []struct {
		body, outcome string
		code          int
	}{
		{`{"jsonrpc":"2.0","id":1,"result":"0x2a"}`, "ok", 200},
		{`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"busy"}}`, "rpc-error", 200},
		{`{"jsonrpc":"2.0","id":1,"result":"bad"}`, "invalid-reply", 200},
		{"", "http-error", 429},
	} {
		t.Run(tc.outcome, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.code); fmt.Fprint(w, tc.body) }))
			defer srv.Close()
			s := historyHead(t.Context(), srv.Client(), srv.URL)
			if s.Outcome != tc.outcome || s.HTTPStatus != tc.code || (tc.outcome == "ok" && (s.Height == nil || *s.Height != 42)) {
				t.Fatalf("unexpected probe: %+v", s)
			}
		})
	}
}
