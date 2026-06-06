package task

import (
	"context"
	"testing"

	"github.com/google/uuid"
	. "github.com/onsi/gomega"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// captureSidecarClient records every submitted TaskRequest and reports tasks as
// still running, so a test can assert what (if anything) reached the sidecar.
type captureSidecarClient struct {
	submitted []sidecar.TaskRequest
}

func (c *captureSidecarClient) SubmitTask(_ context.Context, req sidecar.TaskRequest) (uuid.UUID, error) {
	c.submitted = append(c.submitted, req)
	id := uuid.New()
	if req.Id != nil {
		id = *req.Id
	}
	return id, nil
}

func (c *captureSidecarClient) GetTask(_ context.Context, _ uuid.UUID) (*sidecar.TaskResult, error) {
	return nil, sidecar.ErrNotFound
}
func (c *captureSidecarClient) Healthz(_ context.Context) (bool, error)     { return true, nil }
func (c *captureSidecarClient) GetNodeID(_ context.Context) (string, error) { return "", nil }

func labelPeerNode(resolved ...string) *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "label-node", Namespace: restartNS},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  "label-chain",
			Image:    "sei:v1",
			FullNode: &seiv1alpha1.FullNodeSpec{},
			Peers: []seiv1alpha1.PeerSource{
				{Label: &seiv1alpha1.LabelPeerSource{Selector: map[string]string{"sei.io/chain": "label-chain"}}},
			},
		},
		Status: seiv1alpha1.SeiNodeStatus{ResolvedPeers: resolved},
	}
}

func newDiscoverPeersInitExec(t *testing.T, node *seiv1alpha1.SeiNode, sc SidecarClient) *discoverPeersInitExecution {
	t.Helper()
	cfg := ExecutionConfig{
		Resource:           node,
		BuildSidecarClient: func() (SidecarClient, error) { return sc, nil },
	}
	exec, err := deserializeDiscoverPeersInit(uuid.NewString(), nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return exec.(*discoverPeersInitExecution)
}

// A label-only node whose status.resolvedPeers is empty must NOT submit a
// zero-source discover-peers. Execute defers (marks Failed without error) so
// the executor's retry budget governs convergence, and nothing reaches the
// sidecar.
func TestDiscoverPeersInit_EmptyResolved_DefersWithoutSubmitting(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	node := labelPeerNode() // no resolvedPeers yet
	sc := &captureSidecarClient{}
	exec := newDiscoverPeersInitExec(t, node, sc)

	g.Expect(exec.Execute(ctx)).To(Succeed())
	g.Expect(sc.submitted).To(BeEmpty(), "must not submit a zero-source discover-peers")
	g.Expect(exec.Status(ctx)).To(Equal(ExecutionFailed),
		"empty sources surface as Failed so the executor consumes the retry budget")
	g.Expect(exec.Err()).To(MatchError(ContainSubstring("no resolved peer sources")))
}

// Once status.resolvedPeers is populated, the discover-peers carries the
// resolved sources to the sidecar (the convergence path after reconcilePeers
// runs).
func TestDiscoverPeersInit_ResolvedPopulated_SubmitsResolvedSources(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	resolved := []string{"nodeid@label-node-0.label-node.default.svc.cluster.local:26656"}
	node := labelPeerNode(resolved...)
	sc := &captureSidecarClient{}
	exec := newDiscoverPeersInitExec(t, node, sc)

	g.Expect(exec.Execute(ctx)).To(Succeed())
	g.Expect(sc.submitted).To(HaveLen(1))
	g.Expect(sc.submitted[0].Type).To(Equal(sidecar.TaskTypeDiscoverPeers))

	rebuilt, err := sidecar.DiscoverPeersTaskFromParams(*sc.submitted[0].Params)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rebuilt.Sources).To(HaveLen(1))
	g.Expect(rebuilt.Sources[0].Type).To(Equal(sidecar.PeerSourceStatic))
	g.Expect(rebuilt.Sources[0].Addresses).To(Equal(resolved))

	// Sidecar still processing → Running (not prematurely Complete).
	g.Expect(exec.Status(ctx)).To(Equal(ExecutionRunning))
}

// Convergence: an execution that deferred on empty resolvedPeers submits once a
// later reconcile (fresh deserialize, live node now populated) re-evaluates
// sources. Modeled by a second execution over the populated node.
func TestDiscoverPeersInit_ConvergesAfterResolution(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	sc := &captureSidecarClient{}

	// Reconcile 1: unresolved → defer, no submit.
	empty := newDiscoverPeersInitExec(t, labelPeerNode(), sc)
	g.Expect(empty.Execute(ctx)).To(Succeed())
	g.Expect(empty.Status(ctx)).To(Equal(ExecutionFailed))
	g.Expect(sc.submitted).To(BeEmpty())

	// Reconcile 2 (after reconcilePeers populated status): fresh task, resolved.
	resolved := []string{"nodeid@host:26656"}
	populated := newDiscoverPeersInitExec(t, labelPeerNode(resolved...), sc)
	g.Expect(populated.Execute(ctx)).To(Succeed())
	g.Expect(sc.submitted).To(HaveLen(1))
}
