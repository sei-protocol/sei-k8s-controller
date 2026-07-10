package task_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	. "github.com/onsi/gomega"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"

	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

// recordingSidecar records whether GetTask was polled. A fire-and-forget task
// completes at submit-ack and never polls; a polled task calls GetTask.
type recordingSidecar struct {
	getTaskCalled bool
}

func (m *recordingSidecar) SubmitTask(context.Context, sidecar.TaskRequest) (uuid.UUID, error) {
	return uuid.New(), nil
}

func (m *recordingSidecar) GetTask(context.Context, uuid.UUID) (*sidecar.TaskResult, error) {
	m.getTaskCalled = true
	return nil, fmt.Errorf("still running")
}

func (m *recordingSidecar) Healthz(context.Context) (bool, error)     { return true, nil }
func (m *recordingSidecar) GetNodeID(context.Context) (string, error) { return "node", nil }

// TestRegistry_WorkflowHoldTasksArePolled guards the released-onto-wiped-data
// trap: mark-not-ready does real work (purging mark-ready records) and readiness
// flips only on handler success, so it must be polled — never fire-and-forget
// like its sibling mark-ready. stop-seid and reset-data are likewise polled.
func TestRegistry_WorkflowHoldTasksArePolled(t *testing.T) {
	cases := []struct {
		name       string
		taskType   string
		wantPolled bool
	}{
		{"mark-not-ready is polled", sidecar.TaskTypeMarkNotReady, true},
		{"stop-seid is polled", sidecar.TaskTypeStopSeid, true},
		{"reset-data is polled", sidecar.TaskTypeResetData, true},
		{"mark-ready is fire-and-forget (the trap)", sidecar.TaskTypeMarkReady, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			mock := &recordingSidecar{}
			cfg := task.ExecutionConfig{
				BuildSidecarClient: func() (task.SidecarClient, error) { return mock, nil },
			}
			id := task.DeterministicTaskID("plan", tc.taskType, 0)
			exec, err := task.Deserialize(tc.taskType, id, nil, cfg)
			g.Expect(err).NotTo(HaveOccurred())

			ctx := context.Background()
			g.Expect(exec.Execute(ctx)).To(Succeed())
			_ = exec.Status(ctx)

			g.Expect(mock.getTaskCalled).To(Equal(tc.wantPolled),
				"polled tasks must call GetTask; fire-and-forget tasks must not")
		})
	}
}
