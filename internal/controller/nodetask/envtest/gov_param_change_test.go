//go:build envtest

package envtest_test

import (
	"testing"

	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// validGovParamChange returns a well-formed GovParamChange payload with the
// given raw-JSON value for its single change.
func validGovParamChange(value string) *seiv1alpha1.GovParamChangePayload {
	return &seiv1alpha1.GovParamChangePayload{
		ChainID:     "arctic-1",
		Title:       "Update Consensus Timeout Params",
		Description: "Tighten timeouts.",
		Changes: []seiv1alpha1.GovParamChangeEntry{
			{Subspace: "baseapp", Key: "TimeoutParams", Value: apiextensionsv1.JSON{Raw: []byte(value)}},
		},
		InitialDeposit: "10000000usei",
		Fees:           "8000usei",
		Gas:            300000,
	}
}

// kind=GovParamChange with its matching payload is accepted.
func TestCEL_GovParamChange_Accepted(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	snt := baseTask(ns, "govparam-ok", seiv1alpha1.SeiNodeTaskKindGovParamChange)
	snt.Spec.GovParamChange = validGovParamChange(`{"propose":"300000000","commit":"200000000"}`)
	g.Expect(testCli.Create(testCtx, snt)).To(Succeed())
}

// The value field accepts ANY JSON shape (object, string, number, bool) via
// x-kubernetes-preserve-unknown-fields — the apiserver must not reject a
// scalar or coerce the shape. This is the schema-layer half of the
// single-encode contract: the operator passes structured JSON, not a string.
func TestCEL_GovParamChange_ValueAcceptsAnyJSON(t *testing.T) {
	for _, value := range []string{
		`{"propose":"300000000"}`, // object (struct param)
		`"86400000000000"`,        // string (duration ns)
		`100`,                     // bare number
		`true`,                    // bool
	} {
		t.Run(value, func(t *testing.T) {
			g := NewWithT(t)
			ns := makeNamespace(t)
			snt := baseTask(ns, "govparam-val", seiv1alpha1.SeiNodeTaskKindGovParamChange)
			snt.Spec.GovParamChange = validGovParamChange(value)
			g.Expect(testCli.Create(testCtx, snt)).To(Succeed())

			// Round-trips byte-identical through the apiserver (no re-encode).
			got := &seiv1alpha1.SeiNodeTask{}
			g.Expect(testCli.Get(testCtx, client.ObjectKeyFromObject(snt), got)).To(Succeed())
			g.Expect(string(got.Spec.GovParamChange.Changes[0].Value.Raw)).To(Equal(value))
		})
	}
}

// kind=GovParamChange with NO payload is rejected (exactly-one / kind-required).
func TestCEL_GovParamChange_NoPayload_Rejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	snt := baseTask(ns, "govparam-nopayload", seiv1alpha1.SeiNodeTaskKindGovParamChange)
	err := testCli.Create(testCtx, snt)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(Or(
		ContainSubstring("exactly one"),
		ContainSubstring("govParamChange is required"),
	))
}

// A second payload alongside govParamChange is rejected by the exactly-one rule.
func TestCEL_GovParamChange_MultiplePayloads_Rejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	snt := baseTask(ns, "govparam-two", seiv1alpha1.SeiNodeTaskKindGovParamChange)
	snt.Spec.GovParamChange = validGovParamChange(`{"propose":"300000000"}`)
	snt.Spec.MarkReady = &seiv1alpha1.MarkReadyPayload{}
	err := testCli.Create(testCtx, snt)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("exactly one"))
}

// kind=GovParamChange carrying a mismatched payload (markReady) is rejected by
// the kind/payload agreement rule.
func TestCEL_GovParamChange_KindPayloadMismatch_Rejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	snt := baseTask(ns, "govparam-mismatch", seiv1alpha1.SeiNodeTaskKindGovParamChange)
	snt.Spec.MarkReady = &seiv1alpha1.MarkReadyPayload{}
	err := testCli.Create(testCtx, snt)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("govParamChange is required"))
}

// An empty changes list is rejected by MinItems=1.
func TestCEL_GovParamChange_EmptyChanges_Rejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	snt := baseTask(ns, "govparam-nochanges", seiv1alpha1.SeiNodeTaskKindGovParamChange)
	p := validGovParamChange(`{"propose":"300000000"}`)
	p.Changes = nil
	snt.Spec.GovParamChange = p
	err := testCli.Create(testCtx, snt)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("changes"))
}

// A missing required scalar (empty chainId) is rejected by MinLength=1.
func TestCEL_GovParamChange_MissingChainID_Rejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	snt := baseTask(ns, "govparam-nochainid", seiv1alpha1.SeiNodeTaskKindGovParamChange)
	p := validGovParamChange(`{"propose":"300000000"}`)
	p.ChainID = ""
	snt.Spec.GovParamChange = p
	err := testCli.Create(testCtx, snt)
	g.Expect(err).To(HaveOccurred())
}

// spec.kind is immutable — a GovParamChange task cannot be flipped to MarkReady.
func TestCEL_GovParamChange_KindImmutable(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	snt := baseTask(ns, "govparam-immutable", seiv1alpha1.SeiNodeTaskKindGovParamChange)
	snt.Spec.GovParamChange = validGovParamChange(`{"propose":"300000000"}`)
	g.Expect(testCli.Create(testCtx, snt)).To(Succeed())

	patch := client.MergeFrom(snt.DeepCopy())
	snt.Spec.Kind = seiv1alpha1.SeiNodeTaskKindMarkReady
	snt.Spec.GovParamChange = nil
	snt.Spec.MarkReady = &seiv1alpha1.MarkReadyPayload{}
	err := testCli.Patch(testCtx, snt, patch)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("kind is immutable"))
}
