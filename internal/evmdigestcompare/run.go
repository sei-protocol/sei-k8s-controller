package evmdigestcompare

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sync"
	"time"
)

// Comparator runs digest rounds for every configured chain and records the
// results in Metrics.
type Comparator struct {
	Chains    []Chain
	NewSource func(Endpoint) (Source, error)
	Metrics   *Metrics
	// RoundInterval is the pause between a chain's rounds.
	RoundInterval time.Duration
	// HeightLag is how far below the lowest committed tip a round picks its
	// height, so every node has already committed through it.
	HeightLag int64
	Log       *slog.Logger
	Now       func() time.Time
}

// Run rounds until ctx is done. Each chain runs independently, so one
// unreachable chain never delays another.
func (c *Comparator) Run(ctx context.Context) error {
	runners := make([]*chainRunner, 0, len(c.Chains))
	for _, chain := range c.Chains {
		sources := make([]chainSource, 0, len(chain.Nodes))
		for _, node := range chain.Nodes {
			src, err := c.NewSource(node.Endpoint)
			if err != nil {
				return err
			}
			sources = append(sources, chainSource{node: node, src: src})
		}
		c.Metrics.initChain(chain, c.Now())
		runners = append(runners, &chainRunner{c: c, chain: chain, sources: sources})
	}

	var wg sync.WaitGroup
	for _, r := range runners {
		wg.Go(func() {
			ticker := time.NewTicker(c.RoundInterval)
			defer ticker.Stop()
			for {
				r.round(ctx)
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		})
	}
	wg.Wait()
	return nil
}

type chainSource struct {
	node Node
	src  Source
}

type chainRunner struct {
	c          *Comparator
	chain      Chain
	sources    []chainSource
	lastHeight int64
}

// nodeResult is one node's outcome for a round.
type nodeResult struct {
	chainSource
	report *Report
	err    error
}

func (r *chainRunner) round(ctx context.Context) {
	c := r.c
	m := c.Metrics

	// Tips first: H sits HeightLag below the lowest one, so every node
	// scanned this round has already committed through it.
	live := make([]*nodeResult, 0, len(r.sources))
	minTip := int64(math.MaxInt64)
	for _, cs := range r.sources {
		tip, err := cs.src.Tip(ctx)
		if err != nil {
			reason := ReasonUnavailable
			if errors.Is(err, ErrNoTip) {
				reason = ReasonNoTip
			}
			r.sourceError(ctx, cs.node.Label(), reason, "reading committed height", err)
			continue
		}
		m.SourceHeight.WithLabelValues(r.chain.Name, cs.node.Label()).Set(float64(tip))
		live = append(live, &nodeResult{chainSource: cs})
		if tip < minTip {
			minTip = tip
		}
	}
	if len(live) < 2 {
		if ctx.Err() == nil {
			c.Log.Warn("fewer than two nodes reachable; skipping round",
				"chain", r.chain.Name, "reachable", len(live))
		}
		return
	}
	height := minTip - c.HeightLag
	if height <= 0 || height <= r.lastHeight {
		return
	}

	// One scan per node at a time: scans run concurrently across nodes — the
	// per-sidecar serialization lives in the evm-digest task itself.
	var wg sync.WaitGroup
	for _, nr := range live {
		wg.Go(func() {
			report, err := nr.src.Digest(ctx, height, nr.node.Backend)
			nr.report, nr.err = report, err
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}

	got := make([]*nodeResult, 0, len(live))
	for _, nr := range live {
		if nr.err != nil {
			r.sourceError(ctx, nr.node.Label(), ReasonScanFailed, "evm digest scan", nr.err)
			continue
		}
		m.ScannedHeight.WithLabelValues(r.chain.Name, nr.node.Label()).Set(float64(height))
		got = append(got, nr)
	}
	if len(got) < 2 {
		c.Log.Warn("fewer than two nodes produced a report; skipping comparison",
			"chain", r.chain.Name, "height", height)
		return
	}

	r.record(height, got)
}

// record compares every node pair's report tuple for the round's height and
// exports the result.
func (r *chainRunner) record(height int64, got []*nodeResult) {
	c := r.c
	m := c.Metrics
	for i := range got {
		for j := i + 1; j < len(got); j++ {
			a, b := got[i], got[j]
			if a.report.Version == b.report.Version &&
				a.report.Final.Count == b.report.Final.Count &&
				a.report.Final.Digest == b.report.Final.Digest {
				continue
			}
			m.Diverged.WithLabelValues(r.chain.Name, a.node.Label(), b.node.Label()).Set(1)
			m.Mismatches.WithLabelValues(r.chain.Name, a.node.Label(), b.node.Label()).Inc()
			c.Log.Error("evm digest mismatch",
				"chain", r.chain.Name, "height", height,
				"node_a", a.node.Label(), "node_b", b.node.Label(),
				"a_version", a.report.Version, "b_version", b.report.Version,
				"a_count", a.report.Final.Count, "b_count", b.report.Final.Count,
				"a_digest", a.report.Final.Digest, "b_digest", b.report.Final.Digest)
		}
	}
	r.lastHeight = height
	m.ComparedHeights.WithLabelValues(r.chain.Name).Inc()
	m.LastComparedHeight.WithLabelValues(r.chain.Name).Set(float64(height))
	m.LastComparedTimestamp.WithLabelValues(r.chain.Name).Set(float64(c.Now().Unix()))
	c.Log.Info("evm digest round compared",
		"chain", r.chain.Name, "height", height, "nodes", len(got))
}

func (r *chainRunner) sourceError(ctx context.Context, node, reason, what string, err error) {
	if ctx.Err() != nil {
		return
	}
	r.c.Metrics.SourceErrors.WithLabelValues(r.chain.Name, node, reason).Inc()
	r.c.Log.Warn(what+" failed", "chain", r.chain.Name, "node", node, "reason", reason, "error", err)
}
