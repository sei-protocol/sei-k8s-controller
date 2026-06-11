package peering

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/google/uuid"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const (
	nsDefault       = "default"
	roleLabel       = "role"
	rolePeer        = "peer"
	roleSeed        = "seed"
	ec2PeerEntry    = "ec2id@10.0.0.5:26656"
	staticPeerEntry = "static@9.9.9.9:26656"
	regionUSEast1   = "us-east-1"
)

// --- test doubles ---

type mockSidecar struct {
	nodeID    string
	nodeIDErr error
}

func (m *mockSidecar) SubmitTask(context.Context, sidecar.TaskRequest) (uuid.UUID, error) {
	return uuid.Nil, nil
}
func (m *mockSidecar) GetTask(context.Context, uuid.UUID) (*sidecar.TaskResult, error) {
	return nil, nil
}
func (m *mockSidecar) Healthz(context.Context) (bool, error)     { return true, nil }
func (m *mockSidecar) GetNodeID(context.Context) (string, error) { return m.nodeID, m.nodeIDErr }

// mockEC2 satisfies EC2Resolver.
type mockEC2 struct {
	peers  []string
	err    error
	called bool
}

func (m *mockEC2) Resolve(context.Context, *seiv1alpha1.EC2TagsPeerSource) ([]string, error) {
	m.called = true
	return m.peers, m.err
}

func testScheme(t *testing.T) *k8sruntime.Scheme {
	t.Helper()
	s := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := seiv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newReader(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
}

func fullNode(name string, labels map[string]string) *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nsDefault, Labels: labels},
		Spec:       seiv1alpha1.SeiNodeSpec{ChainID: "test-1", Image: "sei:latest", FullNode: &seiv1alpha1.FullNodeSpec{}},
	}
}

func sidecarFactory(m task.SidecarClient) func(*seiv1alpha1.SeiNode) (task.SidecarClient, error) {
	return func(*seiv1alpha1.SeiNode) (task.SidecarClient, error) { return m, nil }
}

// --- tests ---

func TestResolve_Static_Verbatim(t *testing.T) {
	node := fullNode("n", nil)
	node.Spec.Peers = []seiv1alpha1.PeerSource{
		{Static: &seiv1alpha1.StaticPeerSource{Addresses: []string{"abc@1.2.3.4:26656", "def@5.6.7.8:26656"}}},
	}
	r := Resolver{Reader: newReader(t, node)}

	got, err := r.Resolve(context.Background(), node, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []string{"abc@1.2.3.4:26656", "def@5.6.7.8:26656"}
	assertEqualPeers(t, got.Peers, want)
}

func TestResolve_Label_ComposesNodeID(t *testing.T) {
	consumer := fullNode("consumer", nil)
	consumer.Spec.Peers = []seiv1alpha1.PeerSource{
		{Label: &seiv1alpha1.LabelPeerSource{Selector: map[string]string{roleLabel: rolePeer}}},
	}
	peer := fullNode("peer-1", map[string]string{roleLabel: rolePeer})

	r := Resolver{
		Reader:             newReader(t, consumer, peer),
		BuildSidecarClient: sidecarFactory(&mockSidecar{nodeID: "node-xyz"}),
	}

	got, err := r.Resolve(context.Background(), consumer, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertEqualPeers(t, got.Peers, []string{"node-xyz@peer-1-0.peer-1.default.svc.cluster.local:26656"})
}

func TestResolve_Label_PrefersExternalAddress(t *testing.T) {
	consumer := fullNode("consumer", nil)
	consumer.Spec.Peers = []seiv1alpha1.PeerSource{
		{Label: &seiv1alpha1.LabelPeerSource{Selector: map[string]string{roleLabel: "pub"}}},
	}
	peer := fullNode("pub", map[string]string{roleLabel: "pub"})
	peer.Spec.ExternalAddress = "pub.example.com:26656"

	r := Resolver{
		Reader:             newReader(t, consumer, peer),
		BuildSidecarClient: sidecarFactory(&mockSidecar{nodeID: "nid"}),
	}

	got, err := r.Resolve(context.Background(), consumer, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertEqualPeers(t, got.Peers, []string{"nid@pub.example.com:26656"})
}

func TestResolve_Label_EmptySetYieldsNoPeers(t *testing.T) {
	consumer := fullNode("consumer", nil)
	consumer.Spec.Peers = []seiv1alpha1.PeerSource{
		{Label: &seiv1alpha1.LabelPeerSource{Selector: map[string]string{roleLabel: "none"}}},
	}
	r := Resolver{
		Reader:             newReader(t, consumer),
		BuildSidecarClient: sidecarFactory(&mockSidecar{nodeID: "nid"}),
	}

	got, err := r.Resolve(context.Background(), consumer, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Empty set: persistent_peers="" downstream, no deadlock. No #394 hack.
	if len(got.Peers) != 0 {
		t.Errorf("expected empty peer set, got %v", got.Peers)
	}
}

func TestResolve_Label_TransientFailurePreservesPriorEntry(t *testing.T) {
	consumer := fullNode("consumer", nil)
	consumer.Spec.Peers = []seiv1alpha1.PeerSource{
		{Label: &seiv1alpha1.LabelPeerSource{Selector: map[string]string{roleLabel: rolePeer}}},
	}
	peer := fullNode("peer-1", map[string]string{roleLabel: rolePeer})
	prior := []string{"prior-id@peer-1-0.peer-1.default.svc.cluster.local:26656"}

	r := Resolver{
		Reader:             newReader(t, consumer, peer),
		BuildSidecarClient: sidecarFactory(&mockSidecar{nodeIDErr: errors.New("sidecar mid-restart")}),
	}

	got, err := r.Resolve(context.Background(), consumer, prior)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// node_id fetch failed but a prior entry for this host exists → preserved.
	assertEqualPeers(t, got.Peers, prior)
}

func TestResolve_Label_TransientFailureNoPriorSkips(t *testing.T) {
	consumer := fullNode("consumer", nil)
	consumer.Spec.Peers = []seiv1alpha1.PeerSource{
		{Label: &seiv1alpha1.LabelPeerSource{Selector: map[string]string{roleLabel: rolePeer}}},
	}
	peer := fullNode("peer-1", map[string]string{roleLabel: rolePeer})

	r := Resolver{
		Reader:             newReader(t, consumer, peer),
		BuildSidecarClient: sidecarFactory(&mockSidecar{nodeIDErr: errors.New("unreachable")}),
	}

	got, err := r.Resolve(context.Background(), consumer, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got.Peers) != 0 {
		t.Errorf("new unresolvable peer should be skipped, got %v", got.Peers)
	}
}

func TestResolve_EC2_ComposesFromMock(t *testing.T) {
	node := fullNode("n", nil)
	node.Spec.Peers = []seiv1alpha1.PeerSource{
		{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: regionUSEast1, Tags: map[string]string{roleLabel: roleSeed}}},
	}
	r := Resolver{
		Reader: newReader(t, node),
		EC2:    &mockEC2{peers: []string{ec2PeerEntry}},
	}

	got, err := r.Resolve(context.Background(), node, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertEqualPeers(t, got.Peers, []string{ec2PeerEntry})
}

func TestResolve_EC2_TransientFailurePreservesPriorSet(t *testing.T) {
	node := fullNode("n", nil)
	node.Spec.Peers = []seiv1alpha1.PeerSource{
		{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: regionUSEast1, Tags: map[string]string{roleLabel: roleSeed}}},
	}
	prior := []string{ec2PeerEntry}
	r := Resolver{
		Reader: newReader(t, node),
		EC2:    &mockEC2{err: errors.New("throttled")},
	}

	got, err := r.Resolve(context.Background(), node, prior)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// EC2 transient failure preserves the whole prior peer set (no churn).
	assertEqualPeers(t, got.Peers, prior)
}

func TestResolve_EC2_NilResolverPreservesPriorSet(t *testing.T) {
	node := fullNode("n", nil)
	node.Spec.Peers = []seiv1alpha1.PeerSource{
		{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: regionUSEast1, Tags: map[string]string{roleLabel: roleSeed}}},
	}
	prior := []string{ec2PeerEntry}
	r := Resolver{Reader: newReader(t, node)} // EC2 nil

	got, err := r.Resolve(context.Background(), node, prior)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertEqualPeers(t, got.Peers, prior)
}

// TestResolve_EC2FirstPreserve_LaterSourcesStillResolve proves a transient EC2
// failure (ordered first) does not discard or skip the sources after it: the
// later Static source's fresh peers are present AND prior is unioned (no-shrink).
func TestResolve_EC2FirstPreserve_LaterSourcesStillResolve(t *testing.T) {
	node := fullNode("n", nil)
	node.Spec.Peers = []seiv1alpha1.PeerSource{
		{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: regionUSEast1, Tags: map[string]string{roleLabel: roleSeed}}},
		{Static: &seiv1alpha1.StaticPeerSource{Addresses: []string{staticPeerEntry}}},
	}
	prior := []string{ec2PeerEntry}
	r := Resolver{
		Reader: newReader(t, node),
		EC2:    &mockEC2{err: errors.New("throttled")},
	}

	got, err := r.Resolve(context.Background(), node, prior)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertEqualPeers(t, got.Peers, []string{ec2PeerEntry, staticPeerEntry})
}

// TestResolve_NonEC2FirstThenEC2Fails proves a fresh non-EC2 peer accumulated
// before a failing EC2 source is retained (not discarded for the bare prior set)
// while prior is unioned in.
func TestResolve_NonEC2FirstThenEC2Fails(t *testing.T) {
	node := fullNode("n", nil)
	node.Spec.Peers = []seiv1alpha1.PeerSource{
		{Static: &seiv1alpha1.StaticPeerSource{Addresses: []string{staticPeerEntry}}},
		{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: regionUSEast1, Tags: map[string]string{roleLabel: roleSeed}}},
	}
	prior := []string{ec2PeerEntry}
	r := Resolver{
		Reader: newReader(t, node),
		EC2:    &mockEC2{err: errors.New("throttled")},
	}

	got, err := r.Resolve(context.Background(), node, prior)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertEqualPeers(t, got.Peers, []string{ec2PeerEntry, staticPeerEntry})
}

// TestResolve_EC2_NilResolverMakesNoCall exercises the nil-resolver defensive
// guard: an EC2Tags source with a nil EC2 resolver must short-circuit before any
// Resolve call, so the AWS SDK never runs LoadDefaultConfig / IMDS. This guards
// against an unconfigured resolver (e.g. in tests) silently reaching for ambient
// credentials. A non-nil resolver on a separate run confirms the resolver would
// otherwise have recorded the call.
func TestResolve_EC2_NilResolverMakesNoCall(t *testing.T) {
	node := fullNode("n", nil)
	node.Spec.Peers = []seiv1alpha1.PeerSource{
		{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: regionUSEast1, Tags: map[string]string{roleLabel: roleSeed}}},
	}

	// Nil EC2 resolver: no resolver exists to record a call; the assertion is
	// that resolveEC2's nil guard returns preserve-prior without constructing
	// or invoking any AWS client.
	rNil := Resolver{Reader: newReader(t, node)}
	if _, err := rNil.Resolve(context.Background(), node, []string{ec2PeerEntry}); err != nil {
		t.Fatalf("Resolve (nil resolver): %v", err)
	}

	// Non-nil resolver: a spy IS invoked, proving the source is live and the
	// nil-path skip above was the only thing suppressing the call.
	spy := &mockEC2{peers: []string{ec2PeerEntry}}
	rLive := Resolver{Reader: newReader(t, node), EC2: spy}
	if _, err := rLive.Resolve(context.Background(), node, nil); err != nil {
		t.Fatalf("Resolve (live resolver): %v", err)
	}
	if !spy.called {
		t.Fatal("live resolver should have been called; test is not exercising the EC2 path")
	}
}

// --- EC2 client-level composition (mocked DescribeInstances) ---

type fakeEC2API struct {
	out *ec2.DescribeInstancesOutput
	err error
}

func (f *fakeEC2API) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return f.out, f.err
}

// Valid CometBFT node-ids (40 lowercase hex) for the composition tests.
const (
	nodeIDA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	nodeIDB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestResolveEC2Instances_ComposesPeer(t *testing.T) {
	api := &fakeEC2API{out: &ec2.DescribeInstancesOutput{
		Reservations: []ec2types.Reservation{{
			Instances: []ec2types.Instance{
				{
					InstanceId:    aws.String("i-1"),
					PublicDnsName: aws.String("ec2-1.example.com"),
					Tags: []ec2types.Tag{
						{Key: aws.String(tagNodeID), Value: aws.String(nodeIDA)},
					},
				},
				{
					// custom P2P port via tag; falls back to private IP host.
					InstanceId:       aws.String("i-2"),
					PrivateIpAddress: aws.String("10.0.0.9"),
					Tags: []ec2types.Tag{
						{Key: aws.String(tagNodeID), Value: aws.String(nodeIDB)},
						{Key: aws.String(tagP2PPort), Value: aws.String("30000")},
					},
				},
				{
					// no node_id tag → skipped.
					InstanceId:    aws.String("i-3"),
					PublicDnsName: aws.String("ec2-3.example.com"),
				},
			},
		}},
	}}

	src := &seiv1alpha1.EC2TagsPeerSource{Region: regionUSEast1, Tags: map[string]string{roleLabel: roleSeed}}
	got, err := resolveEC2Instances(context.Background(), api, src)
	if err != nil {
		t.Fatalf("resolveEC2Instances: %v", err)
	}
	want := []string{nodeIDA + "@ec2-1.example.com:26656", nodeIDB + "@10.0.0.9:30000"}
	assertEqualPeers(t, got, want)
}

// TestResolveEC2Instances_RejectsUnsafeTagInput proves tag-derived node_id and
// host are validated before composition: a malformed node_id and a host with an
// injected comma are both skipped, while the one well-formed instance survives.
func TestResolveEC2Instances_RejectsUnsafeTagInput(t *testing.T) {
	api := &fakeEC2API{out: &ec2.DescribeInstancesOutput{
		Reservations: []ec2types.Reservation{{
			Instances: []ec2types.Instance{
				{
					// node_id not 40-hex → skipped.
					InstanceId:    aws.String("i-bad-nodeid"),
					PublicDnsName: aws.String("ec2-bad.example.com"),
					Tags:          []ec2types.Tag{{Key: aws.String(tagNodeID), Value: aws.String("nodeA")}},
				},
				{
					// uppercase hex is not the lowercase shape → skipped.
					InstanceId:    aws.String("i-upper-nodeid"),
					PublicDnsName: aws.String("ec2-upper.example.com"),
					Tags:          []ec2types.Tag{{Key: aws.String(tagNodeID), Value: aws.String("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")}},
				},
				{
					// comma in host would inject an extra persistent_peers entry → skipped.
					InstanceId:       aws.String("i-comma-host"),
					PrivateIpAddress: aws.String("10.0.0.9,evil@1.2.3.4"),
					Tags:             []ec2types.Tag{{Key: aws.String(tagNodeID), Value: aws.String(nodeIDB)}},
				},
				{
					// well-formed → survives.
					InstanceId:    aws.String("i-ok"),
					PublicDnsName: aws.String("ec2-ok.example.com"),
					Tags:          []ec2types.Tag{{Key: aws.String(tagNodeID), Value: aws.String(nodeIDA)}},
				},
			},
		}},
	}}

	src := &seiv1alpha1.EC2TagsPeerSource{Region: regionUSEast1, Tags: map[string]string{roleLabel: roleSeed}}
	got, err := resolveEC2Instances(context.Background(), api, src)
	if err != nil {
		t.Fatalf("resolveEC2Instances: %v", err)
	}
	assertEqualPeers(t, got, []string{nodeIDA + "@ec2-ok.example.com:26656"})
}

func TestResolveEC2Instances_DescribeError(t *testing.T) {
	api := &fakeEC2API{err: errors.New("AccessDenied")}
	src := &seiv1alpha1.EC2TagsPeerSource{Region: regionUSEast1, Tags: map[string]string{roleLabel: roleSeed}}
	if _, err := resolveEC2Instances(context.Background(), api, src); err == nil {
		t.Fatal("expected error from DescribeInstances failure")
	}
}

func assertEqualPeers(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%v vs %v)", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
