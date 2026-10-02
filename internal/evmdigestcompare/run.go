package evmdigestcompare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	sidecar "github.com/sei-protocol/sei-k8s-controller/sidecarapi/client"
	"github.com/sei-protocol/sei-k8s-controller/sidecarapi/wire"
)

// taskIDNamespace seeds the deterministic evm-logical-digest task IDs, so a
// restarted comparator resubmitting the same scan gets the existing task back.
var taskIDNamespace = uuid.MustParse("9b0c5d2e-4f1a-4c39-8e57-2d6b1f0a7c43")

// cancelTimeout bounds the request that cancels a timed-out scan, which runs
// after the scan's own context has expired.
const cancelTimeout = 30 * time.Second

// Source is the subset of the sidecar client a round uses.
type Source interface {
	Status(ctx context.Context) (*sidecar.StatusResponse, error)
	SubmitTask(ctx context.Context, task sidecar.TaskRequest) (uuid.UUID, error)
	GetTask(ctx context.Context, id uuid.UUID) (*sidecar.TaskResult, error)
	ListTasks(ctx context.Context) ([]sidecar.TaskResult, error)
	DeleteTask(ctx context.Context, id uuid.UUID) error
}

// Comparator runs digest rounds for every configured group and records the
// results in Metrics.
type Comparator struct {
	Groups    []Group
	NewSource func(Node) (Source, error)
	Metrics   *Metrics
	// HeightLag is how far below the group's lowest committed tip a round scans.
	HeightLag int64
	// RoundInterval is the minimum time between the starts of two rounds of a group.
	RoundInterval time.Duration
	// TaskPollInterval is how often a running scan's task is polled.
	TaskPollInterval time.Duration
	// ScanTimeout bounds one node's scan; a scan past it is cancelled.
	ScanTimeout time.Duration
	Log         *slog.Logger
	Now         func() time.Time
}

// Run compares until ctx is done. Groups run independently; within a group a
// round finishes on every node before the next starts.
func (c *Comparator) Run(ctx context.Context) error {
	groups := make([]*groupRunner, 0, len(c.Groups))
	for _, g := range c.Groups {
		gr := &groupRunner{c: c, chain: g.Chain}
		labels := make([]string, 0, len(g.Nodes))
		for _, n := range g.Nodes {
			src, err := c.NewSource(n)
			if err != nil {
				return err
			}
			gr.nodes = append(gr.nodes, &nodeRunner{node: n, label: n.Label(), src: src})
			labels = append(labels, n.Label())
		}
		c.Metrics.initGroup(g.Chain, labels, c.Now())
		groups = append(groups, gr)
	}

	var wg sync.WaitGroup
	for _, gr := range groups {
		wg.Go(func() {
			for {
				next := time.NewTimer(c.RoundInterval)
				gr.round(ctx)
				select {
				case <-ctx.Done():
					next.Stop()
					return
				case <-next.C:
				}
			}
		})
	}
	wg.Wait()
	return nil
}

type nodeRunner struct {
	node  Node
	label string
	src   Source
}

type groupRunner struct {
	c          *Comparator
	chain      string
	nodes      []*nodeRunner
	lastHeight int64
}

// round scans every node at one height and compares the digests.
func (g *groupRunner) round(ctx context.Context) {
	h, ok := g.targetHeight(ctx)
	if !ok {
		return
	}
	m := g.c.Metrics
	m.TargetHeight.WithLabelValues(g.chain).Set(float64(h))

	results := make([]*wire.EVMLogicalDigestResult, len(g.nodes))
	var wg sync.WaitGroup
	for i, n := range g.nodes {
		wg.Go(func() {
			res, reason, err := g.scan(ctx, n, h)
			if err != nil {
				if ctx.Err() == nil {
					m.ScanErrors.WithLabelValues(g.chain, n.label, reason).Inc()
					g.c.Log.Warn("scan produced no digest", "chain", g.chain, "node", n.label,
						"height", h, "reason", reason, "error", err)
				}
				return
			}
			m.ScanDuration.WithLabelValues(g.chain, n.label).Set(res.DurationSeconds)
			m.ScanEntries.WithLabelValues(g.chain, n.label).Set(float64(res.Final.Count))
			results[i] = res
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}
	for _, r := range results {
		if r == nil {
			m.Rounds.WithLabelValues(g.chain, ResultIncomplete).Inc()
			return
		}
	}

	g.lastHeight = h
	m.LastComparedHeight.WithLabelValues(g.chain).Set(float64(h))
	m.LastComparedTimestamp.WithLabelValues(g.chain).Set(float64(g.c.Now().Unix()))

	if digestsAgree(results) {
		m.Rounds.WithLabelValues(g.chain, ResultMatch).Inc()
		g.c.Log.Info("evm digests match", "chain", g.chain, "height", h,
			"version", results[0].Version, "count", results[0].Final.Count, "digest", results[0].Final.Digest)
		return
	}
	m.Rounds.WithLabelValues(g.chain, ResultMismatch).Inc()
	m.Mismatches.WithLabelValues(g.chain).Inc()
	m.Diverged.WithLabelValues(g.chain).Set(1)
	for i, r := range results {
		g.c.Log.Error("evm digest mismatch", "chain", g.chain, "height", h, "node", g.nodes[i].label,
			"backend", r.Backend, "version", r.Version, "count", r.Final.Count, "digest", r.Final.Digest,
			"account", r.Account, "code", r.Code, "storage", r.Storage, "misc", r.Misc)
	}
}

// digestsAgree reports whether every result has the same version, final count
// and final digest.
func digestsAgree(results []*wire.EVMLogicalDigestResult) bool {
	first := results[0]
	for _, r := range results[1:] {
		if r.Version != first.Version || r.Final != first.Final {
			return false
		}
	}
	return true
}

// targetHeight reads every node's committed height and returns the round's
// height: HeightLag below the lowest. It reports false when a node is
// unreadable or the height has not moved past the last compared one.
func (g *groupRunner) targetHeight(ctx context.Context) (int64, bool) {
	m := g.c.Metrics
	heights := make([]int64, len(g.nodes))
	errs := make([]error, len(g.nodes))
	var wg sync.WaitGroup
	for i, n := range g.nodes {
		wg.Go(func() {
			st, err := n.src.Status(ctx)
			switch {
			case err != nil:
				errs[i] = err
			case st.CommittedHeight == nil:
				errs[i] = errors.New("sidecar reports no committed height")
			default:
				heights[i] = *st.CommittedHeight
				m.SourceHeight.WithLabelValues(g.chain, n.label).Set(float64(heights[i]))
			}
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return 0, false
	}

	lowest := int64(0)
	for i, n := range g.nodes {
		if errs[i] != nil {
			m.ScanErrors.WithLabelValues(g.chain, n.label, ReasonUnavailable).Inc()
			g.c.Log.Warn("reading committed height", "chain", g.chain, "node", n.label, "error", errs[i])
			m.Rounds.WithLabelValues(g.chain, ResultIncomplete).Inc()
			return 0, false
		}
		if i == 0 || heights[i] < lowest {
			lowest = heights[i]
		}
	}
	h := lowest - g.c.HeightLag
	if h <= 0 || h <= g.lastHeight {
		g.c.Log.Info("no new height to compare", "chain", g.chain, "lowest_tip", lowest, "last_compared", g.lastHeight)
		return 0, false
	}
	return h, true
}

// scan runs one node's evm-logical-digest task at h to completion. On error it
// also returns the metric reason.
func (g *groupRunner) scan(ctx context.Context, n *nodeRunner, h int64) (*wire.EVMLogicalDigestResult, string, error) {
	id := taskID(g.chain, n.label, h)
	g.cancelOtherScans(ctx, n, id)

	task := sidecar.EVMLogicalDigestTask{Backend: n.node.Backend, Height: h}
	if err := task.Validate(); err != nil {
		return nil, ReasonFailed, err
	}
	req := task.ToTaskRequest()
	req.Id = &id
	if _, err := n.src.SubmitTask(ctx, req); err != nil {
		return nil, ReasonUnavailable, err
	}

	scanCtx, cancel := context.WithTimeout(ctx, g.c.ScanTimeout)
	defer cancel()
	for {
		tr, err := n.src.GetTask(scanCtx, id)
		switch {
		case errors.Is(err, sidecar.ErrNotFound):
			return nil, ReasonFailed, fmt.Errorf("task %s disappeared from the sidecar", id)
		case err != nil:
			// Transient: keep polling until the scan times out.
			g.c.Log.Debug("polling scan", "chain", g.chain, "node", n.label, "task", id, "error", err)
		case tr.Status == sidecar.Completed:
			return decodeResult(tr, h)
		case tr.Status == sidecar.Failed:
			msg := "no error message"
			if tr.Error != nil {
				msg = *tr.Error
			}
			return nil, ReasonFailed, errors.New(msg)
		}

		select {
		case <-scanCtx.Done():
			if ctx.Err() != nil {
				return nil, ReasonTimeout, ctx.Err()
			}
			g.cancelScan(ctx, n, id)
			return nil, ReasonTimeout, fmt.Errorf("scan did not finish within %s", g.c.ScanTimeout)
		case <-time.After(g.c.TaskPollInterval):
		}
	}
}

func decodeResult(tr *sidecar.TaskResult, h int64) (*wire.EVMLogicalDigestResult, string, error) {
	if tr.Result == nil {
		return nil, ReasonInvalidResult, errors.New("completed task has no result")
	}
	var res wire.EVMLogicalDigestResult
	if err := json.Unmarshal(*tr.Result, &res); err != nil {
		return nil, ReasonInvalidResult, fmt.Errorf("decoding task result: %w", err)
	}
	if res.RequestedHeight != h || res.Final.Digest == "" {
		return nil, ReasonInvalidResult, fmt.Errorf("task result is for height %d with digest %q, want height %d",
			res.RequestedHeight, res.Final.Digest, h)
	}
	return &res, "", nil
}

// cancelOtherScans cancels digest scans still running on the node under other
// IDs, typically left by a previous comparator process, so the node only ever
// works on this round's scan.
func (g *groupRunner) cancelOtherScans(ctx context.Context, n *nodeRunner, keep uuid.UUID) {
	tasks, err := n.src.ListTasks(ctx)
	if err != nil {
		g.c.Log.Warn("listing sidecar tasks", "chain", g.chain, "node", n.label, "error", err)
		return
	}
	for _, t := range tasks {
		if t.Type == sidecar.TaskTypeEVMLogicalDigest && t.Status == sidecar.Running && t.Id != keep {
			g.cancelScan(ctx, n, t.Id)
		}
	}
}

func (g *groupRunner) cancelScan(ctx context.Context, n *nodeRunner, id uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cancelTimeout)
	defer cancel()
	if err := n.src.DeleteTask(ctx, id); err != nil {
		g.c.Log.Warn("cancelling scan", "chain", g.chain, "node", n.label, "task", id, "error", err)
		return
	}
	g.c.Log.Info("cancelled scan", "chain", g.chain, "node", n.label, "task", id)
}

func taskID(chain, node string, h int64) uuid.UUID {
	return uuid.NewSHA1(taskIDNamespace, fmt.Appendf(nil, "evm-logical-digest/%s/%s/%d", chain, node, h))
}
