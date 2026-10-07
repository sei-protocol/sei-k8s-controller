package task_test

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/google/uuid"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
	sidecar "github.com/sei-protocol/sei-k8s-controller/sidecarapi/client"
)

// countingSidecar counts submissions.
type countingSidecar struct {
	recordingSidecar
	submits int
}

func (m *countingSidecar) SubmitTask(ctx context.Context, req sidecar.TaskRequest) (uuid.UUID, error) {
	m.submits++
	return m.recordingSidecar.SubmitTask(ctx, req)
}

func nodeConfigNode(spec, handled int64) *seiv1alpha1.SeiNode {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "rpc-0", Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeSpec{
			NodeConfig: &seiv1alpha1.NodeConfig{
				ConfigRef: seiv1alpha1.ConfigFileRef{Name: "c"},
				AppRef:    seiv1alpha1.ConfigFileRef{Name: "c"},
			},
			DataResetGeneration: spec,
		},
		Status: seiv1alpha1.SeiNodeStatus{DataResetGeneration: handled, Phase: seiv1alpha1.PhaseRunning},
	}
	return node
}

// 009 Req 3: the start guard blocks only while a reset is pending.
func TestStartBlocked(t *testing.T) {
	g := NewWithT(t)
	g.Expect(task.StartBlocked(nodeConfigNode(2, 1))).To(ContainSubstring("data reset pending"))
	g.Expect(task.StartBlocked(nodeConfigNode(2, 2))).To(BeEmpty())
	g.Expect(task.StartBlocked(nodeConfigNode(0, 0))).To(BeEmpty())

	plain := nodeConfigNode(2, 0)
	plain.Spec.NodeConfig = nil
	g.Expect(task.StartBlocked(plain)).To(BeEmpty(), "only a nodeConfig node runs the reset")
}

// 009 Req 3.1, 3.2: mark-ready fails terminally under the guard and submits
// nothing; with no reset pending it submits as before.
func TestMarkReady_StartGuard(t *testing.T) {
	cases := []struct {
		name        string
		node        *seiv1alpha1.SeiNode
		wantRefused bool
	}{
		{"reset pending", nodeConfigNode(1, 0), true},
		{"reset handled", nodeConfigNode(1, 1), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			mock := &countingSidecar{}
			cfg := task.ExecutionConfig{
				BuildSidecarClient: func() (task.SidecarClient, error) { return mock, nil },
				Resource:           tc.node,
			}
			exec, err := task.Deserialize(sidecar.TaskTypeMarkReady, task.DeterministicTaskID("p", sidecar.TaskTypeMarkReady, 0), nil, cfg)
			g.Expect(err).NotTo(HaveOccurred())

			err = exec.Execute(context.Background())
			if tc.wantRefused {
				var terminal *task.TerminalError
				g.Expect(errors.As(err, &terminal)).To(BeTrue(), "a refusal is terminal")
				g.Expect(exec.Status(context.Background())).To(Equal(task.ExecutionFailed))
				g.Expect(exec.Err()).To(MatchError(ContainSubstring("start guard")))
				g.Expect(mock.submits).To(Equal(0))
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(exec.Status(context.Background())).To(Equal(task.ExecutionComplete))
			g.Expect(mock.submits).To(Equal(1))
		})
	}
}

// A group plan's resource is a SeiNetwork, which has no start gate of its own:
// mark-ready passes through unguarded.
func TestMarkReady_NonNodeResourceUnguarded(t *testing.T) {
	g := NewWithT(t)
	mock := &countingSidecar{}
	cfg := task.ExecutionConfig{
		BuildSidecarClient: func() (task.SidecarClient, error) { return mock, nil },
		Resource:           &seiv1alpha1.SeiNetwork{},
	}
	exec, err := task.Deserialize(sidecar.TaskTypeMarkReady, task.DeterministicTaskID("p", sidecar.TaskTypeMarkReady, 0), nil, cfg)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(mock.submits).To(Equal(1))
}

// 009 Req 2.3: record-data-reset stamps the value the plan was built for and
// never lowers the handled counter.
func TestRecordDataReset(t *testing.T) {
	cases := []struct {
		name      string
		handled   int64
		recordFor int64
		want      int64
	}{
		{"raises", 1, 3, 3},
		{"never lowers", 5, 3, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			node := nodeConfigNode(tc.recordFor, tc.handled)
			params := []byte(`{"generation":` + strconv.FormatInt(tc.recordFor, 10) + `}`)
			exec, err := task.Deserialize(task.TaskTypeRecordDataReset, "id", params, task.ExecutionConfig{Resource: node})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(exec.Execute(context.Background())).To(Succeed())
			g.Expect(exec.Status(context.Background())).To(Equal(task.ExecutionComplete))
			g.Expect(node.Status.DataResetGeneration).To(Equal(tc.want))
		})
	}
}

// 010 Req 2.4: a hold blocks mark-ready but not the hold's own start-once
// step; a pending reset blocks both.
func TestStartGuard_Hold(t *testing.T) {
	cases := []struct {
		name          string
		hold          seiv1alpha1.MaintenanceHold
		resetPending  bool
		taskType      string
		wantSubmitted bool
	}{
		{"mark-ready under hold", seiv1alpha1.MaintenanceHoldImmediate, false, sidecar.TaskTypeMarkReady, false},
		{"start-once under hold", seiv1alpha1.MaintenanceHoldAfterExit, false, task.TaskTypeStartSeidOnce, true},
		{"start-once under reset", seiv1alpha1.MaintenanceHoldAfterExit, true, task.TaskTypeStartSeidOnce, false},
		{"mark-ready released", "", false, sidecar.TaskTypeMarkReady, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			node := nodeConfigNode(0, 0)
			if tc.resetPending {
				node.Spec.DataResetGeneration = 1
			}
			if tc.hold != "" {
				node.Spec.Maintenance = &seiv1alpha1.MaintenanceSpec{Hold: tc.hold}
			}
			mock := &countingSidecar{}
			cfg := task.ExecutionConfig{
				BuildSidecarClient: func() (task.SidecarClient, error) { return mock, nil },
				Resource:           node,
			}
			exec, err := task.Deserialize(tc.taskType, task.DeterministicTaskID("p", tc.taskType, 0), nil, cfg)
			g.Expect(err).NotTo(HaveOccurred())
			err = exec.Execute(context.Background())
			if tc.wantSubmitted {
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(mock.submits).To(Equal(1))
				return
			}
			var terminal *task.TerminalError
			g.Expect(errors.As(err, &terminal)).To(BeTrue())
			g.Expect(mock.submits).To(Equal(0))
		})
	}
}

// 010 Req 2.7: record-maintenance-hold writes the hold in effect, including
// the empty value a release records.
func TestRecordMaintenanceHold(t *testing.T) {
	for _, hold := range []seiv1alpha1.MaintenanceHold{seiv1alpha1.MaintenanceHoldImmediate, ""} {
		g := NewWithT(t)
		node := nodeConfigNode(0, 0)
		node.Status.MaintenanceHold = seiv1alpha1.MaintenanceHoldAfterExit
		params := []byte(`{"hold":"` + string(hold) + `"}`)
		exec, err := task.Deserialize(task.TaskTypeRecordMaintenanceHold, "id", params, task.ExecutionConfig{Resource: node})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(exec.Execute(context.Background())).To(Succeed())
		g.Expect(node.Status.MaintenanceHold).To(Equal(hold))
	}
}

// Review finding: a hold set while an init plan runs must not fail the node.
// The hold half of the guard acts only on a Running node.
func TestStartGuard_HoldInertBeforeRunning(t *testing.T) {
	g := NewWithT(t)
	node := nodeConfigNode(0, 0)
	node.Spec.Maintenance = &seiv1alpha1.MaintenanceSpec{Hold: seiv1alpha1.MaintenanceHoldImmediate}
	node.Status.Phase = seiv1alpha1.PhaseInitializing
	g.Expect(task.StartBlocked(node)).To(BeEmpty())

	node.Status.Phase = seiv1alpha1.PhaseRunning
	g.Expect(task.StartBlocked(node)).To(ContainSubstring("maintenance hold"))
}
