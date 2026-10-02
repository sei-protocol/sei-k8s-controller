package evmdigestcompare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	sidecar "github.com/sei-protocol/sei-k8s-controller/sidecarapi/client"
)

// Source is one node's sidecar API, narrowed to what the comparator needs.
type Source interface {
	// Tip returns the node's latest committed height.
	Tip(ctx context.Context) (int64, error)
	// Digest runs `seidb evm-logical-digest` on the node at height and returns
	// the report the scan produced.
	Digest(ctx context.Context, height int64, backend string) (*Report, error)
}

// Report is the subset of the seidb evm-logical-digest report the comparison
// uses: a round passes when Version, Final.Count, and Final.Digest are equal
// on every node.
type Report struct {
	Version int64 `json:"version"`
	Final   struct {
		Count  uint64 `json:"count"`
		Digest string `json:"digest"`
	} `json:"final"`
}

// ErrNoTip marks a sidecar that answers but reports no committed height —
// distinct from a transport failure, which has its own alert reason.
var ErrNoTip = errors.New("sidecar reports no committed height")

// SidecarSource adapts a sidecar client to Source: Digest submits an
// evm-digest task and polls it to completion.
type SidecarSource struct {
	Client *sidecar.SidecarClient
	// TaskPoll is how often a running evm-digest task is polled.
	TaskPoll time.Duration
	// ScanTimeout bounds one evm-digest task attempt.
	ScanTimeout time.Duration
	// Attempts is the total number of submissions per Digest call; a scan can
	// fail transiently when the changelog ends mid-record while the node
	// writes it.
	Attempts int
}

// Tip implements Source.
func (s *SidecarSource) Tip(ctx context.Context) (int64, error) {
	status, err := s.Client.Status(ctx)
	if err != nil {
		return 0, err
	}
	if status.CommittedHeight == nil {
		return 0, ErrNoTip
	}
	return *status.CommittedHeight, nil
}

// Digest implements Source.
func (s *SidecarSource) Digest(ctx context.Context, height int64, backend string) (*Report, error) {
	attempts := max(s.Attempts, 1)
	var lastErr error
	for range attempts {
		report, err := s.digestOnce(ctx, height, backend)
		if err == nil {
			return report, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		pause := s.TaskPoll
		if pause <= 0 {
			pause = 10 * time.Second
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pause):
		}
	}
	return nil, lastErr
}

func (s *SidecarSource) digestOnce(ctx context.Context, height int64, backend string) (*Report, error) {
	task := sidecar.EVMDigestTask{Height: height, Backend: backend}
	id, err := s.Client.SubmitTask(ctx, task.ToTaskRequest())
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.ScanTimeout)
	defer cancel()
	// The task's lifecycle outlives this call's ctx (a scan-side timeout or a
	// comparator restart leaves it running on the sidecar), so clean it up on
	// an uncancelable parent once the result is in hand — or lost.
	defer func() { _ = s.Client.DeleteTask(context.WithoutCancel(ctx), id) }()

	poll := s.TaskPoll
	if poll <= 0 {
		poll = 10 * time.Second
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("evm-digest task %s: %w", id, ctx.Err())
		case <-ticker.C:
		}
		tr, err := s.Client.GetTask(ctx, id)
		if err != nil {
			if errors.Is(err, sidecar.ErrNotFound) {
				return nil, fmt.Errorf("evm-digest task %s vanished mid-scan", id)
			}
			continue
		}
		switch tr.Status {
		case sidecar.Completed:
			if tr.Result == nil {
				return nil, errors.New("evm-digest task completed with no result")
			}
			var report Report
			if err := json.Unmarshal(*tr.Result, &report); err != nil {
				return nil, fmt.Errorf("evm-digest task result is not a report: %w", err)
			}
			return &report, nil
		case sidecar.Failed:
			msg := "no error detail"
			if tr.Error != nil {
				msg = *tr.Error
			}
			return nil, fmt.Errorf("evm-digest task failed: %s", msg)
		}
	}
}
