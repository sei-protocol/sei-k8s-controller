package planner

import (
	"context"
	"slices"
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const (
	staticConfigMapName = "rpc-config-v1"
	staticTestNamespace = "default"
	staticTestChainID   = "atlantic-2"
	staticTestImage     = "sei:v1.0.0"
)

func withNodeConfig(node *seiv1alpha1.SeiNode, name string) *seiv1alpha1.SeiNode {
	node.Spec.NodeConfig = &seiv1alpha1.NodeConfig{
		ConfigRef: seiv1alpha1.ConfigFileRef{Name: name},
		AppRef:    seiv1alpha1.ConfigFileRef{Name: name},
	}
	return node
}

// pendingNode is an un-provisioned node in the given mode, ready for an init plan.
func pendingNode(configure func(*seiv1alpha1.SeiNode)) *seiv1alpha1.SeiNode {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testNodeName, Namespace: staticTestNamespace, Generation: 1},
		Spec:       seiv1alpha1.SeiNodeSpec{ChainID: staticTestChainID, Image: staticTestImage},
		Status:     seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhasePending},
	}
	configure(node)
	return node
}

var staticModes = []struct {
	name      string
	configure func(*seiv1alpha1.SeiNode)
}{
	{"full", func(n *seiv1alpha1.SeiNode) { n.Spec.FullNode = &seiv1alpha1.FullNodeSpec{} }},
	{overlayTestArchive, func(n *seiv1alpha1.SeiNode) { n.Spec.Archive = &seiv1alpha1.ArchiveSpec{} }},
	{overlayTestValidator, func(n *seiv1alpha1.SeiNode) { n.Spec.Validator = &seiv1alpha1.ValidatorSpec{} }},
}

// TestStaticInitPlanCarriesNoConfigWriter is the assertion the whole feature
// rests on. A task that writes config.toml on the production pod renames it,
// and a rename onto a mounted path from another container detaches the mount.
func TestStaticInitPlanCarriesNoConfigWriter(t *testing.T) {
	for _, mode := range staticModes {
		t.Run(mode.name, func(t *testing.T) {
			g := NewWithT(t)
			node := withNodeConfig(pendingNode(mode.configure), staticConfigMapName)

			g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
			g.Expect(node.Status.Plan).NotTo(BeNil())

			types := planTaskTypes(node.Status.Plan)
			for _, writer := range mountedConfigWriters {
				g.Expect(types).NotTo(ContainElement(writer))
			}
			g.Expect(types).NotTo(ContainElement(TaskConfigValidate),
				"the controller does not validate a file the operator owns")
			g.Expect(types).To(ContainElement(TaskConfigureGenesis))
			g.Expect(types[len(types)-1]).To(Equal(TaskMarkReady))
		})
	}
}

// TestInitPlanKeepsConfigWriterWithoutConfigSource pins the other half: the
// filter is inert for a node the controller configures.
func TestInitPlanKeepsConfigWriterWithoutConfigSource(t *testing.T) {
	for _, mode := range staticModes {
		t.Run(mode.name, func(t *testing.T) {
			g := NewWithT(t)
			node := pendingNode(mode.configure)

			g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
			g.Expect(planTaskTypes(node.Status.Plan)).To(ContainElement(TaskConfigApply))
		})
	}
}

// TestStaticNodeConfigRefusesBootstrap closes the one silent path. The
// bootstrap Job pod carries no mount, but it holds the same data volume as the
// production pod, which reconcileStatefulSet creates unconditionally. A rename
// from the Job pod detaches the production pod's mount and seid then boots on
// the Job's generated config, with the plan reporting success.
func TestStaticNodeConfigRefusesBootstrap(t *testing.T) {
	g := NewWithT(t)
	node := withNodeConfig(pendingNode(func(n *seiv1alpha1.SeiNode) {
		n.Spec.FullNode = &seiv1alpha1.FullNodeSpec{
			Snapshot: &seiv1alpha1.SnapshotSource{
				BootstrapImage: staticTestImage,
				S3:             &seiv1alpha1.S3SnapshotSource{TargetHeight: 100},
			},
		}
	}), staticConfigMapName)
	g.Expect(NeedsBootstrap(node)).To(BeTrue(), "fixture must need a bootstrap Job")

	err := (&NodeResolver{}).ResolvePlan(context.Background(), node)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("bootstrap Job"))
	g.Expect(node.Status.Plan).To(BeNil())
}

// TestStaticRunningPlanRollsThePod covers every transition of the reference.
// Pod replacement is the only config-delivery mechanism: kubelet pins a subPath
// mount at pod start.
func TestStaticRunningPlanRollsThePod(t *testing.T) {
	cases := []struct {
		name      string
		spec      string
		observed  string
		wantRoll  bool
		wantInMsg string
	}{
		{"adopted by a running node", staticConfigMapName, "", true, staticConfigMapName},
		{"republished under a new name", "rpc-config-v2", staticConfigMapName, true, "rpc-config-v2"},
		{"cleared", "", staticConfigMapName, true, staticConfigMapName},
		{"unchanged", staticConfigMapName, staticConfigMapName, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			node := runningFullNode()
			if tc.spec != "" {
				withNodeConfig(node, tc.spec)
			}
			if tc.observed != "" {
				node.Status.CurrentNodeConfig = &seiv1alpha1.NodeConfig{
					ConfigRef: seiv1alpha1.ConfigFileRef{Name: tc.observed},
					AppRef:    seiv1alpha1.ConfigFileRef{Name: tc.observed},
				}
			}

			g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())

			if !tc.wantRoll {
				g.Expect(node.Status.Plan).To(BeNil())
				return
			}
			g.Expect(node.Status.Plan).NotTo(BeNil())
			types := planTaskTypes(node.Status.Plan)
			g.Expect(types).To(ContainElement(task.TaskTypeReplacePod))
			g.Expect(types).To(ContainElement(task.TaskTypeObserveImage))
			g.Expect(types).To(ContainElement(TaskMarkReady))

			if tc.spec == "" {
				// A revert restores the controller-managed base, after the
				// roll has replaced the pod with one that has no mount.
				g.Expect(slices.Index(types, TaskConfigApply)).To(
					BeNumerically(">", slices.Index(types, task.TaskTypeReplacePod)))
			} else {
				// Adopting or republishing carries no config task at all: the
				// writers detach the mount, and config-validate reports on a
				// file the operator owns.
				for _, writer := range mountedConfigWriters {
					g.Expect(types).NotTo(ContainElement(writer))
				}
				g.Expect(types).NotTo(ContainElement(TaskConfigValidate))
			}

			cond := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Message).To(ContainSubstring(tc.wantInMsg))
		})
	}
}

// TestStaticRunningPlanDoesNotLoop pins the stamp that ends the roll.
func TestStaticRunningPlanDoesNotLoop(t *testing.T) {
	g := NewWithT(t)
	node := withNodeConfig(runningFullNode(), staticConfigMapName)

	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	g.Expect(node.Status.Plan).NotTo(BeNil())

	node.Status.CurrentNodeConfig = node.Spec.NodeConfig.DeepCopy()
	node.Status.Plan = nil
	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	g.Expect(node.Status.Plan).To(BeNil())
}

// TestNodeConfigUnsetDoesNotRollOnControllerUpgrade is the invariant that
// replaces the unobserved short-circuit the other pod-template predicates
// carry. Every node in the fleet looks like this on the first reconcile after
// the controller ships.
func TestNodeConfigUnsetDoesNotRollOnControllerUpgrade(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()

	g.Expect(nodeConfigDrifted(node)).To(BeFalse())
	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	g.Expect(node.Status.Plan).To(BeNil())
}

// TestStaticValidateRunsTheModesOwnChecks pins the wrapping. Replacing the mode
// planner instead of wrapping it would drop every per-mode Validate.
func TestStaticValidateRunsTheModesOwnChecks(t *testing.T) {
	g := NewWithT(t)
	// A seed without a node-key Secret is refused by seedPlanner.Validate.
	node := withNodeConfig(pendingNode(func(n *seiv1alpha1.SeiNode) {
		n.Spec.Seed = &seiv1alpha1.SeedSpec{}
	}), staticConfigMapName)

	err := (&NodeResolver{}).ResolvePlan(context.Background(), node)
	g.Expect(err).To(HaveOccurred())
	g.Expect(node.Status.Plan).To(BeNil())
}

// TestStaticValidateRefusesRuntimeDiscoveredConfig covers the node shapes whose
// configuration seid can only learn while running. A ConfigMap written
// beforehand cannot hold those values, and the task that would write them is
// the one this planner removes.
func TestStaticValidateRefusesRuntimeDiscoveredConfig(t *testing.T) {
	cases := []struct {
		name      string
		configure func(*seiv1alpha1.SeiNode)
		wantErr   string
	}{
		{"genesis ceremony", func(n *seiv1alpha1.SeiNode) {
			n.Spec.Validator = &seiv1alpha1.ValidatorSpec{
				GenesisCeremony: &seiv1alpha1.GenesisCeremonyNodeConfig{
					ChainID:        staticTestChainID,
					StakingAmount:  testAccountBalance,
					AccountBalance: "2000000usei",
				},
			}
		}, "genesis-ceremony"},
		{"state-sync snapshot source", func(n *seiv1alpha1.SeiNode) {
			n.Spec.FullNode = &seiv1alpha1.FullNodeSpec{
				Snapshot: &seiv1alpha1.SnapshotSource{StateSync: &seiv1alpha1.StateSyncSource{}},
			}
		}, overlayTestStateSync},
		{"autobahn consensus", func(n *seiv1alpha1.SeiNode) {
			n.Spec.FullNode = &seiv1alpha1.FullNodeSpec{}
			n.Spec.Consensus = &seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineAutobahn}
		}, "Autobahn"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			node := withNodeConfig(pendingNode(tc.configure), staticConfigMapName)

			err := (&NodeResolver{}).ResolvePlan(context.Background(), node)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.wantErr))
			g.Expect(node.Status.Plan).To(BeNil())
		})
	}
}

// TestStaticWorkflowRefused pins the planner half of the lockstep pair with
// SeiNodeReconciler's adoption-time refusal. Every recipe writes config.toml.
func TestStaticWorkflowRefused(t *testing.T) {
	g := NewWithT(t)
	node := withNodeConfig(runningFullNode(), staticConfigMapName)
	wf := &seiv1alpha1.SeiNodeTaskWorkflow{
		Spec: seiv1alpha1.SeiNodeTaskWorkflowSpec{
			StateSync: &seiv1alpha1.StateSyncWorkflow{},
		},
	}

	err := (&stateSyncWorkflowPlanner{}).Validate(node, wf)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("static-config"))
}

// TestMountedConfigWriterInPlan covers the single gate that holds the
// invariant. The progression filter removes the writers and
// staticConfigPlanner owns the Running arms; this checks the finished plan
// regardless of which builder produced it.
func TestMountedConfigWriterInPlan(t *testing.T) {
	plan := func(types ...string) *seiv1alpha1.TaskPlan {
		tasks := make([]seiv1alpha1.PlannedTask, len(types))
		for i, tt := range types {
			tasks[i] = seiv1alpha1.PlannedTask{Type: tt}
		}
		return &seiv1alpha1.TaskPlan{Tasks: tasks}
	}

	cases := []struct {
		name string
		plan *seiv1alpha1.TaskPlan
		want string
	}{
		{"clean plan", plan(TaskConfigureGenesis, TaskConfigValidate, TaskMarkReady), ""},
		{"config-apply", plan(TaskConfigureGenesis, TaskConfigApply, TaskMarkReady), TaskConfigApply},
		{"config-patch", plan(task.TaskTypeApplyStatefulSet, TaskConfigPatch), TaskConfigPatch},
		{"inside a bootstrap window is NOT exempt", plan(
			task.TaskTypeDeployBootstrapJob, TaskConfigApply,
			task.TaskTypeTeardownBootstrap, TaskMarkReady), TaskConfigApply},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			node := withNodeConfig(runningFullNode(), staticConfigMapName)
			g.Expect(mountedConfigWriterInPlan(node, tc.plan)).To(Equal(tc.want))
		})
	}

	t.Run("inert for a node that mounts nothing", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(mountedConfigWriterInPlan(runningFullNode(), plan(TaskConfigApply))).To(BeEmpty())
	})

	// A node mid-revert still has the mount, so the stamp gates it too.
	t.Run("keys on the stamp as well as the spec", func(t *testing.T) {
		g := NewWithT(t)
		node := runningFullNode()
		node.Status.CurrentNodeConfig = &seiv1alpha1.NodeConfig{
			ConfigRef: seiv1alpha1.ConfigFileRef{Name: staticConfigMapName},
			AppRef:    seiv1alpha1.ConfigFileRef{Name: staticConfigMapName},
		}
		g.Expect(MountsNodeConfig(node)).To(BeTrue())
		g.Expect(mountedConfigWriterInPlan(node, plan(TaskConfigPatch))).To(Equal(TaskConfigPatch))
	})
}

// TestNodeConfigRevertRollsBeforeAnyConfigWrite covers the revert. A node
// whose spec no longer names ConfigMaps still has a pod that mounts them, and
// the mode planner's own update plan submits config-patch before it replaces
// the pod — a rename in the sidecar's own mount namespace, which fails EBUSY
// and rebuilds the same plan every reconcile. The stamp keeps the node on the
// static planner until the roll has actually dropped the mount.
func TestNodeConfigRevertRollsBeforeAnyConfigWrite(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Status.CurrentNodeConfig = &seiv1alpha1.NodeConfig{
		ConfigRef: seiv1alpha1.ConfigFileRef{Name: staticConfigMapName},
		AppRef:    seiv1alpha1.ConfigFileRef{Name: staticConfigMapName},
	}

	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	g.Expect(node.Status.Plan).NotTo(BeNil())

	types := planTaskTypes(node.Status.Plan)
	replace := slices.Index(types, task.TaskTypeReplacePod)
	g.Expect(replace).To(BeNumerically(">=", 0))

	// Nothing writes config while the outgoing pod still has the mount.
	for _, writer := range mountedConfigWriters {
		g.Expect(types[:replace]).NotTo(ContainElement(writer))
	}
	// The replacement pod has none, so the base the controller never wrote is
	// written there. A node created with nodeConfig otherwise keeps the files
	// `seid init` left on the volume.
	g.Expect(slices.Index(types, TaskConfigApply)).To(BeNumerically(">", replace))
	g.Expect(types[len(types)-1]).To(Equal(TaskMarkReady))

	// Once the roll drops the mount, the node returns to the mode planner and
	// settles: no drift, no plan.
	node.Status.CurrentNodeConfig = nil
	node.Status.Plan = nil
	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	g.Expect(node.Status.Plan).To(BeNil())
}
