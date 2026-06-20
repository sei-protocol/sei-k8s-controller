package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
)

// This file holds the LIGHT serve-probe WaitReady runs after a resource reaches
// its Ready/Running phase: a single liveness check that the published endpoint is
// actually answering, NOT a consensus gate. It polls until the listener replies
// once (the URL is published before the seid listener binds) or the caller's ctx
// fires. The Sei CometBFT fork sometimes returns /status unwrapped, so both
// envelope shapes are accepted.

// probeInterval is the serve-probe poll cadence. A var, not a const, so tests
// can shrink it without inflating their deadlines.
var probeInterval = 1 * time.Second

// tendermintStatusResponse models just enough of TM /status to confirm a reply.
// Both the JSON-RPC-enveloped and the bare (unwrapped) Sei shapes are accepted.
type tendermintStatusResponse struct {
	Result *struct {
		SyncInfo syncInfo `json:"sync_info"`
	} `json:"result,omitempty"`
	SyncInfo syncInfo `json:"sync_info"`
}

type syncInfo struct {
	LatestBlockHeight string `json:"latest_block_height"`
}

// syncInfo returns the sync_info from whichever envelope shape carried it.
func (r *tendermintStatusResponse) syncInfo() syncInfo {
	if r.Result != nil && r.Result.SyncInfo.LatestBlockHeight != "" {
		return r.Result.SyncInfo
	}
	return r.SyncInfo
}

// probeTendermint polls TM /status until it returns HTTP 200 with a decodable
// body carrying a latest_block_height — proof the RPC listener is bound and
// serving. A light liveness check, not a catching_up gate.
func probeTendermint(ctx context.Context, hc *http.Client, tmRPC, resource string) error {
	err := wait.PollUntilContextCancel(ctx, probeInterval, true, func(ctx context.Context) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, tmRPC+"/status", nil)
		if err != nil {
			return false, err
		}
		resp, err := hc.Do(req)
		if err != nil {
			return false, nil
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return false, nil
		}
		var parsed tendermintStatusResponse
		if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
			return false, nil
		}
		return parsed.syncInfo().LatestBlockHeight != "", nil
	})
	if err != nil {
		return fmt.Errorf("%s: TM /status serve-probe: %w", resource, err)
	}
	return nil
}

// evmRPCResponse models a JSON-RPC envelope with a string result.
type evmRPCResponse struct {
	Result string `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// probeEVM POSTs eth_blockNumber until it returns HTTP 200 with a non-empty,
// error-free result — proof the EVM JSON-RPC listener is bound. A light liveness
// check; non-200 / connection-refused keeps polling.
func probeEVM(ctx context.Context, hc *http.Client, evmRPC, resource string) error {
	const body = `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`
	err := wait.PollUntilContextCancel(ctx, probeInterval, true, func(ctx context.Context) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, evmRPC, strings.NewReader(body))
		if err != nil {
			return false, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			return false, nil
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return false, nil
		}
		var parsed evmRPCResponse
		if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
			return false, nil
		}
		return parsed.Error == nil && parsed.Result != "", nil
	})
	if err != nil {
		return fmt.Errorf("%s: EVM eth_blockNumber serve-probe: %w", resource, err)
	}
	return nil
}
