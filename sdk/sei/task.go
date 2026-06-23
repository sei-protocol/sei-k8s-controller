package sei

import (
	"context"
	"strings"
	"time"
)

// Task support. A SeiNodeTask is a one-shot, typed operation against a single
// SeiNode — submit a gov upgrade proposal, vote, wait for a height, swap the
// node image. The harness drives a major-upgrade or release scenario by running
// these in statement order, threading one task's outputs into the next (the
// proposal ID a GovSoftwareUpgrade mints feeds the GovVote). This replaces the
// Chaos-Mesh Workflow DAG + env-file handoffs the seitask-runner used.
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

	// RequirePhase is the SeiNode phase the target must be in before the task
	// dispatches; "" leaves the CRD default (Running). An UpdateNodeImage on a
	// chain halted at an upgrade height must relax this — the target is not
	// Running. -> spec.target.requirePhase.
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
// proposal ID is returned in TaskOutputs.GovSoftwareUpgrade.ProposalID for the
// follow-on GovVote tasks. Fees/deposit are usei-only (Sei gov rejects other
// denoms; the sidecar pre-validates).
type GovSoftwareUpgrade struct {
	ChainID        string // chain ID the proposal targets
	KeyName        string // signing keyring entry; "" lets the controller derive from the target
	Title          string
	Description    string
	UpgradeName    string // must match an upgrade handler registered in the target binary
	UpgradeHeight  int64  // block height the upgrade is scheduled at; >= 1
	UpgradeInfo    string // optional Plan.Info (e.g. binary checksums)
	InitialDeposit string // coin notation, usei-only (e.g. "10000000usei")
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
// target SeiNode's local height crosses TargetHeight. Used to wait out the
// upgrade height (where the chain halts) before swapping binaries.
type AwaitNodesAtHeight struct {
	TargetHeight int64 // height the target must reach; >= 1
}

// UpdateNodeImage is the payload for TaskUpdateNodeImage — patch spec.image on
// the target SeiNode and complete when status.currentImage observes the new
// image. No readiness check: a major upgrade expects transient CrashLoop while
// the new binary applies the upgrade. Relax TaskSpec.RequirePhase accordingly,
// since the halted target is not Running.
type UpdateNodeImage struct {
	Image string // desired seid image (with tag/digest)
}

// TaskOutputs carries a completed task's typed results. Exactly one field is
// populated, matching the task's Kind; populated only on a completed task.
type TaskOutputs struct {
	GovSoftwareUpgrade *GovSoftwareUpgradeOutputs
	GovVote            *GovVoteOutputs
	AwaitNodesAtHeight *AwaitNodesAtHeightOutputs
	UpdateNodeImage    *UpdateNodeImageOutputs
}

// GovSoftwareUpgradeOutputs are the results of a GovSoftwareUpgrade task.
type GovSoftwareUpgradeOutputs struct {
	ProposalID uint64 // on-chain proposal ID parsed from the inclusion; the GovVote input
	TxHash     string // upper-case hex tx hash
	Height     int64  // inclusion height
}

// GovVoteOutputs are the results of a GovVote task.
type GovVoteOutputs struct {
	TxHash string
	Height int64
}

// AwaitNodesAtHeightOutputs are the results of an AwaitNodesAtHeight task.
type AwaitNodesAtHeightOutputs struct {
	CurrentHeight int64 // height observed on the target when the condition cleared
}

// UpdateNodeImageOutputs are the results of an UpdateNodeImage task.
type UpdateNodeImageOutputs struct {
	AppliedImage string // image now observed on target.status.currentImage
}

// RunTask creates a SeiNodeTask and returns a handle immediately — it does NOT
// wait for completion. Call Task.WaitComplete to block on the result.
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

// Task is a Go-native handle to a SeiNodeTask.
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
