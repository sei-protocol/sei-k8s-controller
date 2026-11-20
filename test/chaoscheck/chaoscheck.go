// Package chaoscheck holds the post-fault assertions the chaos suites share
// and that need no cluster: they speak only to Tendermint RPC endpoints, so
// they unit-test against an httptest server.
//
// The nightly suite's recovery gate asks whether the chain is live and every
// validator pod is Ready. That does not notice validators that came back
// healthy but committed different state. WaitAgreeAbove is that check.
//
// It compares each node's own committed store hash, read from /abci_info
// (ResponseInfo.last_block_app_hash, which the app fills from its multistore's
// LastCommitID). That is the locally computed value, so a node whose store
// came back wrong reports a different hash. The block header's app_hash,
// served by /commit, is copied from the network and cannot differ on a node
// that is accepting blocks: a node with a bad store rejects the next block and
// stalls instead. /abci_info is also served under Autobahn, where the CometBFT
// block store behind /commit is empty.
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
	"sync"
	"time"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

// pollInterval is how often HoldsHeight samples; a var so tests can shrink it.
var pollInterval = 2 * time.Second

// sampleInterval is how often WaitAgreeAbove reads every node. It is well under
// a block time so that nodes' samples overlap on common heights; a var so tests
// can shrink it.
var sampleInterval = 250 * time.Millisecond

// maxReadFailures is how many consecutive failed height reads HoldsHeight
// tolerates before it gives up, so one dropped request does not fail a
// scenario but a node that has gone silent does.
const maxReadFailures = 3

// DivergenceError reports nodes that committed different state at the same
// height. It is never retried: a hash that differs now will not converge.
type DivergenceError struct {
	Height int64
	// Hashes maps each RPC endpoint to the committed hash it reported.
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
	return fmt.Sprintf("committed state diverged at height %d: %s", e.Height, strings.Join(parts, " "))
}

// Sample is one node's last committed block: its height and the hash of the
// state that block left in the node's own store.
type Sample struct {
	Height int64
	Hash   string
}

// infoResponse models the part of /abci_info this package reads. The Sei fork
// sometimes returns the body without the JSON-RPC envelope (the same quirk
// sdk/sei accepts for /status), so both shapes are decoded. The height is a
// json.Number because it arrives as a quoted string or a bare number; the hash
// is base64, compared as the string it arrives as.
type infoResponse struct {
	Result *infoBody `json:"result,omitempty"`
	infoBody
}

type infoBody struct {
	Response struct {
		LastBlockHeight  json.Number `json:"last_block_height"`
		LastBlockAppHash string      `json:"last_block_app_hash"`
	} `json:"response"`
}

func (r *infoResponse) body() infoBody {
	if r.Result != nil && r.Result.Response.LastBlockAppHash != "" {
		return *r.Result
	}
	return r.infoBody
}

// Info reads the node's last committed height and state hash from /abci_info. A
// node that has committed nothing (height 0 or no hash) is an error: an empty
// hash would otherwise compare equal across nodes and hide a failure.
func Info(ctx context.Context, hc *http.Client, tmRPC string) (Sample, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	url := strings.TrimRight(tmRPC, "/") + "/abci_info"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Sample{}, fmt.Errorf("build request %s: %w", url, err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Sample{}, fmt.Errorf("get %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Sample{}, fmt.Errorf("get %s: status %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Sample{}, fmt.Errorf("read %s: %w", url, err)
	}
	var r infoResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return Sample{}, fmt.Errorf("decode %s: %w", url, err)
	}
	b := r.body()
	// last_block_height is omitted at height 0, which arrives as an empty number.
	var h int64
	if b.Response.LastBlockHeight != "" {
		var err error
		if h, err = b.Response.LastBlockHeight.Int64(); err != nil {
			return Sample{}, fmt.Errorf("%s: last_block_height %q: %w", url, b.Response.LastBlockHeight, err)
		}
	}
	if h <= 0 || b.Response.LastBlockAppHash == "" {
		return Sample{}, fmt.Errorf("%s has committed nothing yet (height %d, hash %q)",
			url, h, b.Response.LastBlockAppHash)
	}
	return Sample{Height: h, Hash: b.Response.LastBlockAppHash}, nil
}

// SyncInfo is a node's sync state from /status.
type SyncInfo struct {
	Height     int64
	CatchingUp bool
}

// statusResponse models the part of /status Sync reads, in the enveloped and
// unwrapped shapes the Sei fork emits.
type statusResponse struct {
	Result *statusBody `json:"result,omitempty"`
	statusBody
}

type statusBody struct {
	SyncInfo struct {
		LatestBlockHeight json.Number `json:"latest_block_height"`
		CatchingUp        bool        `json:"catching_up"`
	} `json:"sync_info"`
}

func (r *statusResponse) body() statusBody {
	if r.Result != nil && r.Result.SyncInfo.LatestBlockHeight != "" {
		return *r.Result
	}
	return r.statusBody
}

// Sync reads the node's height and whether it reports itself as catching up, from
// /status. Under CometBFT consensus catching_up is true while the node is block
// syncing or state syncing, which is what a scenario that must land a restart in
// one of those phases watches for. Under Autobahn catching_up is not meaningful.
// An unreachable node, or a body with no height, is an error: the RPC listener
// is not up yet while a node replays on startup, so callers poll through errors.
func Sync(ctx context.Context, hc *http.Client, tmRPC string) (SyncInfo, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	url := strings.TrimRight(tmRPC, "/") + "/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return SyncInfo{}, fmt.Errorf("build request %s: %w", url, err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return SyncInfo{}, fmt.Errorf("get %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return SyncInfo{}, fmt.Errorf("get %s: status %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return SyncInfo{}, fmt.Errorf("read %s: %w", url, err)
	}
	var r statusResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return SyncInfo{}, fmt.Errorf("decode %s: %w", url, err)
	}
	b := r.body()
	if b.SyncInfo.LatestBlockHeight == "" {
		return SyncInfo{}, fmt.Errorf("%s returned no latest_block_height", url)
	}
	h, err := b.SyncInfo.LatestBlockHeight.Int64()
	if err != nil {
		return SyncInfo{}, fmt.Errorf("%s: latest_block_height %q: %w", url, b.SyncInfo.LatestBlockHeight, err)
	}
	return SyncInfo{Height: h, CatchingUp: b.SyncInfo.CatchingUp}, nil
}

// Floor returns the highest committed height any of the nodes reports right
// now. Capture it just before injecting a fault, then pass it to
// WaitAgreeAbove so the comparison happens on state committed after the fault.
// It is a watermark, so unreachable nodes are skipped; it fails only if none
// answers.
func Floor(ctx context.Context, hc *http.Client, tmRPCs []string) (int64, error) {
	var floor int64
	var errs []error
	for _, u := range tmRPCs {
		s, err := Info(ctx, hc, u)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		floor = max(floor, s.Height)
	}
	if floor == 0 {
		return 0, fmt.Errorf("no node reported a committed height: %w", errors.Join(errs...))
	}
	return floor, nil
}

// WaitAgreeAbove waits until every node has committed past floor and the nodes
// have agreed on the state hash of at least wantCommon distinct heights that all
// of them reported. It samples every node continuously, because the chain keeps
// moving: two nodes' tips differ by a block or so, so agreement is read off the
// heights their samples share, not off one instant.
//
// Requiring every node to report a height above floor is what makes this a
// recovery check. A validator that restarted and is still replaying behind the
// fault never reports such a height, so the call fails instead of comparing
// everyone at a height from before the fault, where they trivially agree.
//
// Reads that fail are retried until ctx ends. A node that reports two different
// hashes for one height, or two nodes that differ at a shared height, return a
// *DivergenceError at once.
//
// Every endpoint must be ONE node: a node reporting two hashes for one height is
// read as divergence and never retried, so an endpoint fronted by a Service over
// several pods would false-diverge. The harness reads each validator's and the
// follower's own endpoint.
//
// The nodes are read concurrently each tick. Read one after another, a slow node
// (a replaying one is exactly that) would stretch the gap between the first and
// last read past a sub-second block time, and the samples would never share a
// height.
func WaitAgreeAbove(ctx context.Context, hc *http.Client, tmRPCs []string, floor int64, wantCommon int) error {
	if len(tmRPCs) == 0 {
		return errors.New("no RPC endpoints")
	}
	seen := make(map[string]map[int64]string, len(tmRPCs)) // endpoint -> height -> hash
	for _, u := range tmRPCs {
		seen[u] = map[int64]string{}
	}
	agreed := map[int64]struct{}{}
	var lastErr error

	tick := time.NewTicker(sampleInterval)
	defer tick.Stop()
	for {
		samples, errs := readAll(ctx, hc, tmRPCs)
		for i, u := range tmRPCs {
			s, err := samples[i], errs[i]
			if err != nil {
				lastErr = err
				continue
			}
			if s.Height <= floor {
				continue
			}
			if prev, ok := seen[u][s.Height]; ok && prev != s.Hash {
				return &DivergenceError{Height: s.Height, Hashes: map[string]string{
					u + " (first read)":  prev,
					u + " (second read)": s.Hash,
				}}
			}
			seen[u][s.Height] = s.Hash
		}
		if err := compareShared(tmRPCs, seen, agreed); err != nil {
			return err
		}
		if len(agreed) >= wantCommon {
			return nil
		}
		select {
		case <-ctx.Done():
			// ctx.Err() says whether this was the deadline or a cancellation (a SIGTERM
			// unwinding the run), so it is wrapped rather than described.
			return fmt.Errorf("fewer than %d common heights above %d agreed: %w; %s (last read error: %v)",
				wantCommon, floor, ctx.Err(), progress(tmRPCs, seen), lastErr)
		case <-tick.C:
		}
	}
}

// readAll reads every endpoint's /abci_info concurrently and returns the results
// in endpoint order. The goroutines write only to their own slots.
func readAll(ctx context.Context, hc *http.Client, tmRPCs []string) ([]Sample, []error) {
	samples := make([]Sample, len(tmRPCs))
	errs := make([]error, len(tmRPCs))
	var wg sync.WaitGroup
	for i, u := range tmRPCs {
		wg.Go(func() {
			samples[i], errs[i] = Info(ctx, hc, u)
		})
	}
	wg.Wait()
	return samples, errs
}

// compareShared compares the hashes of every height all nodes have reported,
// recording agreed heights in agreed and returning a *DivergenceError on the
// first mismatch.
func compareShared(tmRPCs []string, seen map[string]map[int64]string, agreed map[int64]struct{}) error {
	for h, first := range seen[tmRPCs[0]] {
		if _, done := agreed[h]; done {
			continue
		}
		hashes := map[string]string{tmRPCs[0]: first}
		shared := true
		for _, u := range tmRPCs[1:] {
			hash, ok := seen[u][h]
			if !ok {
				shared = false
				break
			}
			hashes[u] = hash
		}
		if !shared {
			continue
		}
		for _, hash := range hashes {
			if hash != first {
				return &DivergenceError{Height: h, Hashes: hashes}
			}
		}
		agreed[h] = struct{}{}
	}
	return nil
}

// progress names the nodes that have not reported a height above the floor, so
// a timeout says which node was behind.
func progress(tmRPCs []string, seen map[string]map[int64]string) string {
	var behind []string
	for _, u := range tmRPCs {
		if len(seen[u]) == 0 {
			behind = append(behind, u)
		}
	}
	if len(behind) == 0 {
		return "every node reported heights above the floor but none were shared"
	}
	return fmt.Sprintf("no height above the floor from: %s", strings.Join(behind, ", "))
}

// HoldsHeight asserts that the node at tmRPC does not commit blocks: after
// settle (in-flight blocks finish), its height advances by at most maxAdvance
// over window. It is the stall check for a fault that removes quorum. A node
// that cannot report a height is an error, since an unreachable node proves
// nothing about the chain.
func HoldsHeight(
	ctx context.Context, hc *http.Client, tmRPC string, settle, window time.Duration, maxAdvance int64,
) error {
	select {
	case <-ctx.Done():
		return fmt.Errorf("%s: %w", tmRPC, ctx.Err())
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
			return fmt.Errorf("%s: %w", tmRPC, ctx.Err())
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
