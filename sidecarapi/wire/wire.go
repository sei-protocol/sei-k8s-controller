// Package wire holds the light, dependency-free contract types shared between
// the sidecar server (tasks/engine) and its clients (the generated client, the
// controller). It imports no sei-chain: importing any symbol here must never
// drag the chain graph into a light consumer. Server-side packages re-export
// these (e.g. engine.TaskType aliases wire.TaskType) so their call sites are
// unchanged.
package wire

import (
	"fmt"
	"strings"
)

// TaskType identifies a task on the wire (the request/result `type` field).
type TaskType string

const (
	TaskSnapshotRestore          TaskType = "snapshot-restore"
	TaskConfigPatch              TaskType = "config-patch"
	TaskConfigApply              TaskType = "config-apply"
	TaskConfigValidate           TaskType = "config-validate"
	TaskConfigReload             TaskType = "config-reload"
	TaskMarkReady                TaskType = "mark-ready"
	TaskRestartSeid              TaskType = "restart-seid"
	TaskConfigureGenesis         TaskType = "configure-genesis"
	TaskConfigureStateSync       TaskType = "configure-state-sync"
	TaskSnapshotUpload           TaskType = "snapshot-upload"
	TaskSnapshotUploadOnce       TaskType = "snapshot-upload-once"
	TaskResultExport             TaskType = "result-export"
	TaskAwaitCondition           TaskType = "await-condition"
	TaskGenerateIdentity         TaskType = "generate-identity"
	TaskGenerateGentx            TaskType = "generate-gentx"
	TaskUploadGenesisArtifacts   TaskType = "upload-genesis-artifacts"
	TaskAssembleAndUploadGenesis TaskType = "assemble-and-upload-genesis"
	TaskSetGenesisPeers          TaskType = "set-genesis-peers"
	TaskGovVote                  TaskType = "gov-vote"
	TaskGovSoftwareUpgrade       TaskType = "gov-software-upgrade"
	TaskGovParamChange           TaskType = "gov-param-change"
	TaskEvmLogicalDigest         TaskType = "evm-logical-digest"

	// Workflow node-hold tasks (SeiNodeTaskWorkflow StateSync recipe). These
	// three compose the durable seid hold: mark-not-ready re-arms the start
	// gate, stop-seid parks seid behind it, reset-data clears the chain data
	// directory. Wire strings are a published contract (one-way door).
	TaskMarkNotReady TaskType = "mark-not-ready"
	TaskStopSeid     TaskType = "stop-seid"
	TaskResetData    TaskType = "reset-data"
)

// VoteOption mirrors cosmos gov v1beta1 VoteOption values so callers can parse
// and validate a vote string without importing sei-chain. The values are the
// stable protobuf enum numbers (guarded by a test against govtypes).
type VoteOption int

const (
	OptionEmpty      VoteOption = 0
	OptionYes        VoteOption = 1
	OptionAbstain    VoteOption = 2
	OptionNo         VoteOption = 3
	OptionNoWithVeto VoteOption = 4
)

// ParseVoteOption is the single accepted-option set shared by client and server.
func ParseVoteOption(s string) (VoteOption, error) {
	switch strings.ToLower(s) {
	case "yes":
		return OptionYes, nil
	case "no":
		return OptionNo, nil
	case "abstain":
		return OptionAbstain, nil
	case "no_with_veto", "no-with-veto":
		return OptionNoWithVeto, nil
	default:
		return 0, fmt.Errorf("invalid vote option %q (allowed: yes | no | abstain | no_with_veto)", s)
	}
}

// Inclusion outcomes carried on GovTxResult.InclusionStatus — the stable enum
// the controller keys its condition/reason and re-submit decision on.
const (
	InclusionCommittedOK     = "committed_ok"
	InclusionCommittedFailed = "committed_failed"
	InclusionPending         = "pending"
	// InclusionUnverifiable: the tx was broadcast and accepted at CheckTx, but
	// its on-chain outcome cannot be observed from the target node (its tx
	// index is disabled), so the sidecar can confirm neither success nor
	// failure. Terminal — retrying the same node is futile — but distinct from
	// committed_failed: the operator must verify via an indexed RPC.
	InclusionUnverifiable = "unverifiable"
)

// GovTxResult is the structured result a gov sign-tx handler returns; the engine
// persists it on TaskResult.Result and the controller decodes it into the
// SeiNodeTask outputs. ProposalID is 0 for votes and not-yet-included submits.
type GovTxResult struct {
	TxHash          string `json:"txHash"`
	Height          int64  `json:"height,omitempty"`
	ProposalID      uint64 `json:"proposalId,omitempty"`
	Code            uint32 `json:"code,omitempty"`
	Codespace       string `json:"codespace,omitempty"`
	RawLog          string `json:"rawLog,omitempty"`
	InclusionStatus string `json:"inclusionStatus"`
}

// UploadOutcome is the terminal classification of a snapshot-upload-once,
// carried on the task result's outcome field. A one-shot poller keys its verdict
// on it, so the values are a result-wire contract shared by the sidecar handler
// and its CLI/controller consumers.
type UploadOutcome string

const (
	OutcomeUploaded UploadOutcome = "uploaded"
	OutcomeNoop     UploadOutcome = "noop"
	OutcomeError    UploadOutcome = "error"
)

// NoopReason explains why a snapshot-upload-once returned OutcomeNoop. Empty on
// any other outcome.
type NoopReason string

const (
	NoopFewerThanTwoSnapshots NoopReason = "fewer-than-2-snapshots"
	NoopAlreadyUploaded       NoopReason = "already-uploaded"
)

// UpCheckScheme names how a seid liveness signal is read.
type UpCheckScheme string

const (
	UpCheckTCP  UpCheckScheme = "tcp"
	UpCheckHTTP UpCheckScheme = "http"
)

// UpCheck is the signal that says "seid answers again" after an in-place
// restart. The controller derives it from the node's mode, so the sidecar and
// the pod's readiness probe read the same source: TCP on the P2P port for a
// seed, HTTP GET on the RPC port otherwise. Path is set for HTTP and empty for
// TCP. An absent UpCheck means the pre-mode default, HTTP GET /status on 26657.
type UpCheck struct {
	Scheme UpCheckScheme `json:"scheme"`
	Port   int32         `json:"port"`
	Path   string        `json:"path,omitempty"`
}

// Validate reports a malformed UpCheck. It is part of the wire contract: the
// controller rejects the task before submit and the sidecar rejects it on
// receipt with the same rule.
func (u UpCheck) Validate() error {
	if u.Port < 1 || u.Port > 65535 {
		return fmt.Errorf("upCheck: port %d is outside 1-65535", u.Port)
	}
	switch u.Scheme {
	case UpCheckTCP:
		if u.Path != "" {
			return fmt.Errorf("upCheck: path %q is meaningless for scheme tcp", u.Path)
		}
	case UpCheckHTTP:
		if !strings.HasPrefix(u.Path, "/") {
			return fmt.Errorf("upCheck: http path %q must start with /", u.Path)
		}
	default:
		return fmt.Errorf("upCheck: unknown scheme %q (want tcp or http)", u.Scheme)
	}
	return nil
}
