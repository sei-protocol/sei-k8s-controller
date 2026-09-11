package planner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/gomega"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
	sidecar "github.com/sei-protocol/sei-k8s-controller/sidecarapi/client"
)

type fakeSidecarClient struct {
	healthy   bool
	err       error
	status    *sidecar.StatusResponse
	statusErr error
}

func (f *fakeSidecarClient) SubmitTask(context.Context, sidecar.TaskRequest) (uuid.UUID, error) {
	return uuid.Nil, nil
}
func (f *fakeSidecarClient) GetTask(context.Context, uuid.UUID) (*sidecar.TaskResult, error) {
	return nil, sidecar.ErrNotFound
}
func (f *fakeSidecarClient) Healthz(context.Context) (bool, error) { return f.healthy, f.err }
func (f *fakeSidecarClient) Status(context.Context) (*sidecar.StatusResponse, error) {
	return f.status, f.statusErr
}
func (f *fakeSidecarClient) GetNodeID(context.Context) (string, error) { return "", nil }

func findSidecarReady(node *seiv1alpha1.SeiNode) *metav1.Condition {
	return apimeta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionSidecarReady)
}

func TestProbeSidecarHealth_200_SetsReady(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()

	probeSidecarHealth(context.Background(), node, &fakeSidecarClient{healthy: true})

	c := findSidecarReady(node)
	g.Expect(c).NotTo(BeNil())
	g.Expect(c.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(c.Reason).To(Equal("Ready"))
}

func TestProbeSidecarHealth_503_SetsNotReady(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()

	probeSidecarHealth(context.Background(), node, &fakeSidecarClient{healthy: false})

	c := findSidecarReady(node)
	g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(c.Reason).To(Equal("NotReady"))
}

func TestProbeSidecarHealth_NetworkError_SetsUnknown(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()

	probeSidecarHealth(context.Background(), node, &fakeSidecarClient{err: errors.New("boom")})

	c := findSidecarReady(node)
	g.Expect(c.Status).To(Equal(metav1.ConditionUnknown))
	g.Expect(c.Reason).To(Equal("Unreachable"))
}

func TestResolvePlan_NilClient_DoesNotProbe(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()

	err := (&NodeResolver{}).ResolvePlan(context.Background(), node)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(findSidecarReady(node)).To(BeNil(),
		"no probe should run when client is nil")
}

func TestResolvePlan_Initializing_DoesNotProbe(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Status.Phase = seiv1alpha1.PhaseInitializing

	err := (&NodeResolver{BuildSidecarClient: func(*seiv1alpha1.SeiNode) (task.SidecarClient, error) { return &fakeSidecarClient{healthy: false}, nil }}).ResolvePlan(context.Background(), node)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(findSidecarReady(node)).To(BeNil(),
		"probe must be skipped when not Running — init plan owns the sidecar")
}

func TestObserveCommittedHeight_StampsHeightAndReadTime(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	h := int64(1234)

	observeCommittedHeight(context.Background(), node,
		&fakeSidecarClient{status: &sidecar.StatusResponse{Status: sidecar.Ready, CommittedHeight: &h}})

	g.Expect(node.Status.CommittedHeight).To(HaveValue(Equal(int64(1234))))
	g.Expect(node.Status.CommittedHeightReadTime).NotTo(BeNil())
}

// A failed read, or a sidecar whose status omits the field (an older image),
// must leave the prior reading in place so its read time ages into "stale"
// rather than being overwritten with a zero.
func TestObserveCommittedHeight_UnreadableLeavesPriorReading(t *testing.T) {
	g := NewWithT(t)
	prior := int64(77)
	priorAt := metav1.NewTime(time.Now().Add(-5 * time.Minute))

	for name, fc := range map[string]*fakeSidecarClient{
		"error":         {statusErr: errors.New("boom")},
		"field-omitted": {status: &sidecar.StatusResponse{Status: sidecar.Ready}},
	} {
		node := runningFullNode()
		node.Status.CommittedHeight = &prior
		node.Status.CommittedHeightReadTime = &priorAt

		observeCommittedHeight(context.Background(), node, fc)

		g.Expect(node.Status.CommittedHeight).To(HaveValue(Equal(int64(77))), name)
		g.Expect(node.Status.CommittedHeightReadTime.Time).To(Equal(priorAt.Time), name)
	}
}
