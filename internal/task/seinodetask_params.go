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

// DiscoverPeerSources translates a SeiNode's spec.peers into sidecar peer
// sources. Shared by the planner's init path and the SeiNodeTask DiscoverPeers
// constructor. A label source resolves to the controller-composed
// `<node_id>@<host>:<port>` entries already in status.resolvedPeers, routed as
// static so the sidecar writes them verbatim. An unresolved label (empty
// resolvedPeers) is skipped, not appended as a zero-address static — that would
// submit a discover-peers that silently wipes persistent-peers.
func DiscoverPeerSources(node *seiv1alpha1.SeiNode) []sidecar.PeerSource {
	var sources []sidecar.PeerSource
	for _, s := range node.Spec.Peers {
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
			if len(node.Status.ResolvedPeers) == 0 {
				continue
			}
			sources = append(sources, sidecar.PeerSource{
				Type:      sidecar.PeerSourceStatic,
				Addresses: node.Status.ResolvedPeers,
			})
		}
	}
	return sources
}

// ErrUnsupportedKind sentinels an unwired SeiNodeTask kind: one the CRD enum
// admits but this build does not dispatch. The synthesis site routes it to
// reason=UnsupportedKind (a public enum, CLAUDE.md); every other param-build
// failure is ParamsBuildFailed.
var ErrUnsupportedKind = errors.New("unsupported kind")

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
		return SeiNodeTaskParams{}, fmt.Errorf("%w: kind %q is not wired in this build", ErrUnsupportedKind, cr.Spec.Kind)
	}
}

func updateNodeImageParams(cr *seiv1alpha1.SeiNodeTask) (SeiNodeTaskParams, error) {
	if cr.Spec.UpdateNodeImage == nil {
		return SeiNodeTaskParams{}, errors.New("spec.updateNodeImage is required for kind=UpdateNodeImage")
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
		return SeiNodeTaskParams{}, errors.New("spec.govVote is required for kind=GovVote")
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
		return SeiNodeTaskParams{}, errors.New("spec.govSoftwareUpgrade is required for kind=GovSoftwareUpgrade")
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
		return SeiNodeTaskParams{}, errors.New("spec.awaitCondition is required for kind=AwaitCondition")
	}
	if p.Height == nil {
		return SeiNodeTaskParams{}, errors.New("spec.awaitCondition.height is required (height is the only condition wired in MVP)")
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
		return SeiNodeTaskParams{}, errors.New("spec.awaitNodesAtHeight is required for kind=AwaitNodesAtHeight")
	}
	return SeiNodeTaskParams{sidecar.TaskTypeAwaitCondition, sidecar.AwaitConditionTask{
		Condition:    sidecar.ConditionHeight,
		TargetHeight: p.TargetHeight,
	}}, nil
}

func discoverPeersParams(cr *seiv1alpha1.SeiNodeTask, target *seiv1alpha1.SeiNode) (SeiNodeTaskParams, error) {
	if cr.Spec.DiscoverPeers == nil {
		return SeiNodeTaskParams{}, errors.New("spec.discoverPeers is required for kind=DiscoverPeers")
	}
	// target is nil on the early-validation path; defer source-building (and the
	// empty-peers check) to the driveTask call site, which has the target.
	if target == nil {
		return SeiNodeTaskParams{Type: sidecar.TaskTypeDiscoverPeers}, nil
	}
	sources := DiscoverPeerSources(target)
	if len(sources) == 0 {
		return SeiNodeTaskParams{}, fmt.Errorf("target SeiNode %q has no spec.peers to discover", cr.Spec.Target.NodeRef.Name)
	}
	return SeiNodeTaskParams{sidecar.TaskTypeDiscoverPeers, sidecar.DiscoverPeersTask{Sources: sources}}, nil
}

func restartPodParams(cr *seiv1alpha1.SeiNodeTask) (SeiNodeTaskParams, error) {
	if cr.Spec.RestartPod == nil {
		return SeiNodeTaskParams{}, errors.New("spec.restartPod is required for kind=RestartPod")
	}
	// RestartedPodUID is content-addressed: captured once at synthesis and
	// persisted on status.task. Empty is valid on the early-validation path
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
