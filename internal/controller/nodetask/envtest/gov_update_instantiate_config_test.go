//go:build envtest

package envtest_test

import (
	"testing"

	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func validGovUpdateInstantiateConfig() *seiv1alpha1.GovUpdateInstantiateConfigPayload {
	return &seiv1alpha1.GovUpdateInstantiateConfigPayload{
		ChainID:     "arctic-1",
		Title:       "Disable CosmWasm Contract Instantiation",
		Description: "Set existing code instantiate permissions to Nobody.",
		Updates: []seiv1alpha1.GovInstantiateConfigUpdate{
			{CodeID: 1, Permission: "nobody"},
		},
		InitialDeposit: "10000000usei",
		Fees:           "30000usei",
		Gas:            1_200_000,
	}
}

func TestCELGovUpdateInstantiateConfigAccepted(t *testing.T) {
	g := NewWithT(t)
	namespace := makeNamespace(t)
	task := baseTask(
		namespace,
		"gov-update-instantiate-ok",
		seiv1alpha1.SeiNodeTaskKindGovUpdateInstantiateConfig,
	)
	task.Spec.GovUpdateInstantiateConfig = validGovUpdateInstantiateConfig()
	g.Expect(testCli.Create(testCtx, task)).To(Succeed())
}

func TestCELGovUpdateInstantiateConfigNoPayloadRejected(t *testing.T) {
	g := NewWithT(t)
	namespace := makeNamespace(t)
	task := baseTask(
		namespace,
		"gov-update-instantiate-no-payload",
		seiv1alpha1.SeiNodeTaskKindGovUpdateInstantiateConfig,
	)
	err := testCli.Create(testCtx, task)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(Or(
		ContainSubstring("exactly one"),
		ContainSubstring("govUpdateInstantiateConfig is required"),
	))
}

func TestCELGovUpdateInstantiateConfigEmptyUpdatesRejected(t *testing.T) {
	g := NewWithT(t)
	namespace := makeNamespace(t)
	task := baseTask(
		namespace,
		"gov-update-instantiate-empty",
		seiv1alpha1.SeiNodeTaskKindGovUpdateInstantiateConfig,
	)
	payload := validGovUpdateInstantiateConfig()
	payload.Updates = nil
	task.Spec.GovUpdateInstantiateConfig = payload
	err := testCli.Create(testCtx, task)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("updates"))
}

func TestCELGovUpdateInstantiateConfigInvalidPermissionRejected(t *testing.T) {
	g := NewWithT(t)
	namespace := makeNamespace(t)
	task := baseTask(
		namespace,
		"gov-update-instantiate-invalid-permission",
		seiv1alpha1.SeiNodeTaskKindGovUpdateInstantiateConfig,
	)
	payload := validGovUpdateInstantiateConfig()
	payload.Updates[0].Permission = "somebody"
	task.Spec.GovUpdateInstantiateConfig = payload
	err := testCli.Create(testCtx, task)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("permission"))
}

func TestCELGovUpdateInstantiateConfigPayloadImmutable(t *testing.T) {
	g := NewWithT(t)
	namespace := makeNamespace(t)
	task := baseTask(
		namespace,
		"gov-update-instantiate-payload-immutable",
		seiv1alpha1.SeiNodeTaskKindGovUpdateInstantiateConfig,
	)
	task.Spec.GovUpdateInstantiateConfig = validGovUpdateInstantiateConfig()
	g.Expect(testCli.Create(testCtx, task)).To(Succeed())

	patch := client.MergeFrom(task.DeepCopy())
	task.Spec.GovUpdateInstantiateConfig.Updates[0].Permission = "everybody"
	err := testCli.Patch(testCtx, task, patch)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("govUpdateInstantiateConfig is immutable"))
}

func TestCELGovUpdateInstantiateConfigTargetImmutable(t *testing.T) {
	g := NewWithT(t)
	namespace := makeNamespace(t)
	task := baseTask(
		namespace,
		"gov-update-instantiate-target-immutable",
		seiv1alpha1.SeiNodeTaskKindGovUpdateInstantiateConfig,
	)
	task.Spec.GovUpdateInstantiateConfig = validGovUpdateInstantiateConfig()
	g.Expect(testCli.Create(testCtx, task)).To(Succeed())

	patch := client.MergeFrom(task.DeepCopy())
	task.Spec.Target.NodeRef.Name = "another-validator"
	err := testCli.Patch(testCtx, task, patch)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(
		"spec.target is immutable for kind=GovUpdateInstantiateConfig"))
}
