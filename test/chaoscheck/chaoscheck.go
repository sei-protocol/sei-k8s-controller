// Package chaoscheck holds the post-fault assertions the chaos suites share
// and that need no cluster: they speak only to Tendermint RPC endpoints, so
// they unit-test against an httptest server.
//
// The nightly suite's recovery gate asks whether the chain is live and every
// validator pod is Ready. That does not notice validators that came back
// healthy but disagree on state: a recovery bug that corrupts one node's
// store shows up as a different app hash long before it halts anything.
// WaitAgree is that check.
package chaoscheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

// pollInterval is how often WaitAgree and HoldsHeight sample; a var so tests
// can shrink it.
var pollInterval = 2 * time.Second

// maxReadFailures is how many consecutive failed height reads HoldsHeight
// tolerates before it gives up, so one dropped request does not fail a
// scenario but a node that has gone silent does.
const maxReadFailures = 3

// DivergenceError reports nodes that returned different app hashes for the same
// height. It is never retried: a hash that differs now will not converge.
type DivergenceError struct {
	Height int64
	// Hashes maps each RPC endpoint to the app hash it reported.
	Hashes map[string]string
}

func (e *DivergenceError) Error() string {
	urls := make([]string, 0, len(e.Hashes))
	for u := range e.Hashes {
		urls = append(urls, u)
	}
	slices.Sort(urls)
	parts := make([]string, 0, len(urls))
	for _, u := range urls {
		parts = append(parts, fmt.Sprintf("%s=%s", u, e.Hashes[u]))
	}
	return fmt.Sprintf("app hash diverged at height %d: %s", e.Height, strings.Join(parts, " "))
}

// commitResponse models the part of CometBFT /commit this package reads. The
// Sei fork sometimes returns the body without the JSON-RPC envelope (the same
// quirk sdk/sei accepts for /status), so both shapes are decoded.
type commitResponse struct {
	Result       *commitBody `json:"result,omitempty"`
	SignedHeader signedHdr   `json:"signed_header"`
}

type commitBody struct {
	SignedHeader signedHdr `json:"signed_header"`
}

type signedHdr struct {
	Header struct {
		AppHash string `json:"app_hash"`
	} `json:"header"`
}

func (c *commitResponse) appHash() string {
	if c.Result != nil && c.Result.SignedHeader.Header.AppHash != "" {
		return c.Result.SignedHeader.Header.AppHash
	}
	return c.SignedHeader.Header.AppHash
}

// AppHash returns the app hash in the header of the block at height, as
// reported by the node at tmRPC. An unreachable node, a height it does not
// have, or an empty hash is an error: an empty hash would otherwise compare
// equal across nodes and hide a failure.
func AppHash(ctx context.Context, hc *http.Client, tmRPC string, height int64) (string, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	url := fmt.Sprintf("%s/commit?height=%d", strings.TrimRight(tmRPC, "/"), height)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build request %s: %w", url, err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("get %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("get %s: status %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", url, err)
	}
	var c commitResponse
	if err := json.Unmarshal(body, &c); err != nil {
		return "", fmt.Errorf("decode %s: %w", url, err)
	}
	h := c.appHash()
	if h == "" {
		return "", fmt.Errorf("%s returned no app_hash at height %d", url, height)
	}
	return strings.ToUpper(h), nil
}

// CommonHeight returns the highest height every node has committed, less
// margin: the latest height of the slowest node, pulled back so a node that is
// a block or two behind has the height too. It returns an error if any node
// cannot report a height, or if the result would be below 1.
func CommonHeight(ctx context.Context, hc *http.Client, tmRPCs []string, margin int64) (int64, error) {
	if len(tmRPCs) == 0 {
		return 0, errors.New("no RPC endpoints")
	}
	lowest := int64(-1)
	for _, u := range tmRPCs {
		h, ok := sei.LatestHeight(ctx, hc, u)
		if !ok {
			return 0, fmt.Errorf("%s: no height", u)
		}
		if lowest < 0 || h < lowest {
			lowest = h
		}
	}
	if lowest-margin < 1 {
		return 0, fmt.Errorf("slowest node is at height %d, too low for margin %d", lowest, margin)
	}
	return lowest - margin, nil
}

// AgreeOn fetches the app hash at height from every node and fails with a
// *DivergenceError if they are not all equal. Any fetch error is returned
// as-is, so callers can tell "could not read" from "disagrees".
func AgreeOn(ctx context.Context, hc *http.Client, tmRPCs []string, height int64) error {
	hashes := make(map[string]string, len(tmRPCs))
	var first string
	diverged := false
	for _, u := range tmRPCs {
		h, err := AppHash(ctx, hc, u, height)
		if err != nil {
			return err
		}
		hashes[u] = h
		if first == "" {
			first = h
		} else if h != first {
			diverged = true
		}
	}
	if diverged {
		return &DivergenceError{Height: height, Hashes: hashes}
	}
	return nil
}

// WaitAgree waits until every node can report a common height, then checks that
// they agree on its app hash. Reads that fail (a node still catching up, a
// blip) are retried until ctx ends; a divergence is returned at once.
func WaitAgree(ctx context.Context, hc *http.Client, tmRPCs []string, margin int64) error {
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	var lastErr error
	for {
		h, err := CommonHeight(ctx, hc, tmRPCs, margin)
		if err == nil {
			err = AgreeOn(ctx, hc, tmRPCs, h)
			var div *DivergenceError
			if err == nil || errors.As(err, &div) {
				return err
			}
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("nodes did not reach a comparable height before deadline: %w (last: %v)", ctx.Err(), lastErr)
		case <-tick.C:
		}
	}
}

// HoldsHeight asserts that the node at tmRPC does not commit blocks: after
// settle (in-flight blocks finish), its height advances by at most maxAdvance
// over window. It is the stall check for a fault that removes quorum. A node
// that cannot report a height at all is an error, since an unreachable node
// proves nothing about the chain.
func HoldsHeight(
	ctx context.Context, hc *http.Client, tmRPC string, settle, window time.Duration, maxAdvance int64,
) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(settle):
	}
	start, ok := sei.LatestHeight(ctx, hc, tmRPC)
	if !ok {
		return fmt.Errorf("%s: no height after settling", tmRPC)
	}
	deadline := time.NewTimer(window)
	defer deadline.Stop()
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	failures := 0 // consecutive reads that got no height
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			// A node that answered nothing at the end of the window proves nothing:
			// "the chain held" must rest on a read, not on silence.
			if failures > 0 {
				return fmt.Errorf("%s: no height at the end of the %s window (%d failed reads in a row)",
					tmRPC, window, failures)
			}
			return nil
		case <-tick.C:
			h, ok := sei.LatestHeight(ctx, hc, tmRPC)
			if !ok {
				failures++
				if failures >= maxReadFailures {
					return fmt.Errorf("%s: no height for %d reads in a row while checking that the chain held",
						tmRPC, failures)
				}
				continue
			}
			failures = 0
			if h-start > maxAdvance {
				return fmt.Errorf("%s advanced %d blocks (%d -> %d) while quorum should be lost; allowed %d",
					tmRPC, h-start, start, h, maxAdvance)
			}
		}
	}
}
