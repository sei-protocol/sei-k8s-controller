package hashlogcompare

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	sidecar "github.com/sei-protocol/sei-k8s-controller/sidecarapi/client"
)

// Comparator polls every configured pair and records the results in Metrics.
type Comparator struct {
	Pairs        []Pair
	NewSource    func(Endpoint) (Source, error)
	Metrics      *Metrics
	PollInterval time.Duration
	Log          *slog.Logger
	Now          func() time.Time
}

// Run polls until ctx is done. Each pair runs independently, so one
// unreachable node never delays another pair.
func (c *Comparator) Run(ctx context.Context) error {
	pairs := make([]*pairRunner, 0, len(c.Pairs))
	for _, p := range c.Pairs {
		migrating, err := c.NewSource(p.Migrating)
		if err != nil {
			return err
		}
		reserve, err := c.NewSource(p.Reserve)
		if err != nil {
			return err
		}
		pr := &pairRunner{
			c:         c,
			labels:    []string{p.Chain, p.Migrating.Label(), p.Reserve.Label()},
			migrating: NewReader(migrating),
			reserve:   NewReader(reserve),
			state:     newPairState(),
		}
		c.Metrics.initPair(pr.labels[0], pr.labels[1], pr.labels[2])
		pairs = append(pairs, pr)
	}

	var wg sync.WaitGroup
	for _, pr := range pairs {
		wg.Go(func() {
			ticker := time.NewTicker(c.PollInterval)
			defer ticker.Stop()
			for {
				pr.poll(ctx)
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

type pairRunner struct {
	c                  *Comparator
	labels             []string
	migrating, reserve *Reader
	state              *pairState
}

func (pr *pairRunner) poll(ctx context.Context) {
	pr.read(ctx, RoleMigrating, pr.migrating, pr.state.migrating)
	pr.read(ctx, RoleReserve, pr.reserve, pr.state.reserve)
	pr.record(pr.state.compare())
}

func (pr *pairRunner) read(ctx context.Context, role string, r *Reader, buf map[int64]map[string]string) {
	m := pr.c.Metrics
	roleLabels := append(pr.labels[:len(pr.labels):len(pr.labels)], role)
	rows, invalid, err := r.Poll(ctx)
	if err != nil && ctx.Err() == nil {
		reason := ReasonUnavailable
		switch {
		case errors.Is(err, sidecar.ErrHashLogNotFound):
			reason = ReasonNotFound
		case errors.Is(err, errCoverageGap):
			reason = ReasonCoverageGap
		}
		m.SourceErrors.WithLabelValues(append(roleLabels, reason)...).Inc()
		pr.c.Log.Warn("hash log read failed", pr.logAttrs(role, "reason", reason, "error", err)...)
	}
	for _, row := range rows {
		comparable, cerr := ComparableHashes(row.Hashes)
		if cerr != nil {
			invalid++
			continue
		}
		pr.state.add(buf, row.Height, comparable)
		m.SourceHeight.WithLabelValues(roleLabels...).Set(float64(row.Height))
	}
	if invalid > 0 {
		m.SourceErrors.WithLabelValues(append(roleLabels, ReasonInvalidRow)...).Add(float64(invalid))
		pr.c.Log.Warn("unusable hash log rows", pr.logAttrs(role, "rows", invalid)...)
	}
}

func (pr *pairRunner) record(res CompareResult) {
	m := pr.c.Metrics
	for _, mm := range res.Mismatches {
		m.Diverged.WithLabelValues(pr.labels...).Set(1)
		m.Mismatches.WithLabelValues(pr.labels...).Inc()
		for _, col := range mm.Columns {
			m.MismatchedColumns.WithLabelValues(append(pr.labels[:len(pr.labels):len(pr.labels)], col)...).Inc()
		}
		pr.c.Log.Error("hash log mismatch", pr.logAttrs("", "height", mm.Height, "columns", mm.Columns)...)
	}
	if res.MigratingGaps > 0 {
		m.HeightGaps.WithLabelValues(append(pr.labels[:len(pr.labels):len(pr.labels)], RoleMigrating)...).
			Add(float64(res.MigratingGaps))
	}
	if res.ReserveGaps > 0 {
		m.HeightGaps.WithLabelValues(append(pr.labels[:len(pr.labels):len(pr.labels)], RoleReserve)...).
			Add(float64(res.ReserveGaps))
	}
	if res.Compared > 0 {
		m.ComparedHeights.WithLabelValues(pr.labels...).Add(float64(res.Compared))
		m.LastComparedHeight.WithLabelValues(pr.labels...).Set(float64(pr.state.lastCompared))
		m.LastComparedTimestamp.WithLabelValues(pr.labels...).Set(float64(pr.c.Now().Unix()))
	}
}

func (pr *pairRunner) logAttrs(role string, kv ...any) []any {
	attrs := []any{labelChain, pr.labels[0], labelMigrating, pr.labels[1], labelReserve, pr.labels[2]}
	if role != "" {
		attrs = append(attrs, labelRole, role)
	}
	return append(attrs, kv...)
}
