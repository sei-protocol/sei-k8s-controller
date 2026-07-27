//go:build envtest

package envtest_test

import (
	"testing"

	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/seinetwork/envtest/fixtures"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest"
)

// Seed reconcile coverage. The admission tests in internal/controller/node/envtest
// pin the CEL contract against a live apiserver but run no reconciler; these run
// under the suite's real manager and assert what a seed actually converges to.
//
// Every property below is one seid would fail on: a probe it cannot answer, a
// sidecar that blocks on a port it never binds, an identity it would regenerate,
// or a pool sized for a node it is not.

func newTestSeedNode(namespace, name, nodeKeySecret string) *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "pacific-1",
			Image:   fixtures.DefaultImage,
			Seed: &seiv1alpha1.SeedSpec{
				NodeKey: seiv1alpha1.NodeKeySource{
					Secret: &seiv1alpha1.SecretNodeKeySource{SecretName: nodeKeySecret},
				},
			},
		},
	}
}

// seedStatefulSet creates a seed and returns the StatefulSet the controller
// converges to.
func seedStatefulSet(t *testing.T, ns, name string) *appsv1.StatefulSet {
	t.Helper()
	g := NewWithT(t)

	node := newTestSeedNode(ns, name, name+"-node-key")
	g.Expect(testCli.Create(testCtx, node)).To(Succeed())
	key := client.ObjectKeyFromObject(node)

	waitFor(t, func() bool {
		cur := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, key, cur); err != nil {
			return false
		}
		return cur.Status.StatefulSet != nil && cur.Status.StatefulSet.UID != ""
	}, "a seed must converge to a tracked StatefulSet")

	sts := &appsv1.StatefulSet{}
	g.Expect(testCli.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, sts)).To(Succeed())
	return sts
}

func containerNamed(spec corev1.PodSpec, name string) *corev1.Container {
	for i := range spec.Containers {
		if spec.Containers[i].Name == name {
			return &spec.Containers[i]
		}
	}
	return nil
}

// The pod shape is the whole reason seed needed controller work: inherited
// unchanged, a seed never reaches Ready and its exporter crash-loops.
func TestSeed_ReconcilesToSeedShapedStatefulSet(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	sts := seedStatefulSet(t, ns, "seed-shape")
	spec := sts.Spec.Template.Spec

	// cosmos-exporter blocks on seid's gRPC, which a seed never binds, so it
	// would sit in its wait loop until its own liveness probe killed it.
	g.Expect(containerNamed(spec, "cosmos-exporter")).To(BeNil(),
		"a seed must not carry cosmos-exporter")
	g.Expect(spec.Containers).To(HaveLen(1))

	seid := containerNamed(spec, "seid")
	g.Expect(seid).NotTo(BeNil())

	// /lag_status on 26657 can never answer on a seed, so readiness would never
	// pass. P2P is the one port it does bind.
	g.Expect(seid.ReadinessProbe).NotTo(BeNil())
	g.Expect(seid.ReadinessProbe.HTTPGet).To(BeNil())
	g.Expect(seid.ReadinessProbe.TCPSocket).NotTo(BeNil())
	g.Expect(seid.ReadinessProbe.TCPSocket.Port.IntVal).To(Equal(int32(26656)))

	// Memory is request==limit, so GC slack crossing the cap is an OOMKill.
	var goMemLimit string
	for _, e := range seid.Env {
		if e.Name == "GOMEMLIMIT" {
			goMemLimit = e.Value
		}
	}
	g.Expect(goMemLimit).NotTo(BeEmpty(), "a seed must carry GOMEMLIMIT")
}

// The published NodeID must come from the Secret; left to `seid init` it
// regenerates onto the data volume and every client holding the old one breaks.
func TestSeed_MountsNodeKeyFromSecret(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	sts := seedStatefulSet(t, ns, "seed-identity")
	spec := sts.Spec.Template.Spec

	var vol *corev1.Volume
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == "node-key" {
			vol = &spec.Volumes[i]
		}
	}
	g.Expect(vol).NotTo(BeNil(), "a seed must mount its node-key Secret")
	g.Expect(vol.Secret).NotTo(BeNil())
	g.Expect(vol.Secret.SecretName).To(Equal("seed-identity-node-key"))

	seid := containerNamed(spec, "seid")
	g.Expect(seid).NotTo(BeNil())
	var mounted bool
	for _, m := range seid.VolumeMounts {
		if m.Name == "node-key" {
			mounted = true
			// subPath: kubelet does not refresh it, so a Secret edit cannot swap
			// the identity under a running seid.
			g.Expect(m.SubPath).To(Equal("node_key.json"))
			g.Expect(m.ReadOnly).To(BeTrue())
		}
	}
	g.Expect(mounted).To(BeTrue(), "seid must mount the node key")
}

// A seed inheriting the RPC-class pool costs an order of magnitude more than a
// seed is worth, and karpenter.sh/do-not-disrupt then pins that node.
func TestSeed_SchedulesOnTheSeedNodepool(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	cfg := platformtest.Config()

	sts := seedStatefulSet(t, ns, "seed-pool")
	spec := sts.Spec.Template.Spec

	terms := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	g.Expect(terms[0].MatchExpressions[0].Values).To(ConsistOf(cfg.NodepoolSeed))
	g.Expect(spec.Tolerations[0].Value).To(Equal(cfg.NodepoolSeed))
	g.Expect(cfg.NodepoolSeed).NotTo(Equal(cfg.NodepoolName),
		"the fixture must distinguish the seed pool for this assertion to mean anything")
}

// A seed stores no chain state, so it takes the genesis progression: nothing to
// restore, and no rpc-server witnesses to gate on.
func TestSeed_TakesGenesisProgressionAndIsNotStateSyncGated(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := newTestSeedNode(ns, "seed-plan", "seed-plan-node-key")
	g.Expect(testCli.Create(testCtx, node)).To(Succeed())
	key := client.ObjectKeyFromObject(node)

	waitFor(t, func() bool {
		cur := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, key, cur); err != nil {
			return false
		}
		return cur.Status.Plan != nil && len(cur.Status.Plan.Tasks) > 0
	}, "a seed must get a plan")

	cur := &seiv1alpha1.SeiNode{}
	g.Expect(testCli.Get(testCtx, key, cur)).To(Succeed())

	var types []string
	for _, task := range cur.Status.Plan.Tasks {
		types = append(types, task.Type)
	}
	g.Expect(types).To(ContainElement("validate-node-key"),
		"the identity Secret is pre-flighted before the StatefulSet")
	g.Expect(types).To(ContainElement("configure-genesis"))
	g.Expect(types).NotTo(ContainElement("snapshot-restore"))
	g.Expect(types).NotTo(ContainElement("configure-state-sync"))

	// StateSyncReady is always-present; a seed resolves it NotApplicable rather
	// than leaving it absent.
	var found bool
	for _, c := range cur.Status.Conditions {
		if c.Type == seiv1alpha1.ConditionStateSyncReady {
			found = true
			g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(c.Reason).To(Equal(seiv1alpha1.ReasonStateSyncNotApplicable))
		}
	}
	g.Expect(found).To(BeTrue(), "StateSyncReady must be present on a seed")
}
