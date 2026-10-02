package evmdigestcompare

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/google/uuid"

	sidecar "github.com/sei-protocol/sei-k8s-controller/sidecarapi/client"
	"github.com/sei-protocol/sei-k8s-controller/sidecarapi/wire"
)

// fakeSource is an in-memory sidecar whose digest tasks complete on the first
// poll with the result digest returns.
type fakeSource struct {
	mu        sync.Mutex
	height    *int64
	statusErr error
	// digest returns the result for a scan at h; nil result means the task fails.
	digest func(h int64) *wire.EVMLogicalDigestResult
	// stuck keeps submitted tasks running forever.
	stuck   bool
	tasks   map[uuid.UUID]*sidecar.TaskResult
	submits []sidecar.TaskRequest
	deleted []uuid.UUID
}

func newFakeSource(height int64, digest func(h int64) *wire.EVMLogicalDigestResult) *fakeSource {
	return &fakeSource{height: &height, digest: digest, tasks: map[uuid.UUID]*sidecar.TaskResult{}}
}

func digestOf(digest string, count uint64) func(h int64) *wire.EVMLogicalDigestResult {
	return func(h int64) *wire.EVMLogicalDigestResult {
		return &wire.EVMLogicalDigestResult{
			RequestedHeight: h, Version: h, DurationSeconds: 12,
			Final: wire.EVMDigestBucket{Count: count, Digest: digest},
		}
	}
}

func (f *fakeSource) Status(context.Context) (*sidecar.StatusResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	return &sidecar.StatusResponse{CommittedHeight: f.height, Status: sidecar.Ready}, nil
}

func (f *fakeSource) SubmitTask(_ context.Context, req sidecar.TaskRequest) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submits = append(f.submits, req)
	id := *req.Id
	if _, ok := f.tasks[id]; ok {
		return id, nil
	}
	tr := &sidecar.TaskResult{Id: id, Type: req.Type, Status: sidecar.Running}
	if !f.stuck {
		h := (*req.Params)["height"].(int64)
		if res := f.digest(h); res != nil {
			raw, _ := json.Marshal(res)
			msg := json.RawMessage(raw)
			tr.Status, tr.Result = sidecar.Completed, &msg
		} else {
			e := "seidb exited 1"
			tr.Status, tr.Error = sidecar.Failed, &e
		}
	}
	f.tasks[id] = tr
	return id, nil
}

func (f *fakeSource) GetTask(_ context.Context, id uuid.UUID) (*sidecar.TaskResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tr, ok := f.tasks[id]
	if !ok {
		return nil, sidecar.ErrNotFound
	}
	cp := *tr
	return &cp, nil
}

func (f *fakeSource) ListTasks(context.Context) ([]sidecar.TaskResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sidecar.TaskResult, 0, len(f.tasks))
	for _, tr := range f.tasks {
		out = append(out, *tr)
	}
	return out, nil
}

func (f *fakeSource) DeleteTask(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.tasks[id]; !ok {
		return errors.New("not found")
	}
	delete(f.tasks, id)
	f.deleted = append(f.deleted, id)
	return nil
}
