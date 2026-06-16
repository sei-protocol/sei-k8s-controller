// Package envtest holds test infrastructure shared by the envtest-tagged
// integration tests in this directory. The types defined here are plain
// Go (no build tag) so they can be imported by other test suites without
// inheriting the envtest tag — the helpers are reusable, the test runner
// is what's tag-gated.
package envtest

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
)

// StubSidecarClient satisfies internal/task.SidecarClient (the narrow,
// task-execution-facing interface) with predictable, test-friendly
// behaviour. Healthz returns ready, SubmitTask accepts and registers
// the task, GetTask reports Complete after CompleteAfter has elapsed
// since submission.
//
// Behaviour is deterministic, not random: tests poll on a wall clock,
// so a stub that completes "eventually" (rather than instantly) gives
// the controller's polling loop time to execute a couple of laps —
// which is what we want exercised. Zero CompleteAfter means immediate
// complete-on-first-GetTask, which is still fine for the InPlace path
// because the executor advances synchronous tasks in a loop.
//
// Tests construct one of these and inject it via
// task.ExecutionConfig.BuildSidecarClient. The factory closure should
// return the same *StubSidecarClient instance every call so submitted
// task state is observable across the reconcile loop (and across the
// SeiNode and SND reconcilers if both share the stub).
type StubSidecarClient struct {
	// CompleteAfter is the delay between SubmitTask and when GetTask
	// reports Completed. Zero means complete on first GetTask call.
	CompleteAfter time.Duration

	mu    sync.Mutex
	tasks map[uuid.UUID]*stubTaskRecord
}

type stubTaskRecord struct {
	submittedAt time.Time
	taskType    string
}

// NewStubSidecarClient returns a stub configured for typical envtest use.
// CompleteAfter=0 trades realism for speed: every InPlace test currently
// asserts on terminal state, not on Running observability mid-task.
func NewStubSidecarClient() *StubSidecarClient {
	return &StubSidecarClient{
		tasks: make(map[uuid.UUID]*stubTaskRecord),
	}
}

// SubmitTask records the task and returns its caller-provided ID (or a
// fresh UUID if none was supplied). The sidecar's real engine treats a
// caller-set ID as idempotent — re-submission returns the existing ID;
// this stub matches that contract.
func (s *StubSidecarClient) SubmitTask(_ context.Context, req sidecar.TaskRequest) (uuid.UUID, error) {
	var id uuid.UUID
	if req.Id != nil {
		id = *req.Id
	} else {
		id = uuid.New()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.tasks[id]; !exists {
		s.tasks[id] = &stubTaskRecord{
			submittedAt: time.Now(),
			taskType:    req.Type,
		}
	}
	return id, nil
}

// GetTask reports Completed once CompleteAfter has elapsed since
// submission, Running before that, and returns sidecar.ErrNotFound for
// IDs the stub never saw (matching the real client's contract — the
// sidecarExecution.Status() handler treats ErrNotFound as "keep
// polling").
func (s *StubSidecarClient) GetTask(_ context.Context, id uuid.UUID) (*sidecar.TaskResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.tasks[id]
	if !ok {
		return nil, sidecar.ErrNotFound
	}

	status := sidecar.Running
	var completedAt *time.Time
	if time.Since(rec.submittedAt) >= s.CompleteAfter {
		status = sidecar.Completed
		t := rec.submittedAt.Add(s.CompleteAfter)
		completedAt = &t
	}

	return &sidecar.TaskResult{
		Id:          id,
		Type:        rec.taskType,
		Status:      status,
		SubmittedAt: rec.submittedAt,
		CompletedAt: completedAt,
	}, nil
}

// Healthz always reports ready. The SeiNode reconciler probes this when
// the node is in PhaseRunning; a "false" answer would leave a
// SidecarReady=False condition that the planner reacts to by building
// a mark-ready re-approval plan. The InPlace test does not exercise
// that path, so returning ready is the simpler, more deterministic
// behaviour.
func (s *StubSidecarClient) Healthz(_ context.Context) (bool, error) {
	return true, nil
}

// GetNodeID returns a deterministic stub identity (non-empty is all
// callers check for in envtest).
func (s *StubSidecarClient) GetNodeID(_ context.Context) (string, error) {
	return "stub-node-id", nil
}

// SubmittedCount is a debugging hook for tests — reports how many
// distinct tasks the stub has accepted. Not used by the production
// reconcile path; if it ever is, the stub interface drifted.
func (s *StubSidecarClient) SubmittedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tasks)
}

// nodeIDDoer is a sidecar.HttpRequestDoer that answers only the
// /v0/node-id endpoint with a canned, deterministic identity. It backs
// the concrete *sidecar.SidecarClient that ExecutionConfig.NewSidecarClient
// hands out — the path the genesis-ceremony collect-and-set-peers task
// drives (task.collectAndSetPeersExecution.collectPeers → GetNodeID).
//
// The narrow StubSidecarClient satisfies task.SidecarClient (the
// task-submit/poll/health interface) but cannot stand in for the
// concrete *sidecar.SidecarClient that GetNodeID-via-NewSidecarClient
// requires. Faking at the transport layer lets the real client code run
// while keeping the response deterministic.
//
// GetNodeID GETs {baseURL}/v0/node-id and expects {"nodeId":"<hex>"};
// any non-empty value satisfies the peer-list assembly. Non-node-id
// requests get 404 so an unexpected call surfaces loudly rather than
// silently succeeding.
type nodeIDDoer struct {
	nodeID string
}

func (d nodeIDDoer) Do(req *http.Request) (*http.Response, error) {
	if !strings.HasSuffix(req.URL.Path, "/v0/node-id") {
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Body:       io.NopCloser(strings.NewReader("nodeIDDoer: only /v0/node-id is stubbed")),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}
	body := `{"nodeId":"` + d.nodeID + `"}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    req,
	}, nil
}

// NewNodeIDSidecarClient builds a concrete *sidecar.SidecarClient whose
// HTTP transport is faked to answer /v0/node-id deterministically. Wire it
// into ExecutionConfig.NewSidecarClient so the collect-and-set-peers task
// resolves node IDs without a live sidecar. The base URL is arbitrary (the
// fake transport ignores host); the returned node ID mirrors the
// StubSidecarClient's deterministic GetNodeID contract.
func NewNodeIDSidecarClient() (*sidecar.SidecarClient, error) {
	return sidecar.NewSidecarClient("http://stub.invalid", sidecar.WithHTTPDoer(nodeIDDoer{nodeID: "stub-node-id"}))
}
