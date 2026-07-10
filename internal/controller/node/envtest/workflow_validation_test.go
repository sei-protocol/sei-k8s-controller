//go:build envtest

package envtest_test

import (
	"testing"

	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func newStateSyncWorkflow(name string) *seiv1alpha1.SeiNodeTaskWorkflow {
	return &seiv1alpha1.SeiNodeTaskWorkflow{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeTaskWorkflowSpec{
			Kind:   seiv1alpha1.SeiNodeTaskWorkflowKindStateSync,
			Target: seiv1alpha1.SeiNodeTaskTarget{NodeRef: seiv1alpha1.SeiNodeTaskNodeRef{Name: "rpc-0"}},
			StateSync: &seiv1alpha1.StateSyncWorkflow{
				ConfigPatch: map[string]map[string]apiextensionsv1.JSON{
					"app.toml": {"state-store.evm-ss-split": {Raw: []byte(`true`)}},
				},
			},
		},
	}
}

func TestWorkflowCRD_ValidCreate(t *testing.T) {
	g := NewWithT(t)
	wf := newStateSyncWorkflow("wf-valid")
	g.Expect(testCli.Create(testCtx, wf)).To(Succeed())
	t.Cleanup(func() { _ = testCli.Delete(testCtx, wf) })
}

func TestWorkflowCRD_RejectsMissingRecipe(t *testing.T) {
	g := NewWithT(t)
	wf := newStateSyncWorkflow("wf-norecipe")
	wf.Spec.StateSync = nil
	err := testCli.Create(testCtx, wf)
	g.Expect(err).To(HaveOccurred())
	g.Expect(apierrors.IsInvalid(err)).To(BeTrue())
}

func TestWorkflowCRD_KindImmutable(t *testing.T) {
	g := NewWithT(t)
	wf := newStateSyncWorkflow("wf-kind-immutable")
	g.Expect(testCli.Create(testCtx, wf)).To(Succeed())
	t.Cleanup(func() { _ = testCli.Delete(testCtx, wf) })

	patch := client.MergeFrom(wf.DeepCopy())
	wf.Spec.Kind = "Something"
	err := testCli.Patch(testCtx, wf, patch)
	g.Expect(err).To(HaveOccurred())
	g.Expect(apierrors.IsInvalid(err)).To(BeTrue())
}

func TestWorkflowCRD_TargetImmutable(t *testing.T) {
	g := NewWithT(t)
	wf := newStateSyncWorkflow("wf-target-immutable")
	g.Expect(testCli.Create(testCtx, wf)).To(Succeed())
	t.Cleanup(func() { _ = testCli.Delete(testCtx, wf) })

	patch := client.MergeFrom(wf.DeepCopy())
	wf.Spec.Target.NodeRef.Name = "rpc-1"
	err := testCli.Patch(testCtx, wf, patch)
	g.Expect(err).To(HaveOccurred())
	g.Expect(apierrors.IsInvalid(err)).To(BeTrue())
}

func TestWorkflowCRD_StateSyncLeafImmutable(t *testing.T) {
	g := NewWithT(t)
	wf := newStateSyncWorkflow("wf-statesync-immutable")
	g.Expect(testCli.Create(testCtx, wf)).To(Succeed())
	t.Cleanup(func() { _ = testCli.Delete(testCtx, wf) })

	// Mutating a leaf inside stateSync (the pre-adoption param-tamper attack) is
	// rejected by the deep self.stateSync == oldSelf.stateSync rule, even though
	// configPatch values are x-kubernetes-preserve-unknown-fields.
	patch := client.MergeFrom(wf.DeepCopy())
	wf.Spec.StateSync.RpcServers = []string{"tampered:26657", "other:26657"}
	err := testCli.Patch(testCtx, wf, patch)
	g.Expect(err).To(HaveOccurred())
	g.Expect(apierrors.IsInvalid(err)).To(BeTrue())

	patch = client.MergeFrom(wf.DeepCopy())
	wf.Spec.StateSync.ConfigPatch["app.toml"]["state-store.evm-ss-split"] = apiextensionsv1.JSON{Raw: []byte(`false`)}
	err = testCli.Patch(testCtx, wf, patch)
	g.Expect(err).To(HaveOccurred())
	g.Expect(apierrors.IsInvalid(err)).To(BeTrue())
}

func TestWorkflowCRD_RecipeArmImmutable(t *testing.T) {
	g := NewWithT(t)
	wf := newStateSyncWorkflow("wf-arm-immutable")
	g.Expect(testCli.Create(testCtx, wf)).To(Succeed())
	t.Cleanup(func() { _ = testCli.Delete(testCtx, wf) })

	// Removing the recipe arm is rejected (arm-presence immutability).
	patch := client.MergeFrom(wf.DeepCopy())
	wf.Spec.StateSync = nil
	err := testCli.Patch(testCtx, wf, patch)
	g.Expect(err).To(HaveOccurred())
	g.Expect(apierrors.IsInvalid(err)).To(BeTrue())
}
