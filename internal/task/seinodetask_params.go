package task

import (
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/types"

	sidecar "github.com/sei-protocol/seictl/sidecar/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// SeiNodeTaskParams holds a synthesized task type and its payload. A nil
// Payload is valid on the DiscoverPeers early-validation path (target not yet
// fetched); the caller marshals Payload itself.
type SeiNodeTaskParams struct {
	Type    string
	Payload any
}

// Stable SeiNodeTask Failed-condition reasons for param-build errors. These are
// a public enum (CLAUDE.md) consumed by runbooks/alerting — keep the strings
// stable.
const (
	ReasonParamsBuildFailed = "ParamsBuildFailed"
	ReasonUnsupportedKind   = "UnsupportedKind"
)

// ReasonedError is an error that carries the stable SeiNodeTask condition
// reason it should surface as. Every error SeiNodeTaskParamsFor returns
// implements it, so the synthesis/driveTask call sites map err→reason via
// FailureReason without type-switching on individual error values.
type ReasonedError interface {
	error
	Reason() string
}

// FailureReason returns the stable condition reason for a param-build error.
// It unwraps to a ReasonedError when present and otherwise defaults to
// ParamsBuildFailed — the catch-all for any failure that did not carry its own
// reason (e.g. a marshal error wrapped by the caller).
func FailureReason(err error) string {
	var re ReasonedError
	if errors.As(err, &re) {
		return re.Reason()
	}
	return ReasonParamsBuildFailed
}

// ErrUnsupportedKind reports an unwired SeiNodeTask kind: one the CRD enum
// admits but this build does not dispatch. It carries the offending kind and
// reports reason=UnsupportedKind (a public enum, CLAUDE.md); every other
// param-build failure reports ParamsBuildFailed.
type ErrUnsupportedKind struct {
	Kind seiv1alpha1.SeiNodeTaskKind
}

func (e *ErrUnsupportedKind) Error() string {
	return fmt.Sprintf("unsupported kind %q is not wired in this build", e.Kind)
}

func (e *ErrUnsupportedKind) Reason() string { return ReasonUnsupportedKind }

// paramsError is a param-build failure for a wired kind (missing payload,
// empty peer sources, missing pod UID, etc.). It reports reason=ParamsBuildFailed
// so FailureReason routes it without a call-site conditional.
type paramsError struct{ msg string }

func (e *paramsError) Error() string  { return e.msg }
func (e *paramsError) Reason() string { return ReasonParamsBuildFailed }

// paramsErrorf builds a ParamsBuildFailed-reasoned error.
func paramsErrorf(format string, args ...any) error {
	return &paramsError{msg: fmt.Sprintf(format, args...)}
}

// SeiNodeTaskParamsFor resolves a SeiNodeTask kind to its task type and
// payload. target is the resolved SeiNode; pass nil from the early-validation
// path (before the SeiNode is fetched) — KeyName/peer-source derivation that
// needs target is deferred to the driveTask call site.
func SeiNodeTaskParamsFor(cr *seiv1alpha1.SeiNodeTask, target *seiv1alpha1.SeiNode) (SeiNodeTaskParams, error) {
	switch cr.Spec.Kind {
	case seiv1alpha1.SeiNodeTaskKindUpdateNodeImage:
		return updateNodeImageParams(cr)
	case seiv1alpha1.SeiNodeTaskKindGovVote:
		return govVoteParams(cr, target)
	case seiv1alpha1.SeiNodeTaskKindGovSoftwareUpgrade:
		return govSoftwareUpgradeParams(cr, target)
	case seiv1alpha1.SeiNodeTaskKindAwaitCondition:
		return awaitConditionParams(cr)
	case seiv1alpha1.SeiNodeTaskKindAwaitNodesAtHeight:
		return awaitNodesAtHeightParams(cr)
	case seiv1alpha1.SeiNodeTaskKindDiscoverPeers:
		return discoverPeersParams(cr, target)
	case seiv1alpha1.SeiNodeTaskKindRestartPod:
		return restartPodParams(cr)
	default:
		return SeiNodeTaskParams{}, &ErrUnsupportedKind{Kind: cr.Spec.Kind}
	}
}

func updateNodeImageParams(cr *seiv1alpha1.SeiNodeTask) (SeiNodeTaskParams, error) {
	if cr.Spec.UpdateNodeImage == nil {
		return SeiNodeTaskParams{}, paramsErrorf("spec.updateNodeImage is required for kind=UpdateNodeImage")
	}
	return SeiNodeTaskParams{TaskTypeUpdateNodeImage, UpdateNodeImageParams{
		NodeName:  cr.Spec.Target.NodeRef.Name,
		Namespace: cr.Namespace,
		Image:     cr.Spec.UpdateNodeImage.Image,
	}}, nil
}

func govVoteParams(cr *seiv1alpha1.SeiNodeTask, target *seiv1alpha1.SeiNode) (SeiNodeTaskParams, error) {
	p := cr.Spec.GovVote
	if p == nil {
		return SeiNodeTaskParams{}, paramsErrorf("spec.govVote is required for kind=GovVote")
	}
	return SeiNodeTaskParams{sidecar.TaskTypeGovVote, sidecar.GovVoteTask{
		ChainID:    p.ChainID,
		KeyName:    resolveSigningUID(p.KeyName, target),
		ProposalID: p.ProposalID,
		Option:     p.Option,
		Memo:       p.Memo,
		Fees:       p.Fees,
		Gas:        p.Gas,
	}}, nil
}

func govSoftwareUpgradeParams(cr *seiv1alpha1.SeiNodeTask, target *seiv1alpha1.SeiNode) (SeiNodeTaskParams, error) {
	p := cr.Spec.GovSoftwareUpgrade
	if p == nil {
		return SeiNodeTaskParams{}, paramsErrorf("spec.govSoftwareUpgrade is required for kind=GovSoftwareUpgrade")
	}
	return SeiNodeTaskParams{sidecar.TaskTypeGovSoftwareUpgrade, sidecar.GovSoftwareUpgradeTask{
		ChainID:        p.ChainID,
		KeyName:        resolveSigningUID(p.KeyName, target),
		Title:          p.Title,
		Description:    p.Description,
		UpgradeName:    p.UpgradeName,
		UpgradeHeight:  p.UpgradeHeight,
		UpgradeInfo:    p.UpgradeInfo,
		InitialDeposit: p.InitialDeposit,
		Memo:           p.Memo,
		Fees:           p.Fees,
		Gas:            p.Gas,
	}}, nil
}

func awaitConditionParams(cr *seiv1alpha1.SeiNodeTask) (SeiNodeTaskParams, error) {
	p := cr.Spec.AwaitCondition
	if p == nil {
		return SeiNodeTaskParams{}, paramsErrorf("spec.awaitCondition is required for kind=AwaitCondition")
	}
	if p.Height == nil {
		return SeiNodeTaskParams{}, paramsErrorf("spec.awaitCondition.height is required (height is the only condition wired in MVP)")
	}
	return SeiNodeTaskParams{sidecar.TaskTypeAwaitCondition, sidecar.AwaitConditionTask{
		Condition:    sidecar.ConditionHeight,
		TargetHeight: p.Height.TargetHeight,
		Action:       p.Action,
	}}, nil
}

// awaitNodesAtHeightParams maps the single-node target to the sidecar's
// per-node await-condition(height=H) primitive rather than the
// deployment-scoped fan-out task, keeping status.task.id equal to the sidecar
// task ID.
func awaitNodesAtHeightParams(cr *seiv1alpha1.SeiNodeTask) (SeiNodeTaskParams, error) {
	p := cr.Spec.AwaitNodesAtHeight
	if p == nil {
		return SeiNodeTaskParams{}, paramsErrorf("spec.awaitNodesAtHeight is required for kind=AwaitNodesAtHeight")
	}
	return SeiNodeTaskParams{sidecar.TaskTypeAwaitCondition, sidecar.AwaitConditionTask{
		Condition:    sidecar.ConditionHeight,
		TargetHeight: p.TargetHeight,
	}}, nil
}

// discoverPeersParams builds the sidecar discover-peers payload from the
// target's spec.peers (+ status.resolvedPeers for label sources). It owns its
// source-building independently of the planner's init path: the two have
// different needs (the planner freezes plan params; the nodetask is live and
// fail-fast) and the slight duplication is preferred over a shared interface.
//
// A label source resolves to the controller-composed `<node_id>@<host>:<port>`
// entries already in status.resolvedPeers, routed as static so the sidecar
// writes them verbatim. An unresolved label (empty resolvedPeers) is skipped —
// appending it as a zero-address static would submit a discover-peers that
// silently wipes persistent-peers. If no usable source survives (empty
// spec.peers, or only unresolved label sources), this fails fast rather than
// submitting a zero-source write.
func discoverPeersParams(cr *seiv1alpha1.SeiNodeTask, target *seiv1alpha1.SeiNode) (SeiNodeTaskParams, error) {
	if cr.Spec.DiscoverPeers == nil {
		return SeiNodeTaskParams{}, paramsErrorf("spec.discoverPeers is required for kind=DiscoverPeers")
	}
	// target is nil on the early-validation path; defer source-building (and the
	// empty-peers check) to the driveTask call site, which has the target.
	if target == nil {
		return SeiNodeTaskParams{Type: sidecar.TaskTypeDiscoverPeers}, nil
	}

	var sources []sidecar.PeerSource
	for _, s := range target.Spec.Peers {
		switch {
		case s.EC2Tags != nil:
			sources = append(sources, sidecar.PeerSource{
				Type:   sidecar.PeerSourceEC2Tags,
				Region: s.EC2Tags.Region,
				Tags:   s.EC2Tags.Tags,
			})
		case s.Static != nil:
			sources = append(sources, sidecar.PeerSource{
				Type:      sidecar.PeerSourceStatic,
				Addresses: s.Static.Addresses,
			})
		case s.Label != nil:
			if len(target.Status.ResolvedPeers) == 0 {
				continue
			}
			sources = append(sources, sidecar.PeerSource{
				Type:      sidecar.PeerSourceStatic,
				Addresses: target.Status.ResolvedPeers,
			})
		}
	}
	if len(sources) == 0 {
		return SeiNodeTaskParams{}, paramsErrorf("target SeiNode %q has no usable peer sources to discover "+
			"(empty spec.peers, or only label sources with unresolved status.resolvedPeers)", cr.Spec.Target.NodeRef.Name)
	}
	return SeiNodeTaskParams{sidecar.TaskTypeDiscoverPeers, sidecar.DiscoverPeersTask{Sources: sources}}, nil
}

func restartPodParams(cr *seiv1alpha1.SeiNodeTask) (SeiNodeTaskParams, error) {
	if cr.Spec.RestartPod == nil {
		return SeiNodeTaskParams{}, paramsErrorf("spec.restartPod is required for kind=RestartPod")
	}
	// The pod UID is caller-supplied verbatim (spec.restartPod.podUID) and CEL
	// requires it non-empty for kind=RestartPod; this is the defensive backstop.
	if cr.Spec.RestartPod.PodUID == "" {
		return SeiNodeTaskParams{}, paramsErrorf("spec.restartPod.podUID is required for kind=RestartPod")
	}
	// RestartedPodUID is content-addressed: copied from spec into status.task at
	// synthesis, then threaded here. Empty is valid on the early-validation path
	// (status.task nil) — the real value is threaded once status.task exists.
	var podUID types.UID
	if cr.Status.Task != nil {
		podUID = types.UID(cr.Status.Task.RestartedPodUID)
	}
	return SeiNodeTaskParams{TaskTypeRestartPod, RestartPodParams{
		NodeName:        cr.Spec.Target.NodeRef.Name,
		Namespace:       cr.Namespace,
		RestartedPodUID: podUID,
	}}, nil
}

// resolveSigningUID returns explicit when set; otherwise derives from target
// via ResolveOperatorKeyringUID. With nil target (early-validation path), the
// final marshal happens later from driveTask, which has target.
func resolveSigningUID(explicit string, target *seiv1alpha1.SeiNode) string {
	if explicit != "" {
		return explicit
	}
	if target == nil {
		return ""
	}
	return seiv1alpha1.ResolveOperatorKeyringUID(target)
}
