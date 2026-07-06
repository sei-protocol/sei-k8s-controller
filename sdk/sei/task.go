package sei

import (
	"context"
	"strings"
	"time"
)

// Task support. A SeiNodeTask is a one-shot, typed operation against a single
// SeiNode — submit a gov upgrade proposal, vote, wait for a height, swap the
// node image. A caller drives a major-upgrade or release flow by running these
// in statement order.
//
// Completed tasks surface typed Outputs: a gov proposal submission returns its
// proposal ID and tx hash, so a follow-on GovVote reads the ID off the upgrade
// task rather than querying the chain.
//
// SeiNodeTask is SeiNode-specific, so it belongs in the SDK. The surface mirrors
// CreateNetwork/CreateNode: a typed spec with SDK-native payloads (core stays
// apimachinery-free), translated to the CRD in provider/k8s.

// Task kinds the SDK exposes. A curated subset of the SeiNodeTask CRD kinds —
// the ones the upgrade/release harness suites drive. The remaining CRD kinds
// (GovParamChange, AwaitCondition, RestartSeid, MarkReady) are added here when a
// suite needs them; the value strings match the CRD enum exactly.
const (
	TaskGovSoftwareUpgrade = "GovSoftwareUpgrade"
	TaskGovVote            = "GovVote"
	TaskAwaitNodesAtHeight = "AwaitNodesAtHeight"
	TaskUpdateNodeImage    = "UpdateNodeImage"
)

// Vote options for GovVote.Option (gov v1beta1 VoteOption parse rules).
const (
	VoteYes        = "yes"
	VoteNo         = "no"
	VoteAbstain    = "abstain"
	VoteNoWithVeto = "no_with_veto"
)

// TaskSpec is the typed input to RunTask — one SeiNodeTask against one target
// SeiNode in the same namespace. Exactly one payload pointer must be set and it
// must match Kind; RunTask validates this before applying.
type TaskSpec struct {
	Name      string        // metadata.name
	Namespace string        // "" => client default (kubeconfig context / SA namespace)
	Node      string        // target SeiNode name, same namespace -> spec.target.nodeRef.name
	Kind      string        // -> spec.kind; one of the Task* consts
	Timeout   time.Duration // -> spec.timeoutSeconds (truncated to whole seconds); 0 leaves the CRD unbounded default

	// Labels are extra labels on the SeiNodeTask object — e.g. a caller's
	// run-id/GC selector so a sweep can reap a run's tasks (which are not children
	// of any provisioned resource and so are not cascade-deleted).
	Labels map[string]string

	// RequirePhase is the SeiNode phase the target must be in before the task
	// dispatches; "" leaves the CRD default (Running). The controller gates on
	// EXACT equality (phase == RequirePhase), not a floor — so set this only when
	// the target genuinely sits in a non-Running phase, never to "widen" the gate.
	// -> spec.target.requirePhase.
	RequirePhase string
	// RequirePhaseTimeout bounds the requirePhase wait; 0 leaves the CRD default
	// (5m). -> spec.target.requirePhaseTimeout.
	RequirePhaseTimeout time.Duration

	// Exactly one payload, matching Kind.
	GovSoftwareUpgrade *GovSoftwareUpgrade
	GovVote            *GovVote
	AwaitNodesAtHeight *AwaitNodesAtHeight
	UpdateNodeImage    *UpdateNodeImage
}

// GovSoftwareUpgrade is the payload for TaskGovSoftwareUpgrade — submit an
// on-chain software-upgrade proposal from the target validator. The minted
// proposal ID is NOT returned as a task output (chain-as-medium — see the
// package doc); a harness reads it from the chain for the follow-on GovVote.
//
// Two on-chain timing invariants the SDK cannot enforce but a caller must honor:
// InitialDeposit must clear the chain's min_deposit or the proposal sits in the
// deposit period and never enters voting; and the proposal must PASS (voting
// period elapsed, ≥2/3 yes, quorum) strictly before UpgradeHeight, or the plan
// is scheduled for a height already passed and silently no-ops. On a short-
// voting-period test chain both are easy to satisfy; size UpgradeHeight
// accordingly. Fees/deposit are usei-only (Sei gov rejects other denoms; the
// sidecar pre-validates).
type GovSoftwareUpgrade struct {
	ChainID        string // chain ID the proposal targets
	KeyName        string // signing keyring entry; "" lets the controller derive from the target
	Title          string
	Description    string
	UpgradeName    string // must match an upgrade handler registered in the target binary
	UpgradeHeight  int64  // block height the upgrade is scheduled at; >= 1; must be after the proposal passes
	UpgradeInfo    string // optional Plan.Info (e.g. binary checksums)
	InitialDeposit string // coin notation, usei-only (e.g. "10000000usei"); must clear min_deposit
	Memo           string // optional; do NOT pre-tag (the sidecar appends taskID=<id>)
	Fees           string // coin notation, usei-only (e.g. "2000usei")
	Gas            uint64 // tx gas limit; >= 1
}

// GovVote is the payload for TaskGovVote — cast one validator's vote on a
// proposal. Run one per validator to clear the voting threshold.
type GovVote struct {
	ChainID    string // chain ID the vote targets
	KeyName    string // signing keyring entry; "" lets the controller derive from the target
	ProposalID uint64 // the proposal to vote on; >= 1
	Option     string // one of the Vote* consts
	Memo       string // optional; do NOT pre-tag
	Fees       string // coin notation, usei-only
	Gas        uint64 // tx gas limit; >= 1
}

// AwaitNodesAtHeight is the payload for TaskAwaitNodesAtHeight — block until the
// target SeiNode's local height reaches TargetHeight. Used to wait out the
// upgrade height before swapping binaries.
//
// Boundary care at an upgrade halt: a chain that halts AT UpgradeHeight commits
// up to UpgradeHeight-1 and stops producing UpgradeHeight, so await
// UpgradeHeight-1 (awaiting UpgradeHeight may never clear and WaitComplete is
// bounded only by the caller's ctx — it will hang to the deadline).
type AwaitNodesAtHeight struct {
	TargetHeight int64 // height the target must reach; >= 1
}

// UpdateNodeImage is the payload for TaskUpdateNodeImage — patch spec.image on
// the target SeiNode and complete when status.currentImage observes the new
// image. No readiness check: a major upgrade expects transient CrashLoop while
// the new binary applies the upgrade.
//
// Leave TaskSpec.RequirePhase at its default: a SeiNode that has reached Running
// stays Running across a transient halt (the controller does not demote phase on
// a non-Running blip), so a node halted at an upgrade height still reports
// Running — the default gate matches. Do NOT set RequirePhase to a non-Running
// value here; the gate is exact-equality and would never match, failing the task
// at RequirePhaseTimeout.
type UpdateNodeImage struct {
	Image string // desired seid image (with tag/digest)
}

// TaskOutputs carries a completed task's typed results. A nil sub-field means
// that kind produced no typed output.
type TaskOutputs struct {
	UpdateNodeImage    *UpdateNodeImageOutputs
	GovSoftwareUpgrade *GovProposalOutputs
	GovParamChange     *GovProposalOutputs
	GovVote            *GovVoteOutputs
}

// UpdateNodeImageOutputs are the results of an UpdateNodeImage task.
type UpdateNodeImageOutputs struct {
	AppliedImage string // image now observed on target.status.currentImage
}

// GovProposalOutputs are the results of a gov proposal submission
// (software-upgrade or param-change).
type GovProposalOutputs struct {
	TxHash     string
	Height     int64
	ProposalID uint64
}

// GovVoteOutputs are the results of a gov vote (no proposal is minted).
type GovVoteOutputs struct {
	TxHash string
	Height int64
}

// RunTask creates a SeiNodeTask and returns a handle immediately — it does NOT
// wait for completion. Call Task.WaitComplete to block on the result.
//
// Tasks are apply-once by name. The controller mints the execution on the first
// reconcile and never re-reads spec, so re-running RunTask with the same Name is
// a no-op (no double tx) — but a payload change on a same-name re-apply is
// silently ignored (the original execution stands), and a Delete followed by a
// RunTask with the same Name mints a NEW execution that resubmits the tx. For a
// gov proposal that already landed on-chain, that means a duplicate proposal:
// delete+recreate only against a disposable chain.
//
// The task and its target SeiNode (Node) resolve in the same namespace; set
// Namespace to the target node's namespace (cross-namespace targeting is out of
// scope).
func (c *Client) RunTask(ctx context.Context, spec TaskSpec) (*Task, error) {
	if err := validateTaskSpec(spec); err != nil {
		return nil, err
	}
	h, err := c.provider.RunTask(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &Task{handle: h}, nil
}

// GetTask reads an existing SeiNodeTask into a handle.
func (c *Client) GetTask(ctx context.Context, name, namespace string) (*Task, error) {
	h, err := c.provider.GetTask(ctx, name, namespace)
	if err != nil {
		return nil, err
	}
	return &Task{handle: h}, nil
}

// Task is a handle to a SeiNodeTask.
type Task struct{ handle TaskHandle }

// Name is the SeiNodeTask resource name.
func (t *Task) Name() string { return t.handle.Name() }

// Namespace is where the SeiNodeTask lives.
func (t *Task) Namespace() string { return t.handle.Namespace() }

// WaitComplete blocks until the task reaches Complete and returns its typed
// outputs, or returns an error if the task reaches Failed (carrying the task's
// failure reason) or the caller's ctx fires (IsTimeout on a deadline). The
// returned outputs carry exactly the sub-field matching the task's Kind.
func (t *Task) WaitComplete(ctx context.Context) (*TaskOutputs, error) {
	return t.handle.WaitComplete(ctx)
}

// Delete removes the SeiNodeTask. Caller-invoked; idempotent (a not-found is
// success). SeiNodeTasks are not auto-pruned, so a harness deletes them in
// cleanup the same way it deletes networks and nodes.
func (t *Task) Delete(ctx context.Context) error { return t.handle.Delete(ctx) }

// Object returns the mode-specific raw resource (k8s: *v1alpha1.SeiNodeTask).
func (t *Task) Object() any { return t.handle.Object() }

func validateTaskSpec(s TaskSpec) error {
	if strings.TrimSpace(s.Name) == "" {
		return usageErr("TaskSpec.Name is required")
	}
	if strings.TrimSpace(s.Node) == "" {
		return usageErr("TaskSpec.Node is required (the target SeiNode)")
	}
	// A sub-second non-zero Timeout truncates to 0 seconds, which the CRD reads as
	// unbounded — the opposite of the caller's intent. Reject it rather than
	// silently dropping the bound.
	if s.Timeout != 0 && s.Timeout < time.Second {
		return usageErr("TaskSpec.Timeout %s is below the 1s CRD resolution (0 means unbounded)", s.Timeout)
	}
	// Exactly one payload, and it must match Kind. Count set payloads so a
	// mismatched/missing/extra payload fails here, not at apply.
	set := 0
	if s.GovSoftwareUpgrade != nil {
		set++
	}
	if s.GovVote != nil {
		set++
	}
	if s.AwaitNodesAtHeight != nil {
		set++
	}
	if s.UpdateNodeImage != nil {
		set++
	}
	if set != 1 {
		return usageErr("TaskSpec requires exactly one payload set, got %d", set)
	}
	switch s.Kind {
	case TaskGovSoftwareUpgrade:
		if s.GovSoftwareUpgrade == nil {
			return usageErr("TaskSpec.Kind %q requires GovSoftwareUpgrade payload", s.Kind)
		}
	case TaskGovVote:
		if s.GovVote == nil {
			return usageErr("TaskSpec.Kind %q requires GovVote payload", s.Kind)
		}
		if !validVoteOption(s.GovVote.Option) {
			return usageErr("GovVote.Option %q is invalid (want one of yes/no/abstain/no_with_veto)", s.GovVote.Option)
		}
	case TaskAwaitNodesAtHeight:
		if s.AwaitNodesAtHeight == nil {
			return usageErr("TaskSpec.Kind %q requires AwaitNodesAtHeight payload", s.Kind)
		}
	case TaskUpdateNodeImage:
		if s.UpdateNodeImage == nil {
			return usageErr("TaskSpec.Kind %q requires UpdateNodeImage payload", s.Kind)
		}
	default:
		return usageErr("TaskSpec.Kind %q is not supported by the SDK", s.Kind)
	}
	return nil
}

// validVoteOption reports whether opt is an option the CRD's vote enum accepts
// (the two no_with_veto spellings the gov v1beta1 parser allows are both valid).
func validVoteOption(opt string) bool {
	switch opt {
	case VoteYes, VoteNo, VoteAbstain, VoteNoWithVeto, "no-with-veto":
		return true
	default:
		return false
	}
}
