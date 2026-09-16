package nodetask

import (
	"testing"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/sidecarapi/wire"
)

func TestPopulateGovUpdateInstantiateConfigOutputs(t *testing.T) {
	cr := &seiv1alpha1.SeiNodeTask{
		Spec: seiv1alpha1.SeiNodeTaskSpec{
			Kind: seiv1alpha1.SeiNodeTaskKindGovUpdateInstantiateConfig,
		},
	}
	if !isGovKind(cr.Spec.Kind) {
		t.Fatal("GovUpdateInstantiateConfig must be classified as a gov kind")
	}

	populateGovOutputs(cr, &wire.GovTxResult{
		TxHash:     "ABC",
		Height:     123,
		ProposalID: 259,
	})
	if cr.Status.Outputs == nil || cr.Status.Outputs.GovUpdateInstantiateConfig == nil {
		t.Fatal("GovUpdateInstantiateConfig outputs not populated")
	}
	got := cr.Status.Outputs.GovUpdateInstantiateConfig
	if got.TxHash != "ABC" || got.Height != 123 || got.ProposalID != 259 {
		t.Errorf("outputs = %+v", got)
	}
}
