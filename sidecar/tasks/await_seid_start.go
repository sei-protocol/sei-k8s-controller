package tasks

import (
	"context"
	"time"

	"github.com/sei-protocol/seilog"

	"github.com/sei-protocol/sei-k8s-controller/sidecar/engine"
)

var awaitSeidStartLog = seilog.NewLogger("seictl", "task", "await-seid-start")

// awaitSeidStartPollInterval is how often the task looks for `seid start`.
// The start gate's wait loop polls healthz every 5s, so seid starts within
// about 5s of mark-ready; a short poll here closes the gate again soon after.
const awaitSeidStartPollInterval = 250 * time.Millisecond

// SeidStartAwaiter completes once a `seid start` process runs in the pod. The
// maintenance hold's start-once step runs mark-ready, this task, then
// mark-not-ready: seid starts once, and the gate is closed again before seid
// has loaded its state, so seid parks at its next exit. It only reads /proc.
type SeidStartAwaiter struct {
	find         func() bool
	pollInterval time.Duration
}

// NewSeidStartAwaiter builds a SeidStartAwaiter over the pod's /proc.
func NewSeidStartAwaiter() *SeidStartAwaiter {
	return &SeidStartAwaiter{
		find: func() bool {
			_, err := seidStartFinder{}.FindPID(restartSeidProcess)
			return err == nil
		},
		pollInterval: awaitSeidStartPollInterval,
	}
}

// Handler returns an engine.TaskHandler for the await-seid-start task type.
// Params are empty. It returns when seid runs, or with the context's error.
func (a *SeidStartAwaiter) Handler() engine.TaskHandler {
	return engine.TypedHandler(func(ctx context.Context, _ struct{}) error {
		ticker := time.NewTicker(a.pollInterval)
		defer ticker.Stop()
		for {
			if a.find() {
				awaitSeidStartLog.Info("seid start is running")
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
	})
}
