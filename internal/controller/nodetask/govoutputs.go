package nodetask

import (
	"encoding/json"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

// govResult mirrors the sidecar's tasks.GovTxResult wire shape (decoded from
// the task-result channel) without importing the heavy sidecar tasks package.
type govResult struct {
	TxHash          string `json:"txHash"`
	Height          int64  `json:"height"`
	ProposalID      uint64 `json:"proposalId"`
	Code            uint32 `json:"code"`
	Codespace       string `json:"codespace"`
	RawLog          string `json:"rawLog"`
	InclusionStatus string `json:"inclusionStatus"`
}

// Inclusion outcomes — the sidecar's stable wire enum (seictl tasks.Inclusion*).
const (
	inclusionCommittedOK     = "committed_ok"
	inclusionCommittedFailed = "committed_failed"
	inclusionPending         = "pending"
)

// resulter is the optional accessor sidecarExecution implements to surface the
// handler's structured result.
type resulter interface{ Result() json.RawMessage }

// decodeGovResult extracts the gov result from a terminal execution, or nil if
// it carries none or doesn't parse.
func decodeGovResult(exec task.TaskExecution) *govResult {
	r, ok := exec.(resulter)
	if !ok {
		return nil
	}
	raw := r.Result()
	if len(raw) == 0 {
		return nil
	}
	var gr govResult
	if err := json.Unmarshal(raw, &gr); err != nil {
		return nil
	}
	return &gr
}

func isGovKind(k seiv1alpha1.SeiNodeTaskKind) bool {
	switch k {
	case seiv1alpha1.SeiNodeTaskKindGovVote,
		seiv1alpha1.SeiNodeTaskKindGovSoftwareUpgrade,
		seiv1alpha1.SeiNodeTaskKindGovParamChange:
		return true
	}
	return false
}

// populateGovOutputs maps a decoded gov result into the matching CRD Outputs
// sub-field. Called on both the confirmed and failed terminal paths so txHash
// (and proposalId, when known) are always surfaced.
func populateGovOutputs(cr *seiv1alpha1.SeiNodeTask, gr *govResult) {
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
	case seiv1alpha1.SeiNodeTaskKindGovVote:
		cr.Status.Outputs.GovVote = &seiv1alpha1.GovVoteOutputs{
			TxHash: gr.TxHash, Height: gr.Height,
		}
	}
}
