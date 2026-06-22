package sei

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Readiness probes are the generally-useful chain-provisioning lifecycle piece:
// "the node has joined consensus and is actually serving," not merely "the pod is
// Running." They are mode-agnostic — they take a published endpoint URL and speak
// HTTP, so the k8s/local/docker providers, the seitask Task steps, and external
// harnesses all share one implementation. Kept stdlib-only (no apimachinery) so
// the core package stays dependency-free for lightweight external consumers.

// ProbeInterval is the readiness poll cadence. A var so tests can shrink it.
var ProbeInterval = 2 * time.Second

// tendermintStatus models just enough of CometBFT /status to gate readiness. The
// Sei fork sometimes returns the body unwrapped (no JSON-RPC envelope), so both
// shapes are accepted.
type tendermintStatus struct {
	Result *struct {
		SyncInfo syncInfo `json:"sync_info"`
	} `json:"result,omitempty"`
	SyncInfo syncInfo `json:"sync_info"`
}

type syncInfo struct {
	LatestBlockHeight string `json:"latest_block_height"`
	CatchingUp        bool   `json:"catching_up"`
}

func (s *tendermintStatus) sync() syncInfo {
	if s.Result != nil && s.Result.SyncInfo.LatestBlockHeight != "" {
		return s.Result.SyncInfo
	}
	return s.SyncInfo
}

// WaitCaughtUp blocks until tmRPC reports a committed height > 1 with
// catching_up == false — proof the node has joined consensus and is current, not
// merely answering. This is the readiness contract for a follower joining a chain.
// hc may be nil (http.DefaultClient). The caller's ctx bounds the wait; a deadline
// surfaces as context.DeadlineExceeded (IsTimeout reports true).
func WaitCaughtUp(ctx context.Context, hc *http.Client, tmRPC string) error {
	return pollUntil(ctx, fmt.Sprintf("%s /status caught-up", tmRPC), func(ctx context.Context) bool {
		body, ok := getJSON(ctx, hc, http.MethodGet, tmRPC+"/status", "")
		if !ok {
			return false
		}
		var s tendermintStatus
		if json.Unmarshal(body, &s) != nil {
			return false
		}
		si := s.sync()
		h, err := strconv.ParseInt(si.LatestBlockHeight, 10, 64)
		return err == nil && h > 1 && !si.CatchingUp
	})
}

// evmResponse models a JSON-RPC envelope with a string result.
type evmResponse struct {
	Result string `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// WaitEVMServing blocks until evmRPC answers eth_blockNumber with a non-empty,
// error-free result — proof the EVM JSON-RPC listener is bound and serving (a TM
// height does not prove the EVM listener accepts connections). hc may be nil.
func WaitEVMServing(ctx context.Context, hc *http.Client, evmRPC string) error {
	const body = `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`
	return pollUntil(ctx, fmt.Sprintf("%s eth_blockNumber", evmRPC), func(ctx context.Context) bool {
		raw, ok := getJSON(ctx, hc, http.MethodPost, evmRPC, body)
		if !ok {
			return false
		}
		var r evmResponse
		if json.Unmarshal(raw, &r) != nil {
			return false
		}
		return r.Error == nil && r.Result != ""
	})
}

// pollUntil ticks done() every ProbeInterval until it returns true or ctx fires,
// running once immediately. A stdlib poll loop — no apimachinery in core.
func pollUntil(ctx context.Context, what string, done func(context.Context) bool) error {
	tick := time.NewTicker(ProbeInterval)
	defer tick.Stop()
	for {
		if done(ctx) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: not ready before deadline: %w", what, ctx.Err())
		case <-tick.C:
		}
	}
}

// getJSON performs one request and returns the body on HTTP 200, else ok=false
// (a connection error or non-200 just means "not ready yet" — keep polling).
func getJSON(ctx context.Context, hc *http.Client, method, url, body string) ([]byte, bool) {
	if hc == nil {
		hc = http.DefaultClient
	}
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, false
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false
	}
	return out, true
}
