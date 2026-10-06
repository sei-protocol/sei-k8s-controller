package nodetask

import (
	"encoding/json"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
	"github.com/sei-protocol/sei-k8s-controller/sidecarapi/wire"
)

// resulter is the optional accessor sidecarExecution implements to surface the
// handler's structured result.
type resulter interface{ Result() json.RawMessage }

// decodeGovResult extracts the sign-tx result from a terminal execution, or nil
// if it carries none or doesn't parse.
func decodeGovResult(exec task.TaskExecution) *wire.GovTxResult {
	r, ok := exec.(resulter)
	if !ok {
		return nil
	}
	raw := r.Result()
	if len(raw) == 0 {
		return nil
	}
	var gr wire.GovTxResult
	if err := json.Unmarshal(raw, &gr); err != nil {
		return nil
	}
	return &gr
}

// isSignTxKind reports whether a kind signs and broadcasts a tx through the
// sidecar keyring. Every such kind returns the shared GovTxResult completion
// contract: the gov kinds and Unjail.
func isSignTxKind(k seiv1alpha1.SeiNodeTaskKind) bool {
	switch k {
	case seiv1alpha1.SeiNodeTaskKindGovVote,
		seiv1alpha1.SeiNodeTaskKindGovSoftwareUpgrade,
		seiv1alpha1.SeiNodeTaskKindGovParamChange,
		seiv1alpha1.SeiNodeTaskKindGovUpdateInstantiateConfig,
		seiv1alpha1.SeiNodeTaskKindUnjail:
		return true
	}
	return false
}

// populateTxOutputs maps a decoded sign-tx result into the matching CRD
// Outputs sub-field. Called on both the confirmed and failed terminal paths so
// txHash (and proposalId, when known) are always surfaced.
func populateTxOutputs(cr *seiv1alpha1.SeiNodeTask, gr *wire.GovTxResult) {
	if gr == nil {
		return
	}
	if cr.Status.Outputs == nil {
		cr.Status.Outputs = &seiv1alpha1.SeiNodeTaskOutputs{}
	}
	switch cr.Spec.Kind {
	case seiv1alpha1.SeiNodeTaskKindGovSoftwareUpgrade:
		cr.Status.Outputs.GovSoftwareUpgrade = &seiv1alpha1.GovSoftwareUpgradeOutputs{
			TxHash: gr.TxHash, Height: gr.Height, ProposalID: gr.ProposalID,
		}
	case seiv1alpha1.SeiNodeTaskKindGovParamChange:
		cr.Status.Outputs.GovParamChange = &seiv1alpha1.GovParamChangeOutputs{
			TxHash: gr.TxHash, Height: gr.Height, ProposalID: gr.ProposalID,
		}
	case seiv1alpha1.SeiNodeTaskKindGovUpdateInstantiateConfig:
		cr.Status.Outputs.GovUpdateInstantiateConfig = &seiv1alpha1.GovUpdateInstantiateConfigOutputs{
			TxHash: gr.TxHash, Height: gr.Height, ProposalID: gr.ProposalID,
		}
	case seiv1alpha1.SeiNodeTaskKindGovVote:
		cr.Status.Outputs.GovVote = &seiv1alpha1.GovVoteOutputs{
			TxHash: gr.TxHash, Height: gr.Height,
		}
	case seiv1alpha1.SeiNodeTaskKindUnjail:
		cr.Status.Outputs.Unjail = &seiv1alpha1.UnjailOutputs{
			TxHash: gr.TxHash, Height: gr.Height,
		}
	}
}
