//go:build envtest

package envtest_test

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	nodecontroller "github.com/sei-protocol/sei-k8s-controller/internal/controller/node"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest"
)

// Direct reconciles make both error exits deterministic; all node writes and
// assertions use the real API, including admission of the nested null.
func TestUpdatePlanRejectionPersistsAccurateCondition(t *testing.T) {
	g := NewWithT(t)
	workflowCache, err := cache.New(testCfg, cache.Options{Scheme: testCli.Scheme()})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(workflowCache.IndexField(testCtx, &seiv1alpha1.SeiNodeTaskWorkflow{}, "spec.target.nodeRef.name", func(obj client.Object) []string {
		return []string{obj.(*seiv1alpha1.SeiNodeTaskWorkflow).Spec.Target.NodeRef.Name}
	})).To(Succeed())
	ctx, cancel := context.WithCancel(testCtx)
	t.Cleanup(cancel)
	go func() { _ = workflowCache.Start(ctx) }()
	g.Expect(workflowCache.WaitForCacheSync(ctx)).To(BeTrue())
	cli := &updateFailureClient{Client: testCli, workflows: workflowCache}
	for _, trigger := range []string{"image", "config-only", "config-only-stale"} {
		t.Run(trigger, func(t *testing.T) {
			g := NewWithT(t)
			node := lifecycleNode("plan-rejection-" + trigger)
			g.Expect(testCli.Create(testCtx, node)).To(Succeed())
			node.Status.Phase = seiv1alpha1.PhaseRunning
			node.Status.CurrentImage = node.Spec.Image
			node.Status.CurrentConfigValuesHash = "previous"
			if trigger == "config-only-stale" {
				meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
					Type: seiv1alpha1.ConditionNodeUpdateInProgress, Status: metav1.ConditionTrue,
					Reason: "UpdateStarted", Message: "image drift detected", ObservedGeneration: node.Generation,
				})
			}
			g.Expect(testCli.Status().Update(testCtx, node)).To(Succeed())
			if trigger == "image" {
				node.Spec.Image = "ghcr.io/sei-protocol/seid:next"
			}
			node.Spec.ConfigValues = []seiv1alpha1.ConfigValue{{
				FileName: "config.toml", Key: "custom", Value: apiextensionsv1.JSON{Raw: []byte(`{"nested":null}`)},
			}}
			g.Expect(testCli.Update(testCtx, node)).To(Succeed(), "schema must admit the image bump and nested null together")
			reconciler := &nodecontroller.SeiNodeReconciler{
				Client: cli, Scheme: testCli.Scheme(), Recorder: record.NewFakeRecorder(20),
				Platform: platformtest.Config(), Planner: &planner.NodeResolver{Platform: platformtest.Config()},
			}
			for reconcile := 1; reconcile <= 2; reconcile++ {
				_, err := reconciler.Reconcile(testCtx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(node)})
				g.Expect(err).To(MatchError(ContainSubstring("configValues config.toml:custom: null values are not supported")))
				observed := &seiv1alpha1.SeiNode{}
				g.Expect(testCli.Get(testCtx, client.ObjectKeyFromObject(node), observed)).To(Succeed())
				g.Expect(observed.Status.Plan).To(BeNil())
				condition := meta.FindStatusCondition(observed.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
				g.Expect(condition).NotTo(BeNil())
				g.Expect(condition.Status).To(Equal(metav1.ConditionFalse), "reconcile %d must not persist True with no plan", reconcile)
				g.Expect(condition.Reason).To(Equal("UpdatePlanBuildFailed"))
				g.Expect(condition.Message).To(ContainSubstring("configValues config.toml:custom: null values are not supported"))
				g.Expect(condition.Message).NotTo(ContainSubstring("image drift"))
				g.Expect(condition.ObservedGeneration).To(Equal(observed.Generation))
			}
		})
	}
}

// Workflow selectors are controller cache indexes, not API field selectors.
// Node reads/writes remain direct to avoid eventual consistency in assertions.
type updateFailureClient struct {
	client.Client
	workflows cache.Cache
}

func (c *updateFailureClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*seiv1alpha1.SeiNodeTaskWorkflowList); ok {
		return c.workflows.List(ctx, list, opts...)
	}
	return c.Client.List(ctx, list, opts...)
}
