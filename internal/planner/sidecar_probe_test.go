package planner

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	. "github.com/onsi/gomega"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

type fakeSidecarClient struct {
	healthy bool
	err     error
}

func (f *fakeSidecarClient) SubmitTask(context.Context, sidecar.TaskRequest) (uuid.UUID, error) {
	return uuid.Nil, nil
}
func (f *fakeSidecarClient) GetTask(context.Context, uuid.UUID) (*sidecar.TaskResult, error) {
	return nil, sidecar.ErrNotFound
}
func (f *fakeSidecarClient) Healthz(context.Context) (bool, error) { return f.healthy, f.err }

func findCond(node *seiv1alpha1.SeiNode, typ string) *metav1.Condition {
	return apimeta.FindStatusCondition(node.Status.Conditions, typ)
}

func TestProbeSidecarHealth_200_SetsReady(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()

	probeSidecarHealth(context.Background(), node, &fakeSidecarClient{healthy: true})

	c := findCond(node, seiv1alpha1.ConditionSidecarReady)
	g.Expect(c).NotTo(BeNil())
	g.Expect(c.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(c.Reason).To(Equal("Ready"))
}

func TestProbeSidecarHealth_503_SetsNotReady(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()

	probeSidecarHealth(context.Background(), node, &fakeSidecarClient{healthy: false})

	c := findCond(node, seiv1alpha1.ConditionSidecarReady)
	g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(c.Reason).To(Equal("NotReady"))
}

func TestProbeSidecarHealth_NetworkError_SetsUnknown(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()

	probeSidecarHealth(context.Background(), node, &fakeSidecarClient{err: errors.New("boom")})

	c := findCond(node, seiv1alpha1.ConditionSidecarReady)
	g.Expect(c.Status).To(Equal(metav1.ConditionUnknown))
	g.Expect(c.Reason).To(Equal("Unreachable"))
}

func TestResolvePlan_NilClient_DoesNotProbe(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()

	err := (&NodeResolver{}).ResolvePlan(context.Background(), node)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(findCond(node, seiv1alpha1.ConditionSidecarReady)).To(BeNil(),
		"no probe should run when client is nil")
}

func TestResolvePlan_Initializing_DoesNotProbe(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Status.Phase = seiv1alpha1.PhaseInitializing

	err := (&NodeResolver{BuildSidecarClient: func(*seiv1alpha1.SeiNode) (task.SidecarClient, error) { return &fakeSidecarClient{healthy: false}, nil }}).ResolvePlan(context.Background(), node)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(findCond(node, seiv1alpha1.ConditionSidecarReady)).To(BeNil(),
		"probe must be skipped when not Running — init plan owns the sidecar")
}
