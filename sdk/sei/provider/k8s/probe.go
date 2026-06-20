package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

// This file is the canonical home for the two-stage readiness probe and the
// Sei CometBFT unwrapped-envelope handling (WS-E LLD §5.6) — the single source
// of truth, since the controller's equivalents are unexported in
// internal/seitask and cannot be imported. The gate proves consensus
// (catching_up == false AND height > 1), not just liveness (§5.5).

// tendermintStatusResponse models the subset of Tendermint /status the gate
// needs. Sei's CometBFT fork sometimes returns the body unwrapped (no JSON-RPC
// envelope), so both shapes are accepted and resolved via Result/SyncInfo.
type tendermintStatusResponse struct {
	Result *struct {
		SyncInfo syncInfo `json:"sync_info"`
	} `json:"result,omitempty"`
	SyncInfo syncInfo `json:"sync_info"`
}

type syncInfo struct {
	LatestBlockHeight string `json:"latest_block_height"`
	CatchingUp        bool   `json:"catching_up"`
}

// syncInfo returns the sync_info from whichever envelope shape carried it. The
// unwrapped (bare) shape is the fallback, matching the controller's
// latestHeight() resolution.
func (r *tendermintStatusResponse) syncInfo() syncInfo {
	if r.Result != nil && r.Result.SyncInfo.LatestBlockHeight != "" {
		return r.Result.SyncInfo
	}
	return r.SyncInfo
}

// evmRPCResponse models a JSON-RPC envelope with a string result, used to
// confirm eth_blockNumber returned a well-formed, error-free reply.
type evmRPCResponse struct {
	Result string `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// probeReady runs the canonical two-stage readiness gate against one node:
//
//  1. TM /status: latest_block_height > 1 AND sync_info.catching_up == false —
//     consensus is producing blocks and the node is caught up, not replaying.
//  2. EVM eth_blockNumber: HTTP 200 with a non-empty, error-free result — the
//     JSON-RPC listener is bound (TM height alone does not prove this).
//
// Both stages share the same timeout/interval. tmRPC and evmRPC are the node's
// .status.endpoint URLs.
func probeReady(ctx context.Context, hc *http.Client, tmRPC, evmRPC, resource string, timeout, interval time.Duration) error {
	if err := waitForCaughtUp(ctx, hc, tmRPC, timeout, interval); err != nil {
		return classifyWaitErr(err, resource,
			fmt.Sprintf("TM /status not caught up within %s", timeout))
	}
	if err := waitForEVMReady(ctx, hc, evmRPC, timeout, interval); err != nil {
		return classifyWaitErr(err, resource,
			fmt.Sprintf("EVM eth_blockNumber not ready within %s", timeout))
	}
	return nil
}

// classifyWaitErr maps a terminal wait error onto a Class: a parent cancel
// (context.Canceled) is an explicit abort (ClassCanceled), while an elapsed
// poll budget (context.DeadlineExceeded) is a readiness timeout (ClassTimeout).
// A chaos harness branches on the two outcomes.
func classifyWaitErr(err error, resource, timeoutMsg string) error {
	if errors.Is(err, context.Canceled) {
		return &sei.Error{Class: sei.ClassCanceled, Resource: resource,
			Err: fmt.Errorf("aborted while waiting: %w", err)}
	}
	return &sei.Error{Class: sei.ClassTimeout, Resource: resource,
		Err: fmt.Errorf("%s: %w", timeoutMsg, err)}
}

// waitForCaughtUp polls Tendermint /status until height > 1 AND catching_up is
// false. A connection error or non-200 keeps polling (listener not yet up); a
// catching-up node keeps polling until it converges or the timeout fires.
func waitForCaughtUp(ctx context.Context, hc *http.Client, tmRPC string, timeout, interval time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, interval, timeout, true, func(ctx context.Context) (bool, error) {
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
		si := parsed.syncInfo()
		if si.CatchingUp {
			return false, nil
		}
		if h := si.LatestBlockHeight; h == "" || h == "0" || h == "1" {
			return false, nil
		}
		return true, nil
	})
}

// waitForEVMReady POSTs eth_blockNumber and requires HTTP 200 plus a non-empty,
// error-free result — the gate proving the EVM listener is bound before its URL
// is published as dial-ready. Non-200 / connection-refused keeps polling.
func waitForEVMReady(ctx context.Context, hc *http.Client, evmRPC string, timeout, interval time.Duration) error {
	const body = `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`
	return wait.PollUntilContextTimeout(ctx, interval, timeout, true, func(ctx context.Context) (bool, error) {
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
		if parsed.Error != nil || parsed.Result == "" {
			return false, nil
		}
		return true, nil
	})
}
